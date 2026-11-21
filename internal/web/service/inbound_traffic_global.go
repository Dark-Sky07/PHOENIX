package service

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
	"github.com/Dark-Sky07/phoenix/internal/xray"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// AcceptGlobalTraffic ingests a master panel's aggregated per-client usage
// into client_global_traffics, keyed by (masterGuid, email). The numbers are
// display/enforcement inputs only — client_traffics keeps this panel's
// local-only counters, so the pushing master's (and any other master's)
// delta accounting over our snapshot stays correct.
//
// Unmapped clients still accept decreases. For reseller-owned clients, an
// unversioned decrease is ambiguous and blocks updates until manual review.
// Emails this panel doesn't host are dropped.
func (s *InboundService) AcceptGlobalTraffic(masterGuid string, traffics []*xray.ClientTraffic) error {
	return s.acceptGlobalTraffic(masterGuid, traffics, nil)
}

// AcceptVerifiedGlobalTraffic applies a signed snapshot and advances its
// provisioned receiver sequence in the same database transaction. The caller
// must obtain receiverID and key from trusted peer provisioning, not the
// request. This method is not yet exposed on a network endpoint; the legacy
// endpoint remains separate and is not protected by it.
func (s *InboundService) AcceptVerifiedGlobalTraffic(receiverID string, key []byte, envelope trafficprotocol.Envelope) error {
	if len(envelope.Snapshot) == 0 || len(envelope.Snapshot) > trafficprotocol.MaxSnapshotBytes {
		return errors.New("invalid traffic snapshot size")
	}
	if envelope.Sender != strings.TrimSpace(envelope.Sender) {
		return errors.New("noncanonical traffic snapshot sender")
	}
	// Prevent caller mutation between decoding and verification.
	envelope.Snapshot = append([]byte(nil), envelope.Snapshot...)
	envelope.MAC = append([]byte(nil), envelope.MAC...)
	key = append([]byte(nil), key...)
	var traffics []*xray.ClientTraffic
	if err := json.Unmarshal(envelope.Snapshot, &traffics); err != nil {
		return errors.New("invalid global traffic snapshot")
	}
	if len(traffics) == 0 {
		return errors.New("empty verified global traffic snapshot")
	}
	return s.acceptGlobalTraffic(envelope.Sender, traffics, func(db *gorm.DB, apply func(*gorm.DB) error) error {
		return trafficprotocol.ApplyVerified(db, receiverID, key, envelope, func(tx *gorm.DB, _ []byte) error {
			return apply(tx)
		})
	})
}

