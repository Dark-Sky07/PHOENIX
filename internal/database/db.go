// Package database provides database initialization, migration, and management utilities
// for the PHOENIX panel using GORM with SQLite or PostgreSQL.
package database

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"log"
	"math"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"
	"github.com/Dark-Sky07/phoenix/internal/util/random"
	"github.com/Dark-Sky07/phoenix/internal/xray"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

var db *gorm.DB

const (
	DialectSQLite   = "sqlite"
	DialectPostgres = "postgres"
)

// IsPostgres reports whether the active connection is a PostgreSQL backend.
func IsPostgres() bool {
	if db == nil {
		return config.GetDBKind() == "postgres"
	}
	return db.Dialector.Name() == "postgres"
}

// Dialect returns the active GORM dialect name, or "" if the DB is not open.
func Dialect() string {
	if db == nil {
		return ""
	}
	return db.Dialector.Name()
}

// A second panel process, migrate CLI or old service still shutting down can
// race GORM's first CREATE TYPE/INDEX during AutoMigrate. PostgreSQL reports
// that race as SQLSTATE 23505 on pg_type/pg_index. Serialize schema migration
// across processes on a session advisory lock; the lock is distinct from the
// traffic-allocation key used by the quota path.
const schemaMigrationAdvisoryKey int64 = 0x50484f454e4959

func withPostgresSchemaMigrationLock(pool *sql.DB, migrate func() error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pool.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, "SELECT pg_advisory_lock($1)", schemaMigrationAdvisoryKey); err != nil {
		return err
	}
	defer func() {
		_, _ = conn.ExecContext(context.Background(), "SELECT pg_advisory_unlock($1)", schemaMigrationAdvisoryKey)
	}()
	return migrate()
}

// isConcurrentSchemaChangeErr identifies the PostgreSQL catalog race GORM can
// hit when two processes migrate the same fresh schema. It does not hide real
// duplicate data or constraint violations in tables.
func isConcurrentSchemaChangeErr(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	if !strings.Contains(msg, "sqlstate 23505") &&
		!strings.Contains(msg, "duplicate key value violates unique constraint") {
		return false
	}
	for _, catalog := range []string{"pg_type_", "pg_index", "pg_class", "pg_constraint", "pg_namespace"} {
		if strings.Contains(msg, catalog) {
			return true
		}
	}
	return false
}

func initModels() error {
	if err := dropLegacyResellerInboundUniqueIndex(); err != nil {
		return err
	}
	models := []any{
		&model.User{},
		&model.Inbound{},
		&model.OutboundTraffics{},
		&model.Setting{},
		&model.InboundClientIps{},
		&xray.ClientTraffic{},
		&model.HistoryOfSeeders{},
		&model.Node{},
		// Phase C fleet models are additive and intentionally independent from
		// legacy nodes. Existing direct-node installations do not opt into the
		// outbound agent control plane merely by upgrading.
		&model.FleetTemplate{},
		&model.FleetTemplateRevision{},
		&model.FleetAgentEnrollment{},
		&model.FleetAssignment{},
		&model.FleetPool{},
		&model.FleetPoolMember{},
		&model.FleetAgentTunnelStatus{},
		&model.ManagedTunnel{},
		&model.ApiToken{},
		&model.InitialSetup{},
		&model.LoginProtection{},
		&model.UserSession{},
		&model.RecoveryCode{},
		&model.WebhookSubscription{},
		&model.WebhookDelivery{},
		&model.DNSCertificateProfile{},
		&model.RuleSetCatalog{},
		&model.DashboardPreference{},
		&model.OIDCProviderConfig{},
		&model.ClientRecord{},
		&model.ClientInbound{},
		&model.ClientDevice{},
		&model.ClientGroup{},
		&model.InboundFallback{},
		&model.NodeClientTraffic{},
		&model.ResellerNodeCounterIncident{},
		&model.ClientGlobalTraffic{},
		&model.PPPClientTrafficCounter{},
		&model.OpenVPNSessionCounter{},
		&model.OutboundSubscription{},
		&model.Reseller{},
		&model.ResellerInbound{},
		&model.ResellerClient{},
		&model.ResellerClientMappingArchive{},
		&model.ResellerTransaction{},
		&model.ResellerSale{},
		&model.ResellerCreationIntent{},
		&model.ResellerHistoricalSale{},
		&model.ResellerTrafficArchive{},
		&model.BotNotifyState{},
		&model.ClientInvite{},
		&model.TuicTrafficReceipt{},
		&model.XrayTrafficReceipt{},
		&model.XrayTrafficInFlight{},
		&model.TrafficSnapshotReceiver{},
		&model.TrafficSnapshotSender{},
		&model.UserTemplate{},
		&model.SupportTicket{},
		&model.SupportTicketMessage{},
		&model.PaymentOrder{},
		&model.AuditLog{},
		&model.ResellerVoucher{},
		&model.ResellerNotification{},
	}
	for _, mdl := range models {
		var migrateErr error
		for attempt := 0; ; attempt++ {
			migrateErr = db.AutoMigrate(mdl)
			if migrateErr == nil {
				break
			}
			if isIgnorableDuplicateColumnErr(migrateErr, mdl) {
				log.Printf("Ignoring duplicate column during auto migration for %T: %v", mdl, migrateErr)
				migrateErr = nil
				break
			}
			if isConcurrentSchemaChangeErr(migrateErr) && attempt < 3 {
				time.Sleep(time.Duration(attempt+1) * 200 * time.Millisecond)
				continue
			}
			log.Printf("Error auto migrating model %T: %v", mdl, migrateErr)
			return migrateErr
		}
		if migrateErr != nil {
			return migrateErr
		}
	}
	if err := ensureOIDCIdentityIndex(); err != nil {
		return err
	}
	if err := dropLegacyForeignKeys(); err != nil {
		return err
	}
	if err := dropLegacyResellerInboundUniqueIndex(); err != nil {
		return err
	}
	if err := pruneOrphanedClientInbounds(); err != nil {
		return err
	}
	if err := normalizeInboundSubSortIndex(); err != nil {
		return err
	}
	if IsPostgres() {
		if err := resyncPostgresSequences(db, models); err != nil {
			log.Printf("Error resyncing postgres sequences: %v", err)
			return err
		}
	}
	return nil
}

