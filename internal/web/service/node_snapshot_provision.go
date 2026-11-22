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

// RegisterNodeSnapshotProducer stages a token-bound producer on the node. It
// never overwrites state; identity is transferred securely to the master.
func RegisterNodeSnapshotProducer(tokenID int, reconciled bool) (*model.TrafficSnapshotSender, error) {
	if tokenID <= 0 || !reconciled {
		return nil, errors.New("producer registration requires reconciliation confirmation")
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
		return nil, errors.New("invalid panel identity")
	}
	generation, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	state := model.TrafficSnapshotSender{ID: fmt.Sprintf("node-snapshot:api-token:%d", tokenID), Sender: guid, Generation: generation.String()}
	err = database.GetDB().Transaction(func(tx *gorm.DB) error {
		var setting model.Setting
		if err := tx.Where("key = ?", "panelGuid").First(&setting).Error; err != nil {
			return err
		}
		if setting.Value != guid {
			return errors.New("panel identity not durable")
		}
		var token model.ApiToken
		if err := tx.First(&token, "id = ?", tokenID).Error; err != nil {
			return err
		}
		scope := strings.ToLower(strings.TrimSpace(token.Scope))
		if !token.Enabled || (token.ExpiresAt > 0 && token.ExpiresAt <= time.Now().UnixMilli()) || (scope != "" && scope != "full") {
			return errors.New("producer requires enabled unexpired full-scope token")
		}
		return tx.Create(&state).Error
	})
	if err != nil {
		return nil, err
	}
	return &state, nil
}

// RegisterNodeSnapshotReceiver permanently switches participating node sync
// away from legacy traffic. Provisioning must be coordinated out of band and
// may interrupt node synchronization if the remote is not ready. This does not
// release quota guards or validate unpolled historical usage.
func RegisterNodeSnapshotReceiver(nodeID int, sender, generation string, reconciled bool) error {
	if nodeID <= 0 || !reconciled || !validTrafficPeerIdentity(sender) || !validTrafficPeerIdentity(generation) {
		return errors.New("receiver registration requires reviewed canonical identity and reconciliation confirmation")
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		return err
	}
	defer unlock()
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		var node model.Node
		if err := tx.First(&node, "id = ?", nodeID).Error; err != nil {
			return err
		}
		if node.Scheme != "https" || (node.TlsVerifyMode != "" && node.TlsVerifyMode != "verify") || len(node.ApiToken) < 32 || node.InboundSyncMode == "selected" {
			return errors.New("receiver requires full inbound sync, HTTPS, standard TLS and a valid token")
		}
		return tx.Create(&model.TrafficSnapshotReceiver{ID: fmt.Sprintf("node-inbound:%d", nodeID), Sender: sender, Generation: generation}).Error
	})
}
