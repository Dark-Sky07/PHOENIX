package database

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"

	"gorm.io/driver/postgres"
	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

func TestLegacyXrayReceiptSchemaUpgradePreservesUnknownPayload(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/legacy.db"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Exec("CREATE TABLE xray_traffic_receipts (id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO xray_traffic_receipts (id) VALUES (?)", "legacy").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.XrayTrafficReceipt{}); err != nil {
		t.Fatal(err)
	}
	var receipt model.XrayTrafficReceipt
	if err := db.First(&receipt, "id = ?", "legacy").Error; err != nil || receipt.PayloadHash != "" {
		t.Fatalf("legacy receipt acquired invented payload evidence: %+v %v", receipt, err)
	}
}

func TestLegacyTuicReceiptSchemaUpgradePreservesUnknownPayload(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(t.TempDir()+"/legacy-tuic.db"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = pool.Close() })
	if err := db.Exec("CREATE TABLE tuic_traffic_receipts (id TEXT PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO tuic_traffic_receipts (id) VALUES (?)", "legacy").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.TuicTrafficReceipt{}); err != nil {
		t.Fatal(err)
	}
	var receipt model.TuicTrafficReceipt
	if err := db.First(&receipt, "id = ?", "legacy").Error; err != nil || receipt.PayloadHash != "" {
		t.Fatalf("legacy TUIC receipt acquired invented payload evidence: %+v %v", receipt, err)
	}
}

func TestMigrateData_CompositeKeyTableLargerThanBatch(t *testing.T) {
	dsn := os.Getenv("XUI_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set XUI_TEST_PG_DSN to a reachable Postgres to run this test")
	}

	// Seed a SQLite source with the full schema and >500 client_inbounds rows.
	srcPath := t.TempDir() + "/x-ui.db"
	src, err := gorm.Open(sqlite.Open(srcPath), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, m := range migrationModels() {
		if err := src.AutoMigrate(m); err != nil {
			t.Fatalf("automigrate %T: %v", m, err)
		}
	}
	const n = 600 // > batchSize (500) so the between-batches path is exercised
	links := make([]model.ClientInbound, 0, n)
	for i := 1; i <= n; i++ {
		links = append(links, model.ClientInbound{ClientId: i, InboundId: 1})
	}
	if err := src.CreateInBatches(links, 200).Error; err != nil {
		t.Fatalf("seed client_inbounds: %v", err)
	}
	if sqlDB, err := src.DB(); err == nil {
		sqlDB.Close() // flush before MigrateData reopens the file
	}

	// Make the test re-runnable: drop any tables from a previous run.
	dst, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := dst.Migrator().DropTable(migrationModels()...); err != nil {
		t.Fatalf("drop tables: %v", err)
	}

	if err := MigrateData(srcPath, dsn); err != nil {
		t.Fatalf("MigrateData: %v", err) // fails here before the fix
	}

	var got int64
	if err := dst.Model(&model.ClientInbound{}).Count(&got).Error; err != nil {
		t.Fatalf("count: %v", err)
	}
	if got != n {
		t.Fatalf("client_inbounds rows = %d, want %d", got, n)
	}
}