func (s *InboundService) acceptGlobalTraffic(masterGuid string, traffics []*xray.ClientTraffic, transact func(*gorm.DB, func(*gorm.DB) error) error) error {
	masterGuid = strings.TrimSpace(masterGuid)
	if masterGuid == "" {
		return errors.New("global traffic master identity is required")
	}
	emails := make([]string, 0, len(traffics))
	byEmail := make(map[string]*xray.ClientTraffic, len(traffics))
	for _, t := range traffics {
		if t == nil || strings.TrimSpace(t.Email) == "" {
			return errors.New("global traffic client identity is required")
		}
		if t.Up < 0 || t.Down < 0 {
			return errors.New("negative global traffic counters")
		}
		key := strings.ToLower(strings.TrimSpace(t.Email))
		if _, dup := byEmail[key]; dup {
			return errors.New("duplicate global traffic identity")
		}
		emails = append(emails, key)
		byEmail[key] = t
	}
	if len(emails) == 0 {
		return nil
	}

	// A global snapshot can change effective reseller usage. Serialize capture and commit with quota/ownership
	// mutations and with pushes in other participating panel processes. Take
	// the allocation lock before entering the writer to preserve lock order.
	unlock, err := LockResellerAllocation()
	if err != nil {
		return err
	}
	defer unlock()
	return submitTrafficWrite(func() error {
		db := database.GetDB()
		if err := retryPendingGlobalCounterIncident(db); err != nil {
			return err
		}
		if err := pendingCounterIncidentError(); err != nil {
			return err
		}
		known := make([]string, 0, len(emails))
		for _, batch := range chunkStrings(emails, sqlInChunk) {
			var page []string
			if err := db.Model(xray.ClientTraffic{}).
				Where("LOWER(TRIM(email)) IN ?", batch).
				Pluck("email", &page).Error; err != nil {
				return err
			}
			known = append(known, page...)
		}
		if len(known) == 0 && transact == nil {
			return nil
		}
		seenKnown := make(map[string]bool, len(known))
		for _, email := range known {
			key := transferEmailKey(email)
			if seenKnown[key] {
				return errors.New("ambiguous local client traffic requires reconciliation")
			}
			seenKnown[key] = true
		}

		now := time.Now().UnixMilli()
		rows := make([]model.ClientGlobalTraffic, 0, len(known))
		for _, email := range known {
			t := byEmail[strings.ToLower(strings.TrimSpace(email))]
			if t == nil {
				continue
			}
			rows = append(rows, model.ClientGlobalTraffic{
				MasterGuid: masterGuid,
				Email:      email,
				Up:         t.Up,
				Down:       t.Down,
				UpdatedAt:  now,
			})
		}

		apply := func(tx *gorm.DB) error {
			if transact == nil {
				// Once a sender has been registered for ordered snapshots, never
				// let the legacy endpoint overwrite that sender's accounting.
				// Provisioning must take the same allocation lock. Unregistered
				// senders remain legacy and are not authenticated by this guard.
				var registered int64
				if err := tx.Model(&model.TrafficSnapshotReceiver{}).Where("sender = ?", masterGuid).Count(&registered).Error; err != nil {
					return err
				}
				if registered != 0 {
					return errors.New("registered global traffic sender requires verified snapshots")
				}
			}
			// This protocol has no authenticated generation or ordering. A
			// decrease may be a reset, a stale replay, or missing traffic; it
			// cannot safely be treated as a new accounting window.
			previous := make(map[string]model.ClientGlobalTraffic, len(rows))
			keys := make([]string, 0, len(rows))
			for _, row := range rows {
				keys = append(keys, transferEmailKey(row.Email))
			}
			for _, batch := range chunkStrings(keys, sqlInChunk) {
				var oldRows []model.ClientGlobalTraffic
				if err := tx.Where("master_guid = ? AND LOWER(TRIM(email)) IN ?", masterGuid, batch).Find(&oldRows).Error; err != nil {
					return err
				}
				for _, old := range oldRows {
					if old.Up < 0 || old.Down < 0 {
						return errors.New("negative stored global traffic requires reconciliation")
					}
					key := transferEmailKey(old.Email)
					if _, exists := previous[key]; exists {
						return errors.New("ambiguous stored global traffic requires reconciliation")
					}
					previous[key] = old
				}
			}
			if err := rejectClientNodeIncidents(tx, keys); err != nil {
				return err
			}
			for i := range rows {
				row := &rows[i]
				key := transferEmailKey(row.Email)
				old, exists := previous[key]
				if exists {
					// Preserve the existing unique-index key. Creating a second row
					// for a spelling variant would leave stale usage behind.
					row.Email = old.Email
				}
				if !exists || (row.Up >= old.Up && row.Down >= old.Down) {
					continue
				}
				var mappings int64
				if err := tx.Model(&model.ResellerClient{}).Where("LOWER(TRIM(email)) = ?", key).Count(&mappings).Error; err != nil {
					return err
				}
				if mappings == 0 {
					continue
				}
				return &globalCounterDecrease{masterGuid: masterGuid, email: key, previousUp: old.Up, previousDown: old.Down, observedUp: row.Up, observedDown: row.Down}
			}
			for _, batch := range chunkGlobalRows(rows, 200) {
				if err := tx.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "master_guid"}, {Name: "email"}},
					DoUpdates: clause.AssignmentColumns([]string{"up", "down", "updated_at"}),
				}).Create(&batch).Error; err != nil {
					return err
				}
			}
			return nil
		}
		if transact == nil {
			transact = func(db *gorm.DB, apply func(*gorm.DB) error) error { return db.Transaction(apply) }
		}
		err := transact(db, apply)
		var decrease *globalCounterDecrease
		if errors.As(err, &decrease) {
			rememberGlobalCounterIncident(decrease)
			return retryPendingGlobalCounterIncident(db)
		}
		return err
	})
}

func chunkGlobalRows(rows []model.ClientGlobalTraffic, size int) [][]model.ClientGlobalTraffic {
	if len(rows) == 0 {
		return nil
	}
	out := make([][]model.ClientGlobalTraffic, 0, (len(rows)+size-1)/size)
	for start := 0; start < len(rows); start += size {
		end := min(start+size, len(rows))
		out = append(out, rows[start:end])
	}
	return out
}

// overlayGlobalTraffic raises Up/Down on the given rows to the largest global
// value any master pushed for that email. Read-path only — callers hand it
// rows about to be serialized for display; the stored counters are untouched.
func overlayGlobalTraffic(db *gorm.DB, rows []*xray.ClientTraffic) {
	if err := overlayGlobalTrafficChecked(db, rows); err != nil {
		logger.Warning("overlayGlobalTraffic:", err)
	}
}