// ensureOIDCIdentityIndex prevents one external identity from being bound to
// two panel users. It is a partial unique index, deliberately excluding legacy
// empty columns so an additive migration never changes existing user records.
func ensureOIDCIdentityIndex() error {
	if db == nil || !db.Migrator().HasTable("users") {
		return nil
	}
	const query = `CREATE UNIQUE INDEX IF NOT EXISTS idx_users_oidc_identity ON users (oidc_issuer, oidc_subject) WHERE oidc_issuer <> '' AND oidc_subject <> ''`
	if err := db.Exec(query).Error; err != nil {
		log.Printf("Error creating OIDC identity index: %v", err)
		return err
	}
	return nil
}

func dropLegacyResellerInboundUniqueIndex() error {
	if db == nil || !db.Migrator().HasTable("reseller_inbounds") {
		return nil
	}
	if db.Migrator().HasIndex(&model.ResellerInbound{}, "ux_reseller_inbounds_inbound") {
		if err := db.Migrator().DropIndex(&model.ResellerInbound{}, "ux_reseller_inbounds_inbound"); err != nil {
			log.Printf("Error dropping legacy reseller inbound unique index: %v", err)
			return err
		}
	}
	return nil
}

func dropLegacyForeignKeys() error {
	if !IsPostgres() {
		return nil
	}
	if err := db.Exec("ALTER TABLE client_traffics DROP CONSTRAINT IF EXISTS fk_inbounds_client_stats").Error; err != nil {
		log.Printf("Error dropping legacy foreign key fk_inbounds_client_stats: %v", err)
		return err
	}
	return nil
}

func pruneOrphanedClientInbounds() error {
	// Preserve evidence for reseller-owned clients. An orphaned link may be
	// the only surviving sign of an unverified runtime association; deleting
	// it would let quota checks mistake missing evidence for a local client.
	res := db.Exec(`DELETE FROM client_inbounds WHERE inbound_id NOT IN (SELECT id FROM inbounds)
		AND NOT EXISTS (SELECT 1 FROM clients c JOIN reseller_clients rc
			ON LOWER(TRIM(rc.email)) = LOWER(TRIM(c.email))
			WHERE c.id = client_inbounds.client_id)`)
	if res.Error != nil {
		log.Printf("Error pruning orphaned client_inbounds rows: %v", res.Error)
		return res.Error
	}
	if res.RowsAffected > 0 {
		log.Printf("Pruned %d orphaned client_inbounds row(s)", res.RowsAffected)
	}
	return nil
}

