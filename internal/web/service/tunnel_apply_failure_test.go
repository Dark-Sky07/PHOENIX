package service

import (
	"encoding/json"
	"errors"
	"net"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
)

// Failed construction of the filtered configuration must not fall back to
// raw settings, which can re-enable clients disabled by traffic enforcement.
func TestTunnelApplyRejectsUnfilteredSettingsOnDBFailure(t *testing.T) {
	for _, protocol := range []model.Protocol{model.TUIC, model.AmneziaWG} {
		t.Run(string(protocol), func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			cert, key := testTUICCertificate(t)
			client := model.Client{Email: "filter@example.test", ID: "a0000000-0000-0000-0000-000000000001", Password: "secret", Enable: true}
			settings, err := json.Marshal(map[string]any{"certificate": cert, "private_key": key, "clients": []model.Client{client}})
			if err != nil {
				t.Fatal(err)
			}
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := pc.LocalAddr().(*net.UDPAddr).Port
			_ = pc.Close()
			ib := model.Inbound{UserId: 1, Tag: "filter-test", Listen: "127.0.0.1", Port: port, Protocol: protocol, Enable: true, Settings: string(settings)}
			if err := db.Create(&ib).Error; err != nil {
				t.Fatal(err)
			}
			var calls int
			old := runtime.GetManager()
			runtime.SetManager(runtime.NewManager(runtime.LocalDeps{SetNeedRestart: func() { calls++ }}))
			t.Cleanup(func() { runtime.SetManager(old) })
			if protocol == model.TUIC {
				inst, ok := tuic.InstanceFromInbound(&ib)
				if !ok {
					t.Fatal("invalid TUIC instance")
				}
				if err := tuic.GetManager().Ensure(inst); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { tuic.GetManager().Remove(ib.Id) })
				// Raw empty clients would stop the running server if applied.
				ib.Settings = `{"clients":[]}`
				if err := db.Save(&ib).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Exec("ALTER TABLE client_traffics RENAME TO traffic_unavailable").Error; err != nil {
				t.Fatal(err)
			}
			svc := &InboundService{}
			var applyErr error
			if protocol == model.TUIC {
				applyErr = svc.applyLocalTuic(ib.Id)
			} else {
				applyErr = svc.applyLocalAmneziaWG(ib.Id)
			}
			if applyErr == nil {
				t.Fatal("unavailable traffic filter was ignored")
			}
			if protocol == model.TUIC && !tuic.GetManager().HasRunning() {
				t.Fatal("raw TUIC settings were applied")
			}
			if protocol == model.AmneziaWG && calls != 0 {
				t.Fatal("raw AWG settings were applied")
			}
			batch := newTrafficMutationBatch()
			batch.localPlans = append(batch.localPlans, trafficLocalApplyPlan{inbound: ib, action: trafficRemoveUser})
			if !svc.applyTrafficMutationBatch(batch) {
				t.Fatal("traffic batch lost failed runtime apply")
			}
		})
	}
}