// overlayGlobalTrafficChecked must be used by accounting/sync senders: a
// failed or partial overlay is not a valid global snapshot. Display callers
// retain the best-effort wrapper above.
func overlayGlobalTrafficChecked(db *gorm.DB, rows []*xray.ClientTraffic) error {
	if len(rows) == 0 {
		return nil
	}
	var probe int64
	if err := db.Model(&model.ClientGlobalTraffic{}).Limit(1).Count(&probe).Error; err != nil {
		return err
	}
	if probe == 0 {
		return nil
	}
	emails := make([]string, 0, len(rows))
	byEmail := make(map[string][]*xray.ClientTraffic, len(rows))
	for _, r := range rows {
		if r == nil || r.Email == "" {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(r.Email))
		if key == "" {
			continue
		}
		if _, ok := byEmail[key]; !ok {
			emails = append(emails, key)
		}
		byEmail[key] = append(byEmail[key], r)
	}
	for _, batch := range chunkStrings(emails, sqlInChunk) {
		var globals []model.ClientGlobalTraffic
		if err := db.Where("LOWER(TRIM(email)) IN ?", batch).Find(&globals).Error; err != nil {
			return err
		}
		seen := make(map[string]bool, len(globals))
		for i := range globals {
			global := &globals[i]
			if global.Up < 0 || global.Down < 0 {
				return errors.New("negative global traffic cannot be forwarded")
			}
			identity := global.MasterGuid + "\x00" + transferEmailKey(global.Email)
			if seen[identity] {
				return errors.New("ambiguous global traffic cannot be forwarded")
			}
			seen[identity] = true
			for _, r := range byEmail[transferEmailKey(global.Email)] {
				if global.Up > r.Up {
					r.Up = global.Up
				}
				if global.Down > r.Down {
					r.Down = global.Down
				}
			}
		}
	}
	return nil
}

// overlayGlobalTrafficValues is overlayGlobalTraffic for value slices.
func overlayGlobalTrafficValues(db *gorm.DB, rows []xray.ClientTraffic) {
	if len(rows) == 0 {
		return
	}
	ptrs := make([]*xray.ClientTraffic, 0, len(rows))
	for i := range rows {
		ptrs = append(ptrs, &rows[i])
	}
	overlayGlobalTraffic(db, ptrs)
}

// GetNodeClientTraffics returns this panel's aggregated traffic rows for the
// clients known to live on the given node (those with a delta baseline) —
// the payload for Remote.PushGlobalClientTraffics. The rows carry the global
// overlay so a mid-chain panel forwards the widest view it has seen, not just
// its own aggregate.
func (s *InboundService) GetNodeClientTraffics(nodeID int) ([]*xray.ClientTraffic, error) {
	db := database.GetDB()
	var rawEmails []string
	if err := db.Model(&model.NodeClientTraffic{}).
		Where("node_id = ?", nodeID).
		Pluck("email", &rawEmails).Error; err != nil {
		return nil, err
	}
	emails := make([]string, 0, len(rawEmails))
	seen := make(map[string]struct{}, len(rawEmails))
	for _, email := range rawEmails {
		if key := strings.ToLower(strings.TrimSpace(email)); key != "" {
			if _, ok := seen[key]; !ok {
				seen[key] = struct{}{}
				emails = append(emails, key)
			}
		}
	}
	if len(emails) == 0 {
		return nil, nil
	}
	out := make([]*xray.ClientTraffic, 0, len(emails))
	for _, batch := range chunkStrings(emails, sqlInChunk) {
		var page []*xray.ClientTraffic
		if err := db.Model(xray.ClientTraffic{}).Where("LOWER(TRIM(email)) IN ?", batch).Find(&page).Error; err != nil {
			return nil, err
		}
		out = append(out, page...)
	}
	seenRows := make(map[string]bool, len(out))
	for _, row := range out {
		if row == nil || transferEmailKey(row.Email) == "" || row.Up < 0 || row.Down < 0 {
			return nil, errors.New("invalid local traffic cannot be forwarded")
		}
		key := transferEmailKey(row.Email)
		if seenRows[key] {
			return nil, errors.New("ambiguous local traffic cannot be forwarded")
		}
		seenRows[key] = true
	}
	if err := overlayGlobalTrafficChecked(db, out); err != nil {
		return nil, err
	}
	return out, nil
}

// overlayInboundsClientStats applies the global overlay to every preloaded
// ClientStats row across the given inbounds. UI read paths only — never the
// full /panel/api/inbounds/list payload, which doubles as the traffic
// snapshot masters poll: overlaying that would leak pushed globals back into
// the masters' delta accounting.
func (s *InboundService) overlayInboundsClientStats(db *gorm.DB, inbounds []*model.Inbound) {
	rows := make([]*xray.ClientTraffic, 0)
	for _, ib := range inbounds {
		for j := range ib.ClientStats {
			rows = append(rows, &ib.ClientStats[j])
		}
	}
	overlayGlobalTraffic(db, rows)
}

// clearGlobalTraffic drops every master's pushed rows for the given emails.
// Used by client deletion and traffic-reset flows: after a node-local reset
// the next master push restores the master's (authoritative) numbers, and
// after a master-side reset that push carries the reset values anyway.
func clearGlobalTraffic(tx *gorm.DB, emails ...string) error {
	if len(emails) == 0 {
		return nil
	}
	keys := make([]string, 0, len(emails))
	for _, email := range emails {
		if key := strings.ToLower(strings.TrimSpace(email)); key != "" {
			keys = append(keys, key)
		}
	}
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		if err := tx.Where("LOWER(TRIM(email)) IN ?", batch).Delete(&model.ClientGlobalTraffic{}).Error; err != nil {
			return err
		}
	}
	return nil
}