// normalizeInboundSubSortIndex lifts sub_sort_index values below the 1-based
// minimum (rows written by builds that defaulted the column to 0, or by nodes
// predating the field) so they cannot sort ahead of explicitly ranked inbounds.
func normalizeInboundSubSortIndex() error {
	res := db.Exec("UPDATE inbounds SET sub_sort_index = 1 WHERE sub_sort_index < 1")
	if res.Error != nil {
		log.Printf("Error normalizing inbound sub_sort_index: %v", res.Error)
		return res.Error
	}
	if res.RowsAffected > 0 {
		log.Printf("Normalized sub_sort_index on %d inbound(s)", res.RowsAffected)
	}
	return nil
}

func isIgnorableDuplicateColumnErr(err error, mdl any) bool {
	if err == nil {
		return false
	}
	errMsg := strings.ToLower(err.Error())
	// SQLite: "duplicate column name: foo"
	// Postgres: `pq: column "foo" of relation "bar" already exists` / `sqlstate 42701`
	const sqlitePrefix = "duplicate column name:"
	if _, after, ok := strings.Cut(errMsg, sqlitePrefix); ok {
		col := strings.TrimSpace(after)
		col = strings.Trim(col, "`\"[]")
		return col != "" && db != nil && db.Migrator().HasColumn(mdl, col)
	}
	if strings.Contains(errMsg, "already exists") && strings.Contains(errMsg, "column ") {
		// Best effort: extract the column name between the first pair of double quotes.
		if _, after, ok := strings.Cut(errMsg, "column \""); ok {
			rest := after
			if e := strings.Index(rest, "\""); e > 0 {
				col := rest[:e]
				return col != "" && db != nil && db.Migrator().HasColumn(mdl, col)
			}
		}
	}
	return false
}

// initUser intentionally leaves a fresh panel without an account. The web
// bootstrap service creates the first superadmin through a short-lived,
// one-time setup link. Existing installations already have users and are not
// modified by this behavior.
func initUser() error { return nil }

// runSeeders migrates user passwords to bcrypt and records seeder execution to prevent re-running.
func runSeeders(isUsersEmpty bool) error {
	empty, err := isTableEmpty("history_of_seeders")
	if err != nil {
		log.Printf("Error checking if users table is empty: %v", err)
		return err
	}

	if empty && isUsersEmpty {
		seeders := []string{"UserPasswordHash", "ClientsTable", "InboundClientsArrayFix", "InboundClientTgIdFix", "InboundClientSubIdFix", "FreedomFinalRulesReverseFix", "ApiTokensHash"}
		for _, name := range seeders {
			if err := db.Create(&model.HistoryOfSeeders{SeederName: name}).Error; err != nil {
				return err
			}
		}
		return seedApiTokens()
	}

	var seedersHistory []string
	if err := db.Model(&model.HistoryOfSeeders{}).Pluck("seeder_name", &seedersHistory).Error; err != nil {
		log.Printf("Error fetching seeder history: %v", err)
		return err
	}

	if !slices.Contains(seedersHistory, "UserPasswordHash") && !isUsersEmpty {
		var users []model.User
		if err := db.Find(&users).Error; err != nil {
			log.Printf("Error fetching users for password migration: %v", err)
			return err
		}

		for _, user := range users {
			if crypto.IsHashed(user.Password) {
				continue
			}
			hashedPassword, err := crypto.HashPasswordAsBcrypt(user.Password)
			if err != nil {
				log.Printf("Error hashing password for user '%s': %v", user.Username, err)
				return err
			}
			if err := db.Model(&user).Update("password", hashedPassword).Error; err != nil {
				log.Printf("Error updating password for user '%s': %v", user.Username, err)
				return err
			}
		}

		hashSeeder := &model.HistoryOfSeeders{
			SeederName: "UserPasswordHash",
		}
		if err := db.Create(hashSeeder).Error; err != nil {
			return err
		}
	}

	if !slices.Contains(seedersHistory, "ApiTokensTable") {
		if err := seedApiTokens(); err != nil {
			return err
		}
	}

	if !slices.Contains(seedersHistory, "ApiTokensHash") {
		if err := hashExistingApiTokens(); err != nil {
			return err
		}
	}

	if !slices.Contains(seedersHistory, "ClientsTable") {
		if err := seedClientsFromInboundJSON(); err != nil {
			return err
		}
	}

	if !slices.Contains(seedersHistory, "InboundClientsArrayFix") {
		if err := normalizeInboundClientsArray(); err != nil {
			return err
		}
	}

	if !slices.Contains(seedersHistory, "InboundClientTgIdFix") {
		if err := normalizeInboundClientTgId(); err != nil {
			return err
		}
	}

	if !slices.Contains(seedersHistory, "InboundClientSubIdFix") {
		if err := normalizeInboundClientSubId(); err != nil {
			return err
		}
	}

	if !slices.Contains(seedersHistory, "FreedomFinalRulesReverseFix") {
		if err := normalizeFreedomFinalRules(); err != nil {
			return err
		}
	}
	return nil
}

