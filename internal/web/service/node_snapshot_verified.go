package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"gorm.io/gorm"
)

// NodeInboundVerificationRequired reports durable receiver registration. A
// read error must never be interpreted as permission to use the legacy path.
func NodeInboundVerificationRequired(nodeID int) (bool, error) {
	if nodeID <= 0 {
		return false, errors.New("invalid node")
	}
	var count int64
	if err := database.GetDB().Model(&model.TrafficSnapshotReceiver{}).Where("id = ?", fmt.Sprintf("node-inbound:%d", nodeID)).Count(&count).Error; err != nil {
		return false, err
	}
	return count != 0, nil
}

// SetVerifiedRemoteTraffic is called with the allocation lock held, just as
// SetRemoteTraffic is. Its sequence and accounting commit atomically. It does
// not prove runtime freshness or justify lifting reseller quota guards.
func (s *InboundService) SetVerifiedRemoteTraffic(nodeID int, envelope trafficprotocol.Envelope) (bool, error) {
	var changed bool
	err := submitTrafficWrite(func() error {
		if err := persistPendingNodeCounterIncident(database.GetDB(), nodeID); err != nil {
			return err
		}
		if err := diskCounterIncidentError(); err != nil {
			return err
		}
		var inner error
		changed, inner = s.acceptVerifiedNodeTrafficLocked(nodeID, envelope)
		var decrease *resellerNodeCounterDecrease
		if errors.As(inner, &decrease) {
			rememberNodeCounterDecrease(decrease)
			return persistPendingNodeCounterIncident(database.GetDB(), nodeID)
		}
		return inner
	})
	return changed, err
}

// acceptVerifiedNodeTrafficLocked requires the normal allocation lock and
// traffic writer. Reconciliation remains an explicit operator responsibility.
func (s *InboundService) acceptVerifiedNodeTrafficLocked(nodeID int, envelope trafficprotocol.Envelope) (bool, error) {
	if nodeID <= 0 || len(envelope.Snapshot) == 0 || len(envelope.Snapshot) > trafficprotocol.MaxSnapshotBytes {
		return false, errors.New("invalid verified node snapshot")
	}
	// Copy before parsing; the same exact bytes are authenticated below.
	envelope.Snapshot = append([]byte(nil), envelope.Snapshot...)
	envelope.MAC = append([]byte(nil), envelope.MAC...)
	var snap runtime.TrafficSnapshot
	if err := json.Unmarshal(envelope.Snapshot, &snap); err != nil || snap.Inbounds == nil {
		return false, errors.New("invalid or incomplete verified node snapshot")
	}
	seen := make(map[string]bool, len(snap.Inbounds))
	canonicalTags := make(map[string]bool, len(snap.Inbounds))
	for _, ib := range snap.Inbounds {
		if ib == nil || ib.Tag == "" || ib.Tag != strings.TrimSpace(ib.Tag) || seen[ib.Tag] {
			return false, errors.New("ambiguous verified node inbound")
		}
		seen[ib.Tag] = true
		canonical := strings.TrimPrefix(ib.Tag, nodeTagPrefix(&nodeID))
		if canonical == "" || canonicalTags[canonical] {
			return false, errors.New("ambiguous node inbound tag aliases")
		}
		canonicalTags[canonical] = true
		// GetClients historically tolerates invalid JSON; a signed malformed
		// document must not be treated as an empty client configuration.
		var settings map[string]json.RawMessage
		if err := json.Unmarshal([]byte(ib.Settings), &settings); err != nil || settings == nil {
			return false, fmt.Errorf("invalid node inbound settings for %q", ib.Tag)
		}
		if clients, ok := settings["clients"]; ok {
			var decoded []model.Client
			if string(clients) == "null" || json.Unmarshal(clients, &decoded) != nil {
				return false, fmt.Errorf("invalid node clients for %q", ib.Tag)
			}
			stats := make(map[string]bool, len(ib.ClientStats))
			for _, stat := range ib.ClientStats {
				stats[nodeEmailKey(stat.Email)] = true
			}
			clientsSeen := make(map[string]bool, len(decoded))
			for _, client := range decoded {
				key := nodeEmailKey(client.Email)
				if key == "" || clientsSeen[key] || !stats[key] {
					return false, fmt.Errorf("missing or ambiguous node client traffic for %q", ib.Tag)
				}
				clientsSeen[key] = true
			}
		}
	}
	return s.setRemoteTrafficTransaction(nodeID, &snap, false, func(tx *gorm.DB) error {
		var node model.Node
		if err := tx.First(&node, "id = ?", nodeID).Error; err != nil {
			return err
		}
		if node.Scheme != "https" || (node.TlsVerifyMode != "" && node.TlsVerifyMode != "verify") || node.ConfigDirty || node.InboundSyncMode == "selected" {
			return errors.New("node configuration is not eligible for verified full snapshot merge")
		}
		key, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(node.ApiToken)
		if err != nil {
			return err
		}
		return trafficprotocol.ApplyVerifiedInTransaction(tx, fmt.Sprintf("node-inbound:%d", nodeID), key, envelope, func(_ *gorm.DB, _ []byte) error { return nil })
	})
}
