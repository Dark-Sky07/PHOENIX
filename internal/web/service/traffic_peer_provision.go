package service

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
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

// RegisterVerifiedTrafficSender creates a fresh sequence and enables verified
// mode atomically. It never resets existing state. The confirmation indicates
// that an operator has reconciled existing usage and understands that the
// receiver must be registered separately; it is not a remote attestation.
// A generated generation is returned for secure out-of-band registration.
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
		result := tx.Model(&model.Node{}).Where("id = ? AND verified_global_push = ?", nodeID, false).Update("verified_global_push", true)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("node verification mode changed concurrently")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &state, nil
}
