package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"gorm.io/gorm"
)

// acceptVerifiedNodeTrafficLocked is an integration building block, not yet
// connected to the node sync job. Caller must hold the allocation lock and run
// on the traffic writer, with the usual incident barriers. Inbound transport,
// provisioning, downgrade prevention, migration and reconciliation are not
// implemented by this function; it must not be used to lift quota guards.
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