func TestMigrateData_PreservesFalseDefaultedColumns(t *testing.T) {
	dsn := os.Getenv("XUI_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("set XUI_TEST_PG_DSN to a reachable Postgres to run this test")
	}

	srcPath := t.TempDir() + "/x-ui.db"
	src, err := gorm.Open(sqlite.Open(srcPath), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	for _, m := range migrationModels() {
		if err := src.AutoMigrate(m); err != nil {
			t.Fatalf("automigrate %T: %v", m, err)
		}
	}

	if err := src.Create([]*model.ClientRecord{
		{Email: "on@example.com"},
		{Email: "off@example.com"},
	}).Error; err != nil {
		t.Fatalf("seed clients: %v", err)
	}
	if err := src.Model(&model.ClientRecord{}).Where("email = ?", "off@example.com").
		Update("enable", false).Error; err != nil {
		t.Fatalf("disable client: %v", err)
	}
	if err := src.Create(&model.Node{Name: "n-off", Address: "1.2.3.4", Port: 1, ApiToken: "tok"}).Error; err != nil {
		t.Fatalf("seed node: %v", err)
	}
	if err := src.Model(&model.Node{}).Where("name = ?", "n-off").
		Update("enable", false).Error; err != nil {
		t.Fatalf("disable node: %v", err)
	}
	if sqlDB, err := src.DB(); err == nil {
		sqlDB.Close()
	}

	dst, err := gorm.Open(postgres.Open(dsn), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open postgres: %v", err)
	}
	if err := dst.Migrator().DropTable(migrationModels()...); err != nil {
		t.Fatalf("drop tables: %v", err)
	}

	if err := MigrateData(srcPath, dsn); err != nil {
		t.Fatalf("MigrateData: %v", err)
	}

	var off model.ClientRecord
	if err := dst.Where("email = ?", "off@example.com").First(&off).Error; err != nil {
		t.Fatalf("load disabled client: %v", err)
	}
	if off.Enable {
		t.Fatalf("disabled client re-enabled after migration (enable=%v)", off.Enable)
	}

	var on model.ClientRecord
	if err := dst.Where("email = ?", "on@example.com").First(&on).Error; err != nil {
		t.Fatalf("load enabled client: %v", err)
	}
	if !on.Enable {
		t.Fatalf("enabled client wrongly disabled after migration")
	}

	var node model.Node
	if err := dst.Where("name = ?", "n-off").First(&node).Error; err != nil {
		t.Fatalf("load node: %v", err)
	}
	if node.Enable {
		t.Fatalf("disabled node re-enabled after migration")
	}
}

func TestInitDB_DropsLegacyResellerInboundUniqueIndex(t *testing.T) {
	dbPath := t.TempDir() + "/x-ui-legacy.db"
	raw, err := gorm.Open(sqlite.Open(dbPath), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	if err := raw.AutoMigrate(&model.ResellerInbound{}); err != nil {
		t.Fatalf("automigrate reseller_inbounds: %v", err)
	}
	if err := raw.Exec("CREATE UNIQUE INDEX ux_reseller_inbounds_inbound ON reseller_inbounds(inbound_id)").Error; err != nil {
		t.Fatalf("create legacy unique index: %v", err)
	}
	if sqlDB, err := raw.DB(); err == nil {
		sqlDB.Close()
	}

	if err := InitDB(dbPath); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() {
		_ = CloseDB()
	})
	if db.Migrator().HasIndex(&model.ResellerInbound{}, "ux_reseller_inbounds_inbound") {
		t.Fatal("legacy unique index ux_reseller_inbounds_inbound was not dropped")
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: 1, InboundId: 10}).Error; err != nil {
		t.Fatalf("first reseller inbound mapping: %v", err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: 2, InboundId: 10}).Error; err != nil {
		t.Fatalf("shared reseller inbound mapping rejected after upgrade: %v", err)
	}
}

func TestCopyTableSkipsNewModelMissingFromLegacySource(t *testing.T) {
	src, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open source sqlite: %v", err)
	}
	dst, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatalf("open destination sqlite: %v", err)
	}
	if err := dst.AutoMigrate(&model.PPPClientTrafficCounter{}); err != nil {
		t.Fatalf("migrate destination table: %v", err)
	}
	copied, err := copyTable(src, dst, &model.PPPClientTrafficCounter{})
	if err != nil {
		t.Fatalf("copy missing legacy table: %v", err)
	}
	if copied != 0 {
		t.Fatalf("copied %d rows from a nonexistent source table, want 0", copied)
	}
}