func normalizeInboundClientTgId() error {
	var inbounds []model.Inbound
	if err := db.Find(&inbounds).Error; err != nil {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, inbound := range inbounds {
			if strings.TrimSpace(inbound.Settings) == "" {
				continue
			}
			var settings map[string]any
			if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
				log.Printf("InboundClientTgIdFix: skip inbound %d (invalid settings json): %v", inbound.Id, err)
				continue
			}
			clients, ok := settings["clients"].([]any)
			if !ok {
				continue
			}
			mutated := false
			for i, raw := range clients {
				obj, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				tgRaw, present := obj["tgId"]
				if !present {
					continue
				}
				v, isFloat := tgRaw.(float64)
				if isFloat && !math.IsNaN(v) && !math.IsInf(v, 0) && v == math.Trunc(v) {
					continue
				}
				obj["tgId"] = int64(0)
				clients[i] = obj
				mutated = true
			}
			if !mutated {
				continue
			}
			settings["clients"] = clients
			newSettings, err := json.MarshalIndent(settings, "", "  ")
			if err != nil {
				log.Printf("InboundClientTgIdFix: skip inbound %d (marshal failed): %v", inbound.Id, err)
				continue
			}
			if err := tx.Model(&model.Inbound{}).Where("id = ?", inbound.Id).
				Update("settings", string(newSettings)).Error; err != nil {
				return err
			}
		}
		return tx.Create(&model.HistoryOfSeeders{SeederName: "InboundClientTgIdFix"}).Error
	})
}

func normalizeInboundClientSubId() error {
	var inbounds []model.Inbound
	if err := db.Find(&inbounds).Error; err != nil {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, inbound := range inbounds {
			if strings.TrimSpace(inbound.Settings) == "" {
				continue
			}
			var settings map[string]any
			if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
				log.Printf("InboundClientSubIdFix: skip inbound %d (invalid settings json): %v", inbound.Id, err)
				continue
			}
			clients, ok := settings["clients"].([]any)
			if !ok {
				continue
			}
			mutated := false
			for i, raw := range clients {
				obj, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				existing, _ := obj["subId"].(string)
				if strings.TrimSpace(existing) != "" {
					continue
				}
				obj["subId"] = random.NumLower(16)
				clients[i] = obj
				mutated = true
			}
			if !mutated {
				continue
			}
			settings["clients"] = clients
			newSettings, err := json.MarshalIndent(settings, "", "  ")
			if err != nil {
				log.Printf("InboundClientSubIdFix: skip inbound %d (marshal failed): %v", inbound.Id, err)
				continue
			}
			if err := tx.Model(&model.Inbound{}).Where("id = ?", inbound.Id).
				Update("settings", string(newSettings)).Error; err != nil {
				return err
			}
		}
		return tx.Create(&model.HistoryOfSeeders{SeederName: "InboundClientSubIdFix"}).Error
	})
}

func normalizeInboundClientsArray() error {
	var inbounds []model.Inbound
	if err := db.Find(&inbounds).Error; err != nil {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		for _, inbound := range inbounds {
			if strings.TrimSpace(inbound.Settings) == "" {
				continue
			}
			var settings map[string]any
			if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
				log.Printf("InboundClientsArrayFix: skip inbound %d (invalid settings json): %v", inbound.Id, err)
				continue
			}
			raw, exists := settings["clients"]
			if !exists || raw != nil {
				continue
			}
			settings["clients"] = []any{}
			newSettings, err := json.MarshalIndent(settings, "", "  ")
			if err != nil {
				log.Printf("InboundClientsArrayFix: skip inbound %d (marshal failed): %v", inbound.Id, err)
				continue
			}
			if err := tx.Model(&model.Inbound{}).Where("id = ?", inbound.Id).
				Update("settings", string(newSettings)).Error; err != nil {
				return err
			}
		}
		return tx.Create(&model.HistoryOfSeeders{SeederName: "InboundClientsArrayFix"}).Error
	})
}

