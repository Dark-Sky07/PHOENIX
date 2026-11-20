package service

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/xray"
)

func TestLocalXrayCounterIncidentBlocksQuotaAndRemoval(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	db := database.GetDB()
	decrease := &xray.UserTrafficCounterDecrease{Email: client.Email, Direction: "downlink", Previous: 180, Observed: 12}
	if err := recordLocalXrayCounterIncident(db, decrease); err != nil {
		t.Fatal(err)
	}
	if err := recordLocalXrayCounterIncident(db, decrease); err != nil {
		t.Fatal(err)
	}
	var rows []model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ?", 0).Find(&rows).Error; err != nil || len(rows) != 1 {
		t.Fatalf("incidents=%+v err=%v", rows, err)
	}
	if rows[0].Direction != "downlink" || rows[0].PreviousDown != 180 || rows[0].ObservedDown != 12 {
		t.Fatalf("incident evidence=%+v", rows[0])
	}
	if err := checkLocalXrayCounterIncidents(db); err == nil {
		t.Fatal("local collector resumed with incident")
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("reseller allocation allowed")
	}
	if _, err := (&ClientService{}).Delete(&InboundService{}, client.Id, false); err == nil {
		t.Fatal("client removal allowed")
	}
	if err := db.Delete(&rows[0]).Error; err != nil {
		t.Fatal(err)
	}
	if err := checkLocalXrayCounterIncidents(db); err != nil {
		t.Fatal(err)
	}
}

func TestLocalXrayIncidentRetriedAfterStorageFailure(t *testing.T) {
	_, client := seedResellerRenewal(t)
	db := database.GetDB()
	state := xrayTrafficState{pendingCounterDecrease: &xray.UserTrafficCounterDecrease{Email: client.Email, Direction: "uplink", Previous: 120, Observed: 4}}
	if err := db.Migrator().DropTable(&model.ResellerNodeCounterIncident{}); err != nil {
		t.Fatal(err)
	}
	if err := state.persistPendingCounterDecrease(db); err == nil || state.pendingCounterDecrease == nil {
		t.Fatal("failed incident write was forgotten")
	}
	// Process replacement must not discard the pending evidence.
	state.observeProcess(xray.NewProcess(nil))
	if state.pendingCounterDecrease == nil || state.pendingCounterDecrease.Observed != 4 {
		t.Fatal("process replacement lost evidence")
	}
	if err := db.AutoMigrate(&model.ResellerNodeCounterIncident{}); err != nil {
		t.Fatal(err)
	}
	if err := state.persistPendingCounterDecrease(db); err == nil || state.pendingCounterDecrease != nil {
		t.Fatal("incident was not retained durably and reported")
	}
	var incident model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ? AND email = ?", 0, client.Email).First(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if incident.PreviousUp != 120 || incident.ObservedUp != 4 {
		t.Fatalf("changed evidence: %+v", incident)
	}
	if err := checkLocalXrayCounterIncidents(db); err == nil {
		t.Fatal("collector resumed before reconciliation")
	}
}

func TestPendingLocalXrayIncidentBlocksParticipatingMutations(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	xrayCounterIncidentPending.Store(true)
	t.Cleanup(func() { xrayCounterIncidentPending.Store(false) })
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("allocation bypassed pending in-memory incident")
	}
	if _, err := (&ClientService{}).Delete(&InboundService{}, client.Id, false); err == nil {
		t.Fatal("client removal bypassed pending in-memory incident")
	}
	var link model.ClientInbound
	if err := database.GetDB().Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&InboundService{}).DelInbound(link.InboundId); err == nil {
		t.Fatal("inbound removal bypassed pending in-memory incident")
	}
}

func TestUnexpectedXrayExitBlocksAllResellersWithoutGuessingUsage(t *testing.T) {
	reseller, client := seedResellerRenewal(t)
	db := database.GetDB()
	t.Cleanup(func() { xrayCoreExitPending.Store(false) })
	if err := recordUnexpectedXrayExit(db); err != nil {
		t.Fatal(err)
	}
	var incident model.ResellerNodeCounterIncident
	if err := db.Where("node_id = ? AND direction = ?", 0, "core-exited").First(&incident).Error; err != nil {
		t.Fatal(err)
	}
	if incident.Email != "" || incident.PreviousUp != 0 || incident.PreviousDown != 0 || incident.ObservedUp != 0 || incident.ObservedDown != 0 {
		t.Fatalf("exit invented client traffic: %+v", incident)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("quota accepted after unverified core exit")
	}
	if _, err := (&ClientService{}).Delete(&InboundService{}, client.Id, false); err == nil {
		t.Fatal("client removed after unverified core exit")
	}
	var link model.ClientInbound
	if err := db.Where("client_id = ?", client.Id).First(&link).Error; err != nil {
		t.Fatal(err)
	}
	if _, err := (&InboundService{}).DelInbound(link.InboundId); err == nil {
		t.Fatal("inbound removed after unverified core exit")
	}
	if err := checkLocalXrayCounterIncidents(db); err == nil {
		t.Fatal("collector resumed before reconciliation")
	}
	// A second host without the fallback folder must still see the DB marker.
	t.Setenv("XUI_DB_FOLDER", t.TempDir())
	if err := rejectUnexpectedXrayExit(db); err == nil {
		t.Fatal("DB-only exit marker did not block another host")
	}
}

