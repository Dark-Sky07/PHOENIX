package service

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/xtls/xray-core/app/proxyman/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type recordingRemovalServer struct {
	command.UnimplementedHandlerServiceServer
	requests chan *command.AlterInboundRequest
	failure  error
}

func (s *recordingRemovalServer) AlterInbound(_ context.Context, req *command.AlterInboundRequest) (*command.AlterInboundResponse, error) {
	s.requests <- req
	if s.failure != nil {
		return nil, s.failure
	}
	return &command.AlterInboundResponse{}, nil
}

func TestDetachSharedEmailRemovesOnlyTargetInboundRuntimeUser(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	email := "shared-remove@example.test"
	client := model.ClientRecord{Email: email, Enable: true}
	if err := db.Create(&client).Error; err != nil {
		t.Fatal(err)
	}
	settings := `{"clients":[{"email":"shared-remove@example.test","id":"a17e367c-2074-4d3e-aaeb-fbef5dfde7e7","enable":true}]}`
	first := model.Inbound{UserId: 1, Port: 19181, Protocol: model.VLESS, Tag: "shared-first", Settings: settings}
	second := model.Inbound{UserId: 1, Port: 19182, Protocol: model.VLESS, Tag: "shared-second", Settings: settings}
	for _, ib := range []*model.Inbound{&first, &second} {
		if err := db.Create(ib).Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}).Error; err != nil {
			t.Fatal(err)
		}
	}
	stats := xray.ClientTraffic{Email: email, Up: 123, Down: 456}
	if err := db.Create(&stats).Error; err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer()
	recorder := &recordingRemovalServer{requests: make(chan *command.AlterInboundRequest, 2)}
	command.RegisterHandlerServiceServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	old := runtime.GetManager()
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
	t.Cleanup(func() { runtime.SetManager(old) })
	needRestart, err := (&ClientService{}).DelInboundClientByEmail(&InboundService{}, first.Id, email, false)
	if err != nil {
		t.Fatal(err)
	}
	if needRestart {
		t.Fatal("successful per-user removal requested a restart")
	}
	select {
	case req := <-recorder.requests:
		if req.Tag != first.Tag {
			t.Fatalf("removed from unexpected inbound %q", req.Tag)
		}
		op, err := req.Operation.GetInstance()
		if err != nil {
			t.Fatal(err)
		}
		removed, ok := op.(*command.RemoveUserOperation)
		if !ok || removed.Email != email {
			t.Fatalf("unexpected runtime operation: %v", op)
		}
	default:
		t.Fatal("shared email was not removed from target runtime")
	}
	select {
	case req := <-recorder.requests:
		t.Fatalf("unexpected second runtime mutation: %v", req)
	default:
	}
	var saved xray.ClientTraffic
	if err := db.Where("email = ?", email).First(&saved).Error; err != nil {
		t.Fatal(err)
	}
	if saved.Up != 123 || saved.Down != 456 {
		t.Fatalf("shared traffic changed: %+v", saved)
	}
	var other model.Inbound
	if err := db.First(&other, second.Id).Error; err != nil {
		t.Fatal(err)
	}
	if other.Settings != settings {
		t.Fatal("other inbound settings changed")
	}
}

func TestBulkDetachAttemptsRemovalWhenTrafficDisabledOrMissing(t *testing.T) {
	for _, withStats := range []bool{false, true} {
		name := "missing"
		if withStats {
			name = "disabled"
		}
		t.Run(name, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			email := "bulk-remove@example.test"
			client := model.ClientRecord{Email: email, Enable: true}
			ib := model.Inbound{UserId: 1, Port: 19183, Protocol: model.VLESS, Tag: "bulk-test", Settings: `{"clients":[{"email":"bulk-remove@example.test","id":"a17e367c-2074-4d3e-aaeb-fbef5dfde7e7","enable":true}]}`}
			for _, v := range []any{&client, &ib} {
				if err := db.Create(v).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}).Error; err != nil {
				t.Fatal(err)
			}
			if withStats {
				if err := db.Create(&xray.ClientTraffic{Email: email, Enable: false}).Error; err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server := grpc.NewServer()
			recorder := &recordingRemovalServer{requests: make(chan *command.AlterInboundRequest, 2)}
			command.RegisterHandlerServiceServer(server, recorder)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			old := runtime.GetManager()
			runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
			t.Cleanup(func() { runtime.SetManager(old) })
			result := (&ClientService{}).bulkDelInboundClients(&InboundService{}, ib.Id, []string{email}, map[string]*model.ClientRecord{email: &client}, true)
			if result.needRestart || len(result.perEmailSkipped) != 0 {
				t.Fatalf("unexpected result: %+v", result)
			}
			select {
			case req := <-recorder.requests:
				if req.Tag != ib.Tag {
					t.Fatalf("unexpected inbound: %q", req.Tag)
				}
				op, err := req.Operation.GetInstance()
				if err != nil {
					t.Fatal(err)
				}
				removed, ok := op.(*command.RemoveUserOperation)
				if !ok || removed.Email != email {
					t.Fatalf("unexpected operation: %v", op)
				}
			default:
				t.Fatal("missing or disabled counters suppressed runtime removal")
			}
		})
	}
}