// Accounting state must survive database export/import together with the
// traffic rows. The databases here are independent SQLite files; this test does
// not assert that live writes, journals on disk, or runtimes are quiesced.
func TestCopyAllModelsPreservesTrafficProtocolEvidence(t *testing.T) {
	open := func(name string) *gorm.DB {
		d, err := gorm.Open(sqlite.Open(t.TempDir()+"/"+name+".db"), &gorm.Config{Logger: logger.Discard})
		if err != nil {
			t.Fatal(err)
		}
		pool, err := d.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pool.Close() })
		return d
	}
	src, dst := open("source"), open("destination")
	rows := []any{
		&model.ResellerNodeCounterIncident{NodeId: 123, Email: "migration@example.test", PreviousUp: 100, ObservedUp: 10},
		&model.ResellerNodeCounterIncident{NodeId: -2, Email: "", Direction: "l2tp-orphan"},
		&model.ResellerNodeCounterIncident{NodeId: -3, Email: "", Direction: "ppp-counter-decreased"},
		&model.PPPClientTrafficCounter{SessionID: "ppp-migration", Email: "migration@example.test", InboundID: 27, RxBytes: 123, TxBytes: 321},
		&model.OpenVPNSessionCounter{SessionID: "openvpn-migration", Email: "migration@example.test", InboundID: 28, RxBytes: 456, TxBytes: 654},
		&model.ClientGlobalTraffic{MasterGuid: "master", Email: "migration@example.test", Up: 77},
		&model.XrayTrafficReceipt{ID: "batch-committed", PayloadHash: "v1:test-migration-hash"},
		&model.TuicTrafficReceipt{ID: "tuic-committed", PayloadHash: "v1:tuic-migration-hash"},
		&model.XrayTrafficInFlight{ID: "batch-pending"},
		&model.TrafficSnapshotReceiver{ID: "node-inbound:123", Sender: "sender", Generation: "epoch", LastSequence: 42},
		&model.TrafficSnapshotSender{ID: "sender-token", Sender: "sender", Generation: "epoch", LastSequence: 43},
	}
	for _, row := range rows {
		if err := src.AutoMigrate(row); err != nil {
			t.Fatal(err)
		}
		if err := src.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := copyAllModels(src, dst); err != nil {
		t.Fatal(err)
	}
	var recv model.TrafficSnapshotReceiver
	if err := dst.First(&recv, "id = ?", "node-inbound:123").Error; err != nil || recv.LastSequence != 42 || recv.Generation != "epoch" {
		t.Fatalf("receiver: %+v %v", recv, err)
	}
	var sender model.TrafficSnapshotSender
	if err := dst.First(&sender, "id = ?", "sender-token").Error; err != nil || sender.LastSequence != 43 {
		t.Fatalf("sender: %+v %v", sender, err)
	}
	var ovpn model.OpenVPNSessionCounter
	if err := dst.First(&ovpn, "session_id = ?", "openvpn-migration").Error; err != nil || ovpn.RxBytes != 456 || ovpn.TxBytes != 654 {
		t.Fatalf("openvpn session: %+v %v", ovpn, err)
	}
	var global model.ClientGlobalTraffic
	if err := dst.First(&global, "master_guid = ?", "master").Error; err != nil || global.Up != 77 {
		t.Fatalf("global: %+v %v", global, err)
	}
	var receipt model.XrayTrafficReceipt
	if err := dst.First(&receipt, "id = ?", "batch-committed").Error; err != nil || receipt.PayloadHash != "v1:test-migration-hash" {
		t.Fatalf("receipt hash migration failed: %+v %v", receipt, err)
	}
	var tuicReceipt model.TuicTrafficReceipt
	if err := dst.First(&tuicReceipt, "id = ?", "tuic-committed").Error; err != nil || tuicReceipt.PayloadHash != "v1:tuic-migration-hash" {
		t.Fatalf("TUIC receipt hash migration failed: %+v %v", tuicReceipt, err)
	}
	var pending model.XrayTrafficInFlight
	if err := dst.First(&pending, "id = ?", "batch-pending").Error; err != nil {
		t.Fatal(err)
	}
	var incident model.ResellerNodeCounterIncident
	if err := dst.First(&incident, "node_id = ?", 123).Error; err != nil || incident.PreviousUp != 100 || incident.ObservedUp != 10 {
		t.Fatalf("incident: %+v %v", incident, err)
	}
	var l2tpIncident model.ResellerNodeCounterIncident
	if err := dst.First(&l2tpIncident, "node_id = ?", -2).Error; err != nil || l2tpIncident.Direction != "l2tp-orphan" {
		t.Fatalf("L2TP incident: %+v %v", l2tpIncident, err)
	}
	var pppIncident model.ResellerNodeCounterIncident
	if err := dst.First(&pppIncident, "node_id = ?", -3).Error; err != nil || pppIncident.Direction != "ppp-counter-decreased" {
		t.Fatalf("PPP incident: %+v %v", pppIncident, err)
	}
	var ppp model.PPPClientTrafficCounter
	if err := dst.First(&ppp, "session_id = ?", "ppp-migration").Error; err != nil || ppp.RxBytes != 123 || ppp.TxBytes != 321 {
		t.Fatalf("PPP checkpoint: %+v %v", ppp, err)
	}
}

