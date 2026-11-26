package service

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/l2tp"
	"github.com/Dark-Sky07/phoenix/internal/pppvpn"
	"gorm.io/gorm"
)

// rejectUnprovenDaemonRemoval is an interim fence, not a drain protocol.
// Scope nil means all local inbounds, an empty non-nil scope none. Daemon exit,
// missing spool files, and OpenVPN management polls do not prove final traffic
// was committed. Known local attachments stay blocked pending reconciliation.
func rejectUnprovenDaemonRemoval(db *gorm.DB, emails []string, scope []int) error {
	if scope != nil && len(scope) == 0 {
		return nil
	}
	if db == nil {
		return errors.New("database unavailable while checking PPP removal")
	}
	keys := make([]string, 0, len(emails))
	seen := make(map[string]bool, len(emails))
	for _, email := range emails {
		key := transferEmailKey(email)
		if key == "" {
			return errors.New("client removal has no accounting identity; manual reconciliation required")
		}
		if !seen[key] {
			seen[key] = true
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		return nil
	}
	if err := pppvpn.CheckClientSessionEvidence(keys, scope); err != nil {
		return fmt.Errorf("PPP client removal requires reconciliation: %w", err)
	}
	if err := l2tp.CheckClientSessionEvidence(keys, scope); err != nil {
		return fmt.Errorf("L2TP client removal requires reconciliation: %w", err)
	}
	q := db.Where("node_id IS NULL AND protocol IN ?", []model.Protocol{model.PPTP, model.SSTP, model.L2TP, model.OpenVPN})
	if scope != nil {
		q = q.Where("id IN ?", scope)
	}
	var inbounds []model.Inbound
	if err := q.Find(&inbounds).Error; err != nil {
		return err
	}
	for _, inbound := range inbounds {
		label := "PPP"
		if inbound.Protocol == model.OpenVPN {
			label = "OpenVPN"
		}
		// SQL LOWER/TRIM does not match transferEmailKey for all Unicode case
		// and whitespace. Inspect linked identities in Go, otherwise a stale
		// link hidden by SQL normalization could authorize unsafe removal.
		var links []struct {
			ClientID int     `gorm:"column:client_id"`
			Email    *string `gorm:"column:email"`
		}
		if err := db.Table("client_inbounds").Select("client_inbounds.client_id, clients.email").
			Joins("LEFT JOIN clients ON clients.id = client_inbounds.client_id").
			Where("client_inbounds.inbound_id = ?", inbound.Id).Scan(&links).Error; err != nil {
			return err
		}
		for _, link := range links {
			if link.Email == nil || transferEmailKey(*link.Email) == "" {
				return fmt.Errorf("%s inbound %d has an unresolved client link %d; manual reconciliation required", label, inbound.Id, link.ClientID)
			}
			if seen[transferEmailKey(*link.Email)] {
				return fmt.Errorf("%s inbound %d requires verified disconnect and accounting before client removal", label, inbound.Id)
			}
		}
		var settings map[string]json.RawMessage
		if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil || settings == nil {
			return fmt.Errorf("%s inbound %d settings cannot be verified for removal", label, inbound.Id)
		}
		if raw, ok := settings["clients"]; ok && string(raw) != "null" {
			var clients []struct {
				Email string `json:"email"`
			}
			if err := json.Unmarshal(raw, &clients); err != nil {
				return fmt.Errorf("%s inbound %d client list cannot be verified: %w", label, inbound.Id, err)
			}
			for _, client := range clients {
				if transferEmailKey(client.Email) == "" {
					return fmt.Errorf("%s inbound %d has a client without an accounting identity; manual reconciliation required", label, inbound.Id)
				}
				if seen[transferEmailKey(client.Email)] {
					return fmt.Errorf("%s inbound %d requires verified disconnect and accounting before client removal", label, inbound.Id)
				}
			}
		}
	}
	return nil
}