func normalizeFreedomFinalRules() error {
	var setting model.Setting
	err := db.Model(model.Setting{}).Where("key = ?", "xrayTemplateConfig").First(&setting).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return db.Create(&model.HistoryOfSeeders{SeederName: "FreedomFinalRulesReverseFix"}).Error
	}
	if err != nil {
		return err
	}

	updated, changed, rErr := rewriteFreedomFinalRules(setting.Value)
	if rErr != nil {
		log.Printf("FreedomFinalRulesReverseFix: skip (invalid xrayTemplateConfig json): %v", rErr)
		return db.Create(&model.HistoryOfSeeders{SeederName: "FreedomFinalRulesReverseFix"}).Error
	}

	return db.Transaction(func(tx *gorm.DB) error {
		if changed {
			if err := tx.Model(&model.Setting{}).Where("key = ?", "xrayTemplateConfig").
				Update("value", updated).Error; err != nil {
				return err
			}
		}
		return tx.Create(&model.HistoryOfSeeders{SeederName: "FreedomFinalRulesReverseFix"}).Error
	})
}

func rewriteFreedomFinalRules(raw string) (string, bool, error) {
	if strings.TrimSpace(raw) == "" {
		return raw, false, nil
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return raw, false, err
	}
	outbounds, ok := cfg["outbounds"].([]any)
	if !ok {
		return raw, false, nil
	}
	changed := false
	for _, ob := range outbounds {
		obj, ok := ob.(map[string]any)
		if !ok {
			continue
		}
		if proto, _ := obj["protocol"].(string); proto != "freedom" {
			continue
		}
		settings, ok := obj["settings"].(map[string]any)
		if !ok {
			continue
		}
		if !isLegacyPrivateOnlyFinalRules(settings["finalRules"]) {
			continue
		}
		settings["finalRules"] = []any{map[string]any{"action": "allow"}}
		changed = true
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return raw, false, err
	}
	return string(out), true, nil
}

func isLegacyPrivateOnlyFinalRules(v any) bool {
	rules, ok := v.([]any)
	if !ok || len(rules) != 1 {
		return false
	}
	rule, ok := rules[0].(map[string]any)
	if !ok {
		return false
	}
	if action, _ := rule["action"].(string); action != "allow" {
		return false
	}
	ips, ok := rule["ip"].([]any)
	if !ok || len(ips) != 1 {
		return false
	}
	if s, _ := ips[0].(string); s != "geoip:private" {
		return false
	}
	for k := range rule {
		if k != "action" && k != "ip" {
			return false
		}
	}
	return true
}

// normalizeClientJSONFields coerces loosely-typed numeric fields in a raw
// settings.clients entry so json.Unmarshal into model.Client doesn't fail
// when older rows wrote tgId/limitIp/totalGB/etc. as strings. Empty strings
// drop the key so the field falls back to its zero value.
func normalizeClientJSONFields(obj map[string]any) {
	normalizeInt := func(key string) {
		raw, exists := obj[key]
		if !exists {
			return
		}
		s, ok := raw.(string)
		if !ok {
			return
		}
		trimmed := strings.ReplaceAll(strings.TrimSpace(s), " ", "")
		if trimmed == "" {
			delete(obj, key)
			return
		}
		if n, err := strconv.ParseInt(trimmed, 10, 64); err == nil {
			obj[key] = n
		} else {
			delete(obj, key)
		}
	}
	for _, k := range []string{"tgId", "limitIp", "totalGB", "expiryTime", "reset", "created_at", "updated_at"} {
		normalizeInt(k)
	}
}