func TestUnexpectedXrayExitFailsClosedIfDatabaseUnavailable(t *testing.T) {
	_, _ = seedResellerRenewal(t)
	t.Cleanup(func() { xrayCoreExitPending.Store(false) })
	if err := recordUnexpectedXrayExit(nil); err == nil {
		t.Fatal("unrecorded exit allowed restart")
	}
	if !xrayCoreExitPending.Load() {
		t.Fatal("failed DB write did not keep local barrier")
	}
	if err := diskCounterIncidentError(); err == nil {
		t.Fatal("fallback missing")
	}
}

func TestWatchdogFirstObservedXrayExitRecordsIncident(t *testing.T) {
	reseller, _ := seedResellerRenewal(t)
	db := database.GetDB()
	oldProcess, oldStopped := p, isManuallyStopped.Load()
	t.Cleanup(func() { p = oldProcess; isManuallyStopped.Store(oldStopped); xrayCoreExitPending.Store(false) })
	p = xray.NewProcess(nil)
	isManuallyStopped.Store(false)
	observed, err := (&XrayService{}).ObserveUnexpectedXrayExit()
	if err != nil || !observed {
		t.Fatalf("observation=%v err=%v", observed, err)
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ? AND direction = ?", 0, "core-exited").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("incident=%d err=%v", count, err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("first observation did not block quota")
	}
	// The same stopped core must not create multiple records on later checks.
	if observed, err := (&XrayService{}).ObserveUnexpectedXrayExit(); err != nil || !observed {
		t.Fatalf("second observation=%v err=%v", observed, err)
	}
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ? AND direction = ?", 0, "core-exited").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("duplicate incident=%d err=%v", count, err)
	}
}

func TestWatchdogDoesNotInventExitForNilOrManuallyStoppedCore(t *testing.T) {
	_, _ = seedResellerRenewal(t)
	db := database.GetDB()
	oldProcess, oldStopped := p, isManuallyStopped.Load()
	t.Cleanup(func() { p = oldProcess; isManuallyStopped.Store(oldStopped); xrayCoreExitPending.Store(false) })
	p = nil
	isManuallyStopped.Store(false)
	if observed, err := (&XrayService{}).ObserveUnexpectedXrayExit(); err != nil || !observed {
		t.Fatalf("startup observation=%v err=%v", observed, err)
	}
	p = xray.NewProcess(nil)
	isManuallyStopped.Store(true)
	if observed, err := (&XrayService{}).ObserveUnexpectedXrayExit(); err != nil || observed {
		t.Fatalf("manual stop observation=%v err=%v", observed, err)
	}
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unexpected incident=%d err=%v", count, err)
	}
}

func TestManagedXrayExitWatcherRecordsUncertaintyImmediately(t *testing.T) {
	reseller, _ := seedResellerRenewal(t)
	db := database.GetDB()
	oldProcess, oldStopped := p, isManuallyStopped.Load()
	t.Cleanup(func() { p = oldProcess; isManuallyStopped.Store(oldStopped); xrayCoreExitPending.Store(false) })
	started := xray.NewProcess(nil)
	p = started
	isManuallyStopped.Store(false)
	done := make(chan struct{})
	close(done)
	watchManagedXrayExit(started, done)
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Where("node_id = ? AND direction = ?", 0, "core-exited").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("incident=%d err=%v", count, err)
	}
	if err := (&ResellerService{}).CheckTrafficQuota(&reseller, 1); err == nil {
		t.Fatal("observed exit did not block quota")
	}
}

func TestManagedXrayExitWatcherIgnoresManualStopAndReplacedCore(t *testing.T) {
	_, _ = seedResellerRenewal(t)
	db := database.GetDB()
	oldProcess, oldStopped := p, isManuallyStopped.Load()
	t.Cleanup(func() { p = oldProcess; isManuallyStopped.Store(oldStopped); xrayCoreExitPending.Store(false) })
	started := xray.NewProcess(nil)
	done := make(chan struct{})
	close(done)
	p = started
	isManuallyStopped.Store(true)
	watchManagedXrayExit(started, done)
	isManuallyStopped.Store(false)
	p = xray.NewProcess(nil)
	watchManagedXrayExit(started, done)
	var count int64
	if err := db.Model(&model.ResellerNodeCounterIncident{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("unexpected incident=%d err=%v", count, err)
	}
}
