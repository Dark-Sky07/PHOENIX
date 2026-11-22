package service

import (
	"context"
	"net"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/xtls/xray-core/app/proxyman/command"
	"google.golang.org/grpc"
)

type recordingRemovalServer struct {
	command.UnimplementedHandlerServiceServer
	requests chan *command.AlterInboundRequest
}

func (s *recordingRemovalServer) AlterInbound(_ context.Context, req *command.AlterInboundRequest) (*command.AlterInboundResponse, error) {
	s.requests <- req
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