func seedClientsFromInboundJSON() error {
	var inbounds []model.Inbound
	if err := db.Find(&inbounds).Error; err != nil {
		return err
	}

	return db.Transaction(func(tx *gorm.DB) error {
		byEmail := map[string]*model.ClientRecord{}

		var existing []model.ClientRecord
		if err := tx.Find(&existing).Error; err != nil {
			return err
		}
		for i := range existing {
			byEmail[existing[i].Email] = &existing[i]
		}

		for _, inbound := range inbounds {
			if strings.TrimSpace(inbound.Settings) == "" {
				continue
			}
			var settings map[string]any
			if err := json.Unmarshal([]byte(inbound.Settings), &settings); err != nil {
				log.Printf("ClientsTable seed: skip inbound %d (invalid settings json): %v", inbound.Id, err)
				continue
			}
			rawList, ok := settings["clients"].([]any)
			if !ok {
				continue
			}

			for _, raw := range rawList {
				obj, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				normalizeClientJSONFields(obj)
				blob, err := json.Marshal(obj)
				if err != nil {
					continue
				}
				var c model.Client
				if err := json.Unmarshal(blob, &c); err != nil {
					log.Printf("ClientsTable seed: skip client in inbound %d (unmarshal failed): %v; payload=%s",
						inbound.Id, err, string(blob))
					continue
				}
				email := strings.TrimSpace(c.Email)
				if email == "" {
					continue
				}
				incoming := c.ToRecord()

				row, dup := byEmail[email]
				if !dup {
					if err := tx.Create(incoming).Error; err != nil {
						return err
					}
					byEmail[email] = incoming
					row = incoming
				} else {
					conflicts := model.MergeClientRecord(row, incoming)
					for _, x := range conflicts {
						log.Printf("client merge: email=%s conflict on %s old=%v new=%v kept=%v",
							email, x.Field, x.Old, x.New, x.Kept)
					}
					if err := tx.Save(row).Error; err != nil {
						return err
					}
				}

				link := model.ClientInbound{
					ClientId:     row.Id,
					InboundId:    inbound.Id,
					FlowOverride: c.Flow,
				}
				if err := tx.Where("client_id = ? AND inbound_id = ?", row.Id, inbound.Id).
					FirstOrCreate(&link).Error; err != nil {
					return err
				}
			}
		}

		return tx.Create(&model.HistoryOfSeeders{SeederName: "ClientsTable"}).Error
	})
}

// seedApiTokens copies the legacy `apiToken` setting into the new
// api_tokens table as a row named "default" so existing central panels
// keep working after the upgrade. Idempotent — records itself in
// history_of_seeders and only runs when api_tokens is empty.
func seedApiTokens() error {
	empty, err := isTableEmpty("api_tokens")
	if err != nil {
		return err
	}
	if empty {
		var legacy model.Setting
		err := db.Model(model.Setting{}).Where("key = ?", "apiToken").First(&legacy).Error
		if err == nil && legacy.Value != "" {
			row := &model.ApiToken{
				Name:    "default",
				Token:   legacy.Value,
				Enabled: true,
			}
			if err := db.Create(row).Error; err != nil {
				log.Printf("Error migrating legacy apiToken: %v", err)
				return err
			}
		}
	}
	return db.Create(&model.HistoryOfSeeders{SeederName: "ApiTokensTable"}).Error
}

// hashExistingApiTokens replaces any plaintext token stored before tokens were
// hashed at rest with its SHA-256 digest. Callers keep their plaintext copy
// (used on remote nodes), so existing tokens keep authenticating; the panel
// just can no longer reveal them. Idempotent — already-hashed rows are skipped.
func hashExistingApiTokens() error {
	var rows []*model.ApiToken
	if err := db.Find(&rows).Error; err != nil {
		return err
	}
	for _, r := range rows {
		if crypto.IsSHA256Hex(r.Token) {
			continue
		}
		hashed := crypto.HashTokenSHA256(r.Token)
		if err := db.Model(model.ApiToken{}).Where("id = ?", r.Id).Update("token", hashed).Error; err != nil {
			log.Printf("Error hashing api token %d: %v", r.Id, err)
			return err
		}
	}
	return db.Create(&model.HistoryOfSeeders{SeederName: "ApiTokensHash"}).Error
}

// isTableEmpty returns true if the named table contains zero rows.
func isTableEmpty(tableName string) (bool, error) {
	var count int64
	err := db.Table(tableName).Count(&count).Error
	return count == 0, err
}

