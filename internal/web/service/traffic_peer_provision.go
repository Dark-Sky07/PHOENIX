package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// validTrafficPeerIdentity accepts opaque canonical printable identifiers.
// Registration is an administrative trust decision, not proof of identity.
func validTrafficPeerIdentity(value string) bool {
	if value == "" || len(value) > 256 || value != strings.TrimSpace(value) {
		return false
	}
	for _, c := range value {
		if c < 0x21 || c > 0x7e {
			return false
		}
	}
	return true
}

// RegisterVerifiedTrafficReceiver binds an existing API token to one sender
// and generation. It never overwrites an existing sequence or re-registers a
// sender, including after token rotation. Reconciliation and secure transfer
// of the expected identity are an operator responsibility.
func RegisterVerifiedTrafficReceiver(tokenID int, sender, generation string, reconciled bool) error {
	if !reconciled || tokenID <= 0 || !validTrafficPeerIdentity(sender) || !validTrafficPeerIdentity(generation) {
		return errors.New("verified receiver requires a token, canonical peer identity, generation, and reconciliation confirmation")
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		return err
	}
	defer unlock()
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		var token model.ApiToken
		if err := tx.First(&token, "id = ?", tokenID).Error; err != nil {
			return err
		}
		scope := strings.ToLower(strings.TrimSpace(token.Scope))
		if !token.Enabled || (token.ExpiresAt > 0 && token.ExpiresAt <= time.Now().UnixMilli()) || (scope != "" && scope != "full") {
			return errors.New("verified receiver requires an enabled, unexpired full-scope token")
		}
		return tx.Create(&model.TrafficSnapshotReceiver{ID: fmt.Sprintf("api-token:%d", tokenID), Sender: sender, Generation: generation}).Error
	})
}

// RegisterVerifiedTrafficSender stages a new durable sequence but does not
// enable verified sends. The operator registers the receiver separately and
// then explicitly activates after an authenticated readiness probe.
func RegisterVerifiedTrafficSender(nodeID int, reconciled bool) (*model.TrafficSnapshotSender, error) {
	if nodeID <= 0 || !reconciled {
		return nil, errors.New("verified sender requires a node and reconciliation confirmation")
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		return nil, err
	}
	defer unlock()
	guid, err := (&SettingService{}).GetPanelGuid()
	if err != nil {
		return nil, err
	}
	if !validTrafficPeerIdentity(guid) {
		return nil, errors.New("invalid panel GUID")
	}
	generation, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	state := model.TrafficSnapshotSender{ID: fmt.Sprintf("node:%d", nodeID), Sender: guid, Generation: generation.String()}
	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		// GetPanelGuid can log a save failure; require a durable matching value.
		var setting model.Setting
		if err := tx.Where("key = ?", "panelGuid").First(&setting).Error; err != nil {
			return err
		}
		if setting.Value != guid {
			return errors.New("panel GUID is not durably registered")
		}
		var node model.Node
		if err := tx.First(&node, "id = ?", nodeID).Error; err != nil {
			return err
		}
		if node.VerifiedGlobalPush {
			return errors.New("node already requires verified global traffic")
		}
		if node.Scheme != "https" || (node.TlsVerifyMode != "" && node.TlsVerifyMode != "verify") || len(node.ApiToken) < 32 {
			return errors.New("verified sender requires HTTPS, standard TLS verification, and a valid node token")
		}
		if err := tx.Create(&state).Error; err != nil {
			return err
		}

		return nil
	})
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// ActivateVerifiedTrafficSender probes the exact configured receiver before
// enabling verified outbound mode. This is not reconciliation, a promise of
// future availability, nor validation of incoming node snapshots.
func ActivateVerifiedTrafficSender(ctx context.Context, nodeID int, reconciled bool) error {
	return activateVerifiedTrafficSender(ctx, nodeID, reconciled, func(ctx context.Context, node *model.Node, state model.TrafficSnapshotSender) error {
		return runtime.NewRemote(node).CheckTrafficReadiness(ctx, state.Sender, state.Generation, uint64(state.LastSequence))
	})
}

func activateVerifiedTrafficSender(ctx context.Context, nodeID int, reconciled bool, probe func(context.Context, *model.Node, model.TrafficSnapshotSender) error) error {
	if nodeID <= 0 || !reconciled || probe == nil {
		return errors.New("sender activation requires reconciliation confirmation")
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		return err
	}
	defer unlock()
	db := database.GetDB()
	var node model.Node
	if err := db.First(&node, "id = ?", nodeID).Error; err != nil {
		return err
	}
	if node.VerifiedGlobalPush {
		return errors.New("verified sender already active")
	}
	if node.Scheme != "https" || (node.TlsVerifyMode != "" && node.TlsVerifyMode != "verify") || len(node.ApiToken) < 32 {
		return errors.New("verified sender requires HTTPS, standard TLS verification, and a valid node token")
	}
	var state model.TrafficSnapshotSender
	if err := db.First(&state, "id = ?", fmt.Sprintf("node:%d", nodeID)).Error; err != nil {
		return err
	}
	if state.LastSequence < 0 || !validTrafficPeerIdentity(state.Sender) || !validTrafficPeerIdentity(state.Generation) {
		return errors.New("invalid sender state")
	}
	// No database transaction remains open during the bounded network probe.
	if err := probe(ctx, &node, state); err != nil {
		return err
	}
	return db.Transaction(func(tx *gorm.DB) error {
		var current model.TrafficSnapshotSender
		if err := tx.First(&current, "id = ?", state.ID).Error; err != nil {
			return err
		}
		if current != state {
			return errors.New("traffic sender state changed during readiness probe")
		}
		result := tx.Model(&model.Node{}).Where("id = ? AND verified_global_push = ? AND scheme = ? AND address = ? AND port = ? AND base_path = ? AND api_token = ? AND tls_verify_mode = ? AND allow_private_address = ?", node.Id, false, node.Scheme, node.Address, node.Port, node.BasePath, node.ApiToken, node.TlsVerifyMode, node.AllowPrivateAddress).Where("EXISTS (SELECT 1 FROM traffic_snapshot_senders s WHERE s.id = ? AND s.sender = ? AND s.generation = ? AND s.last_sequence = ?)", state.ID, state.Sender, state.Generation, state.LastSequence).Update("verified_global_push", true)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("node configuration changed during readiness probe")
		}
		return nil
	})
}