// A not-found reply may follow a timed-out deletion and does not prove the
// earlier sessions finished. Post-commit enforcement must request recovery.
func TestTrafficRemovalNotFoundRequestsRestart(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer()
	recorder := &recordingRemovalServer{requests: make(chan *command.AlterInboundRequest, 1), failure: status.Error(codes.NotFound, "User alice not found.")}
	command.RegisterHandlerServiceServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	old := runtime.GetManager()
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
	t.Cleanup(func() { runtime.SetManager(old) })
	b := newTrafficMutationBatch()
	b.localPlans = append(b.localPlans, trafficLocalApplyPlan{action: trafficRemoveUser, inbound: model.Inbound{Tag: "test", Protocol: model.VLESS}, email: "alice"})
	if !(&InboundService{}).applyTrafficMutationBatch(b) {
		t.Fatal("uncertain removal failed to request recovery")
	}
}

func TestBulkDetachRemovesNormalizedEmailAndPreservesRestartOnSaveFailure(t *testing.T) {
	for _, saveFailure := range []bool{false, true} {
		name := "success"
		if saveFailure {
			name = "save-failure"
		}
		t.Run(name, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			client := model.ClientRecord{Email: "bulk-detach@example.test", Enable: true}
			ib := model.Inbound{UserId: 1, Port: 19184, Protocol: model.VLESS, Tag: "bulk-detach", Settings: `{"clients":[{"email":"BULK-DETACH@example.test","id":"a17e367c-2074-4d3e-aaeb-fbef5dfde7e7","enable":true}]}`}
			for _, v := range []any{&client, &ib} {
				if err := db.Create(v).Error; err != nil {
					t.Fatal(err)
				}
			}
			if err := db.Create(&model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}).Error; err != nil {
				t.Fatal(err)
			}
			if saveFailure {
				if err := db.Exec("CREATE TRIGGER reject_detach_save BEFORE UPDATE ON inbounds BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server := grpc.NewServer()
			recorder := &recordingRemovalServer{requests: make(chan *command.AlterInboundRequest, 2)}
			command.RegisterHandlerServiceServer(server, recorder)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			old := runtime.GetManager()
			runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
			t.Cleanup(func() { runtime.SetManager(old) })
			result, restart, err := (&ClientService{}).BulkDetach(&InboundService{}, []string{client.Email}, []int{ib.Id})
			if err != nil {
				t.Fatal(err)
			}
			if saveFailure {
				if !restart || len(result.Errors) == 0 || len(result.Detached) != 0 {
					t.Fatalf("lost partial failure: restart=%v result=%+v", restart, result)
				}
			} else if restart || len(result.Errors) != 0 || len(result.Detached) != 1 {
				t.Fatalf("unexpected result: restart=%v result=%+v", restart, result)
			}
			select {
			case req := <-recorder.requests:
				op, err := req.Operation.GetInstance()
				if err != nil {
					t.Fatal(err)
				}
				removed, ok := op.(*command.RemoveUserOperation)
				if !ok || removed.Email != "BULK-DETACH@example.test" || req.Tag != ib.Tag {
					t.Fatalf("wrong runtime removal: %v", req)
				}
			default:
				t.Fatal("bulk detach skipped runtime removal for missing traffic row")
			}
		})
	}
}

func TestUpdateInboundClientDoesNotReaddAfterUncertainRemoval(t *testing.T) {
	for _, code := range []codes.Code{codes.NotFound, codes.DeadlineExceeded, codes.Unavailable, codes.OK} {
		t.Run(code.String(), func(t *testing.T) {
			setupBulkDB(t)
			original := []model.Client{{Email: "update-remove@example.test", ID: "a17e367c-2074-4d3e-aaeb-fbef5dfde7e7", Enable: true}}
			ib := mkInbound(t, 22213, model.VLESS, clientsSettings(t, original))
			svc := &ClientService{}
			if err := svc.SyncInbound(nil, ib.Id, original); err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server := grpc.NewServer()
			recorder := &recordingRemovalServer{requests: make(chan *command.AlterInboundRequest, 3)}
			if code != codes.OK {
				recorder.failure = status.Error(code, "User update-remove@example.test not found.")
			}
			command.RegisterHandlerServiceServer(server, recorder)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			old := runtime.GetManager()
			runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
			t.Cleanup(func() { runtime.SetManager(old) })
			updated := []model.Client{{Email: original[0].Email, ID: "c2a615fe-086d-4fc8-9d5f-97cda3dceee0", Enable: true}}
			restart, err := svc.UpdateInboundClient(&InboundService{}, &model.Inbound{Id: ib.Id, Settings: clientsSettings(t, updated)}, original[0].Email)
			if err != nil {
				t.Fatal(err)
			}
			if restart != (code != codes.OK) {
				t.Fatalf("restart=%v, code=%v", restart, code)
			}
			var first *command.AlterInboundRequest
			select {
			case first = <-recorder.requests:
			default:
				t.Fatal("runtime removal was not attempted")
			}
			op, err := first.Operation.GetInstance()
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := op.(*command.RemoveUserOperation); !ok {
				t.Fatalf("first operation is not removal: %v", op)
			}
			select {
			case second := <-recorder.requests:
				if code != codes.OK {
					t.Fatalf("add attempted after uncertain removal: %v", second)
				}
				op, err := second.Operation.GetInstance()
				if err != nil {
					t.Fatal(err)
				}
				if _, ok := op.(*command.AddUserOperation); !ok {
					t.Fatalf("second operation is not add: %v", op)
				}
			default:
				if code == codes.OK {
					t.Fatal("successful removal did not update runtime")
				}
			}
		})
	}
}