// InitDB sets up the database connection, migrates models, and runs seeders.
// When XUI_DB_TYPE=postgres, dbPath is ignored and XUI_DB_DSN is used instead.
func InitDB(dbPath string) error {
	var gormLogger logger.Interface
	if config.IsDebug() {
		gormLogger = logger.New(
			log.New(os.Stdout, "\r\n", log.LstdFlags),
			logger.Config{
				SlowThreshold:             time.Second,
				LogLevel:                  logger.Info,
				IgnoreRecordNotFoundError: true,
				Colorful:                  true,
			},
		)
	} else {
		gormLogger = logger.Discard
	}
	c := &gorm.Config{Logger: gormLogger, DisableForeignKeyConstraintWhenMigrating: true}

	var err error
	switch config.GetDBKind() {
	case "postgres":
		dsn := config.GetDBDSN()
		if dsn == "" {
			return errors.New("XUI_DB_TYPE=postgres but XUI_DB_DSN is empty")
		}
		db, err = gorm.Open(postgres.Open(dsn), c)
		if err != nil {
			return err
		}
	default:
		dir := path.Dir(dbPath)
		if err = os.MkdirAll(dir, 0755); err != nil {
			return err
		}
		dsn := dbPath + "?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_txlock=immediate"
		db, err = gorm.Open(sqlite.Open(dsn), c)
		if err != nil {
			return err
		}
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		if _, err := sqlDB.Exec("PRAGMA journal_mode=WAL"); err != nil {
			return err
		}
		if _, err := sqlDB.Exec("PRAGMA busy_timeout=10000"); err != nil {
			return err
		}
		if _, err := sqlDB.Exec("PRAGMA synchronous=NORMAL"); err != nil {
			return err
		}
	}

	sqlDB, err := db.DB()
	if err != nil {
		return err
	}
	var maxOpen, maxIdle int
	switch config.GetDBKind() {
	case "postgres":
		maxOpen = envInt("XUI_DB_MAX_OPEN_CONNS", 25)
		maxIdle = envInt("XUI_DB_MAX_IDLE_CONNS", 25)
	default:
		maxOpen = envInt("XUI_DB_MAX_OPEN_CONNS", 8)
		maxIdle = envInt("XUI_DB_MAX_IDLE_CONNS", 4)
	}
	sqlDB.SetMaxOpenConns(maxOpen)
	sqlDB.SetMaxIdleConns(maxIdle)
	sqlDB.SetConnMaxLifetime(time.Hour)
	sqlDB.SetConnMaxIdleTime(30 * time.Minute)

	bootstrap := func() error {
		if err := initModels(); err != nil {
			return err
		}
		usersEmpty, err := isTableEmpty("users")
		if err != nil {
			return err
		}
		if err := initUser(); err != nil {
			return err
		}
		return runSeeders(usersEmpty)
	}
	if IsPostgres() {
		return withPostgresSchemaMigrationLock(sqlDB, bootstrap)
	}
	return bootstrap()
}

func envInt(key string, def int) int {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil || n <= 0 {
		return def
	}
	return n
}

// CloseDB closes the database connection if it exists.
func CloseDB() error {
	if db != nil {
		sqlDB, err := db.DB()
		if err != nil {
			return err
		}
		return sqlDB.Close()
	}
	return nil
}

// GetDB returns the global GORM database instance.
func GetDB() *gorm.DB {
	return db
}

func IsNotFound(err error) bool {
	return errors.Is(err, gorm.ErrRecordNotFound)
}

// IsSQLiteDB checks if the given file is a valid SQLite database by reading its signature.
func IsSQLiteDB(file io.ReaderAt) (bool, error) {
	signature := []byte("SQLite format 3\x00")
	buf := make([]byte, len(signature))
	_, err := file.ReadAt(buf, 0)
	if err != nil {
		return false, err
	}
	return bytes.Equal(buf, signature), nil
}

// Checkpoint performs a WAL checkpoint on the SQLite database to ensure data consistency.
// No-op on PostgreSQL (WAL there is managed by the server).
func Checkpoint() error {
	if IsPostgres() {
		return nil
	}
	return db.Exec("PRAGMA wal_checkpoint;").Error
}

// ValidateSQLiteDB opens the provided sqlite DB path with a throw-away connection
// and runs a PRAGMA integrity_check to ensure the file is structurally sound.
// It does not mutate global state or run migrations.
func ValidateSQLiteDB(dbPath string) error {
	if _, err := os.Stat(dbPath); err != nil { // file must exist
		return err
	}
	gdb, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		return err
	}
	sqlDB, err := gdb.DB()
	if err != nil {
		return err
	}
	defer sqlDB.Close()
	var res string
	if err := gdb.Raw("PRAGMA integrity_check;").Scan(&res).Error; err != nil {
		return err
	}
	if res != "ok" {
		return errors.New("sqlite integrity check failed: " + res)
	}
	return nil
}