func TestCopyAllModelsRejectsMissingTrafficEvidenceBeforeDestinationChanges(t *testing.T) {
	src, err := gorm.Open(sqlite.Open(t.TempDir()+"/src.db"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	dst, err := gorm.Open(sqlite.Open(t.TempDir()+"/dst.db"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range []*gorm.DB{src, dst} {
		pool, err := d.DB()
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = pool.Close() })
	}
	if err := src.AutoMigrate(&model.TrafficSnapshotReceiver{}); err != nil {
		t.Fatal(err)
	}
	if err := dst.AutoMigrate(&model.TrafficSnapshotReceiver{}); err != nil {
		t.Fatal(err)
	}
	if err := dst.Create(&model.TrafficSnapshotReceiver{ID: "existing", Sender: "existing", Generation: "g", LastSequence: 9}).Error; err != nil {
		t.Fatal(err)
	}
	if err := copyAllModels(src, dst); err == nil {
		t.Fatal("missing evidence tables accepted")
	}
	var existing model.TrafficSnapshotReceiver
	if err := dst.First(&existing, "id = ?", "existing").Error; err != nil || existing.LastSequence != 9 {
		t.Fatalf("destination changed: %+v %v", existing, err)
	}
}

func TestTrafficEvidenceMigrationPostgres(t *testing.T) {
	dsn := os.Getenv("XUI_TEST_PG_DSN")
	if dsn == "" {
		t.Skip("XUI_TEST_PG_DSN not set")
	}
	dir := t.TempDir()
	srcPath := dir + "/source.db"
	src, err := gorm.Open(sqlite.Open(srcPath), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range migrationModels() {
		if err := src.AutoMigrate(m); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []any{
		&model.TrafficSnapshotReceiver{ID: "node-inbound:313", Sender: "migration-sender", Generation: "g", LastSequence: 71},
		&model.TrafficSnapshotSender{ID: "node-sender", Sender: "migration-sender", Generation: "g", LastSequence: 72},
		&model.ResellerNodeCounterIncident{NodeId: 313, Email: "migration@test", PreviousUp: 80, ObservedUp: 10},
		&model.ResellerNodeCounterIncident{NodeId: -2, Email: "", Direction: "l2tp-orphan"},
		&model.ResellerNodeCounterIncident{NodeId: -3, Email: "", Direction: "ppp-counter-decreased"},
		&model.PPPClientTrafficCounter{SessionID: "ppp-pg-migration", Email: "migration@test", InboundID: 313, RxBytes: 123, TxBytes: 321},
		&model.OpenVPNSessionCounter{SessionID: "openvpn-pg-migration", Email: "migration@test", InboundID: 314, RxBytes: 456, TxBytes: 654},
		&model.XrayTrafficReceipt{ID: "migration-receipt"}, &model.XrayTrafficInFlight{ID: "migration-inflight"},
		&model.TuicTrafficReceipt{ID: "tuic-migration-receipt", PayloadHash: "v1:test-roundtrip"},
	} {
		if err := src.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	if pool, err := src.DB(); err == nil {
		_ = pool.Close()
	}
	if err := MigrateData(srcPath, dsn); err != nil {
		t.Fatal(err)
	}
	dstPath := dir + "/export.db"
	if err := ExportPostgresToSQLite(dsn, dstPath); err != nil {
		t.Fatal(err)
	}
	dst, err := gorm.Open(sqlite.Open(dstPath), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := dst.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	var receiver model.TrafficSnapshotReceiver
	if err := dst.First(&receiver, "id = ?", "node-inbound:313").Error; err != nil || receiver.LastSequence != 71 {
		t.Fatalf("receiver: %+v %v", receiver, err)
	}
	var sender model.TrafficSnapshotSender
	if err := dst.First(&sender, "id = ?", "node-sender").Error; err != nil || sender.LastSequence != 72 {
		t.Fatalf("sender: %+v %v", sender, err)
	}
	var receipt model.XrayTrafficReceipt
	if err := dst.First(&receipt, "id = ?", "migration-receipt").Error; err != nil {
		t.Fatal(err)
	}
	var tuicReceipt model.TuicTrafficReceipt
	if err := dst.First(&tuicReceipt, "id = ?", "tuic-migration-receipt").Error; err != nil || tuicReceipt.PayloadHash != "v1:test-roundtrip" {
		t.Fatalf("TUIC receipt: %+v %v", tuicReceipt, err)
	}
	var inflight model.XrayTrafficInFlight
	if err := dst.First(&inflight, "id = ?", "migration-inflight").Error; err != nil {
		t.Fatal(err)
	}
	var incident model.ResellerNodeCounterIncident
	if err := dst.First(&incident, "node_id = ?", 313).Error; err != nil || incident.PreviousUp != 80 || incident.ObservedUp != 10 {
		t.Fatalf("incident: %+v %v", incident, err)
	}
	var l2tpIncident model.ResellerNodeCounterIncident
	if err := dst.First(&l2tpIncident, "node_id = ?", -2).Error; err != nil || l2tpIncident.Direction != "l2tp-orphan" {
		t.Fatalf("L2TP incident: %+v %v", l2tpIncident, err)
	}
	var pppIncident model.ResellerNodeCounterIncident
	if err := dst.First(&pppIncident, "node_id = ?", -3).Error; err != nil || pppIncident.Direction != "ppp-counter-decreased" {
		t.Fatalf("PPP incident: %+v %v", pppIncident, err)
	}
	var ppp model.PPPClientTrafficCounter
	if err := dst.First(&ppp, "session_id = ?", "ppp-pg-migration").Error; err != nil || ppp.RxBytes != 123 || ppp.TxBytes != 321 {
		t.Fatalf("PPP checkpoint: %+v %v", ppp, err)
	}
	var ovpn model.OpenVPNSessionCounter
	if err := dst.First(&ovpn, "session_id = ?", "openvpn-pg-migration").Error; err != nil || ovpn.RxBytes != 456 || ovpn.TxBytes != 654 || ovpn.InboundID != 314 {
		t.Fatalf("OpenVPN checkpoint: %+v %v", ovpn, err)
	}
}
