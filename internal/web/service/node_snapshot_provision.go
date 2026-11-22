package service

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
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

// RegisterNodeSnapshotReceiver probes a nonce-bound, signed snapshot before
// permanently switching away from legacy sync. The probe advances producer
// sequence but does not apply accounting; its sequence is fenced in the new
// receiver row. Readiness is not proof of historical or unpolled completeness.
func RegisterNodeSnapshotReceiver(ctx context.Context, nodeID int, sender, generation string, reconciled bool) error {
	return registerNodeSnapshotReceiver(ctx, nodeID, sender, generation, reconciled, func(ctx context.Context, node *model.Node) (*trafficprotocol.Envelope, error) {
		return runtime.NewRemote(node).FetchVerifiedNodeSnapshot(ctx)
	})
}

func registerNodeSnapshotReceiver(ctx context.Context, nodeID int, sender, generation string, reconciled bool, probe func(context.Context, *model.Node) (*trafficprotocol.Envelope, error)) error {
	if nodeID <= 0 || !reconciled || !validTrafficPeerIdentity(sender) || !validTrafficPeerIdentity(generation) || probe == nil {
		return errors.New("receiver registration requires reviewed canonical identity and reconciliation confirmation")
	}
	db := database.GetDB()
	var node model.Node
	if err := db.First(&node, "id = ?", nodeID).Error; err != nil {
		return err
	}
	if node.Scheme != "https" || (node.TlsVerifyMode != "" && node.TlsVerifyMode != "verify") || len(node.ApiToken) < 32 || node.InboundSyncMode == "selected" {
		return errors.New("receiver requires full inbound sync, HTTPS, standard TLS and a valid token")
	}
	required, err := NodeInboundVerificationRequired(nodeID)
	if err != nil {
		return err
	}
	if required {
		return errors.New("node snapshot receiver already registered")
	}
	// Remote producer takes its own allocation lock. Do not hold the local
	// allocation lock across the network, avoiding reciprocal lock deadlock.
	envelope, err := probe(ctx, &node)
	if err != nil {
		return err
	}
	if envelope == nil || envelope.Sequence > math.MaxInt64 {
		return errors.New("invalid node readiness snapshot")
	}
	key, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(node.ApiToken)
	if err != nil {
		return err
	}
	if err := trafficprotocol.Verify(*envelope, key, trafficprotocol.Expected{Sender: sender, Generation: generation}); err != nil {
		return err
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		return err
	}
	defer unlock()
	return db.Transaction(func(tx *gorm.DB) error {
		var current model.Node
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&current, "id = ?", nodeID).Error; err != nil {
			return err
		}
		if current.Scheme != node.Scheme || current.TlsVerifyMode != node.TlsVerifyMode || current.ApiToken != node.ApiToken || current.Address != node.Address || current.Port != node.Port || current.BasePath != node.BasePath || current.AllowPrivateAddress != node.AllowPrivateAddress || current.InboundSyncMode != node.InboundSyncMode {
			return errors.New("node configuration changed during readiness probe")
		}
		return tx.Create(&model.TrafficSnapshotReceiver{ID: fmt.Sprintf("node-inbound:%d", nodeID), Sender: sender, Generation: generation, LastSequence: int64(envelope.Sequence)}).Error
	})
}