func TestInboundUpdateSaveFailureReportsRuntimeDivergence(t *testing.T) {
	for _, mode := range []string{"direct", "client-update", "field-update"} {
		t.Run(mode, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			original := []model.Client{{Email: "update-save@example.test", ID: "a17e367c-2074-4d3e-aaeb-fbef5dfde7e7", Enable: true}}
			settings := clientsSettings(t, original)
			ib := mkInbound(t, 22214, model.VLESS, settings)
			svc := &ClientService{}
			if err := svc.SyncInbound(nil, ib.Id, original); err != nil {
				t.Fatal(err)
			}
			if err := db.Exec("CREATE TRIGGER reject_update_save BEFORE UPDATE ON inbounds BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
				t.Fatal(err)
			}
			listener, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			server := grpc.NewServer()
			recorder := &recordingRemovalServer{requests: make(chan *command.AlterInboundRequest, 3)}
			command.RegisterHandlerServiceServer(server, recorder)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			old := runtime.GetManager()
			runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
			t.Cleanup(func() { runtime.SetManager(old) })
			updated := original[0]
			updated.ID = "c2a615fe-086d-4fc8-9d5f-97cda3dceee0"
			var restart bool
			switch mode {
			case "direct":
				restart, err = svc.UpdateInboundClient(&InboundService{}, &model.Inbound{Id: ib.Id, Settings: clientsSettings(t, []model.Client{updated})}, original[0].Email)
			case "client-update":
				rec, recErr := svc.GetRecordByEmail(nil, original[0].Email)
				if recErr != nil {
					t.Fatal(recErr)
				}
				restart, err = svc.Update(&InboundService{}, rec.Id, updated)
			case "field-update":
				restart, err = svc.ResetClientIpLimitByEmail(&InboundService{}, original[0].Email, 2)
			}
			if err == nil || !restart {
				t.Fatalf("expected error and restart after runtime mutation: restart=%v err=%v", restart, err)
			}
			if len(recorder.requests) != 2 {
				t.Fatalf("expected remove and add before failed save, got %d calls", len(recorder.requests))
			}
			var persisted model.Inbound
			if err := db.First(&persisted, ib.Id).Error; err != nil {
				t.Fatal(err)
			}
			if persisted.Settings != settings {
				t.Fatal("failed update did not preserve inbound settings")
			}
		})
	}
}

func TestInboundUpdateCommitFailureIsReported(t *testing.T) {
	setupConflictDB(t)
	db := database.GetDB()
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	// SQLite foreign key enforcement is per connection. A single connection
	// ensures the deferred constraint fails at commit, not during Save.
	sqlDB.SetMaxOpenConns(1)
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"CREATE TABLE commit_parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE commit_child (parent_id INTEGER REFERENCES commit_parent(id) DEFERRABLE INITIALLY DEFERRED)",
		"CREATE TRIGGER reject_update_commit AFTER UPDATE ON inbounds BEGIN INSERT INTO commit_child(parent_id) VALUES (99); END",
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	original := []model.Client{{Email: "update-commit@example.test", ID: "a17e367c-2074-4d3e-aaeb-fbef5dfde7e7", Enable: true}}
	ib := mkInbound(t, 22215, model.VLESS, clientsSettings(t, original))
	svc := &ClientService{}
	if err := svc.SyncInbound(nil, ib.Id, original); err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer()
	recorder := &recordingRemovalServer{requests: make(chan *command.AlterInboundRequest, 3)}
	command.RegisterHandlerServiceServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	old := runtime.GetManager()
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
	t.Cleanup(func() { runtime.SetManager(old) })
	updated := original[0]
	updated.ID = "c2a615fe-086d-4fc8-9d5f-97cda3dceee0"
	restart, err := svc.UpdateInboundClient(&InboundService{}, &model.Inbound{Id: ib.Id, Settings: clientsSettings(t, []model.Client{updated})}, original[0].Email)
	if err == nil || !strings.Contains(err.Error(), "commit inbound client update") || !restart {
		t.Fatalf("commit failure hidden or recovery lost: restart=%v err=%v", restart, err)
	}
	if len(recorder.requests) != 2 {
		t.Fatalf("expected runtime update before commit failure, got %d calls", len(recorder.requests))
	}
}