func TestInboundCreateDoesNotApplyUnfilteredTUICOnFilterError(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	cert, key := testTUICCertificate(t)
	client := model.Client{Email: "create-filter@example.test", ID: "a0000000-0000-0000-0000-000000000001", Password: "secret", Enable: true}
	settings, err := json.Marshal(map[string]any{"certificate": cert, "private_key": key, "clients": []model.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := pc.LocalAddr().(*net.UDPAddr).Port
	_ = pc.Close()
	ib := model.Inbound{UserId: 1, Tag: "create-filter", Listen: "127.0.0.1", Port: port, Protocol: model.TUIC, Enable: true, Settings: string(settings)}
	oldManager := runtime.GetManager()
	var runtimeCalls int
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{SetNeedRestart: func() { runtimeCalls++ }}))
	t.Cleanup(func() { runtime.SetManager(oldManager); tuic.GetManager().Remove(ib.Id) })
	const callback = "test:fail-create-daemon-filter"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "client_traffics" && strings.Join(tx.Statement.Selects, ",") == "id,email,enable" {
			tx.AddError(errors.New("injected daemon filter failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	if _, _, err := (&InboundService{}).AddInbound(&ib); err == nil || !strings.Contains(err.Error(), "filtered daemon settings") {
		t.Fatalf("filter failure ignored: %v", err)
	}
	if runtimeCalls != 0 || tuic.GetManager().HasRunning() {
		t.Fatal("unfiltered runtime started")
	}
	var count int64
	if err := db.Model(&model.Inbound{}).Where("tag = ?", ib.Tag).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed inbound creation committed: %d %v", count, err)
	}
}

func TestInboundEditDoesNotApplyUnfilteredTUICOnFilterError(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	cert, key := testTUICCertificate(t)
	client := model.Client{Email: "edit-filter@example.test", ID: "a0000000-0000-0000-0000-000000000001", Password: "secret", Enable: true}
	settings, err := json.Marshal(map[string]any{"certificate": cert, "private_key": key, "clients": []model.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	ib := model.Inbound{UserId: 1, Tag: "edit-filter-original", Listen: "127.0.0.1", Port: 24445, Protocol: model.TUIC, Enable: true, Settings: string(settings)}
	if err := db.Create(&ib).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{Email: client.Email, InboundId: ib.Id, Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	oldManager := runtime.GetManager()
	var runtimeCalls int
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{SetNeedRestart: func() { runtimeCalls++ }}))
	t.Cleanup(func() { runtime.SetManager(oldManager); tuic.GetManager().Remove(ib.Id) })
	const callback = "test:fail-daemon-filter"
	if err := db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "client_traffics" && strings.Join(tx.Statement.Selects, ",") == "id,email,enable" {
			tx.AddError(errors.New("injected daemon filter failure"))
		}
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Callback().Query().Remove(callback) })
	updated := ib
	updated.Tag = "edit-filter-new"
	if _, _, err := (&InboundService{}).UpdateInbound(&updated); err == nil || !strings.Contains(err.Error(), "filtered daemon settings") {
		t.Fatalf("filter failure ignored: %v", err)
	}
	if runtimeCalls != 0 {
		t.Fatalf("unfiltered runtime mutation attempted: %d", runtimeCalls)
	}
	var saved model.Inbound
	if err := db.First(&saved, ib.Id).Error; err != nil || saved.Tag != ib.Tag {
		t.Fatalf("failed edit committed: %+v %v", saved, err)
	}
}

func TestTUICClientUpdateReportsFailedApplyAfterCommit(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	client := model.Client{Email: "apply-failure@example.test", ID: "a0000000-0000-0000-0000-000000000001", Password: "secret", Enable: true}
	settings, err := json.Marshal(map[string]any{"certificate": "invalid", "private_key": "invalid", "clients": []model.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	ib := model.Inbound{UserId: 1, Tag: "failed-tuic-apply", Listen: "127.0.0.1", Port: 24444, Protocol: model.TUIC, Enable: true, Settings: string(settings)}
	if err := db.Create(&ib).Error; err != nil {
		t.Fatal(err)
	}
	svc := &ClientService{}
	if err := svc.SyncInbound(nil, ib.Id, []model.Client{client}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{InboundId: ib.Id, Email: client.Email, Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	old := runtime.GetManager()
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{}))
	t.Cleanup(func() { runtime.SetManager(old); tuic.GetManager().Remove(ib.Id) })
	client.Password = "new-secret"
	payload, err := json.Marshal(map[string]any{"clients": []model.Client{client}})
	if err != nil {
		t.Fatal(err)
	}
	restart, err := svc.UpdateInboundClient(&InboundService{}, &model.Inbound{Id: ib.Id, Settings: string(payload)}, client.Email)
	if err != nil || !restart {
		t.Fatalf("expected committed update with recovery request: restart=%v err=%v", restart, err)
	}
	var saved model.Inbound
	if err := db.First(&saved, ib.Id).Error; err != nil {
		t.Fatal(err)
	}
	clients, err := (&InboundService{}).GetClients(&saved)
	if err != nil || len(clients) != 1 || clients[0].Password != "new-secret" {
		t.Fatalf("committed state missing: %+v err=%v", clients, err)
	}
}
