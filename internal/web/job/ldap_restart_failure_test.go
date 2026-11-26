package job

import (
	"context"
	"net"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/xtls/xray-core/app/proxyman/command"
	"google.golang.org/grpc"
)

type ldapRemovalRecorder struct {
	command.UnimplementedHandlerServiceServer
	calls chan struct{}
}

func (s *ldapRemovalRecorder) AlterInbound(context.Context, *command.AlterInboundRequest) (*command.AlterInboundResponse, error) {
	s.calls <- struct{}{}
	return &command.AlterInboundResponse{}, nil
}

func TestLDAPBatchRequestsRestartAfterFailedUpdateWithRuntimeMutation(t *testing.T) {
	setupIntegrationDB(t)
	db := database.GetDB()
	email := "ldap-failure@example.test"
	ib := model.Inbound{Tag: "ldap-failure", Port: 21982, Protocol: model.VLESS, Enable: true, Settings: `{"clients":[{"email":"ldap-failure@example.test","id":"a17e367c-2074-4d3e-aaeb-fbef5dfde7e7","enable":true}]}`}
	if err := db.Create(&ib).Error; err != nil {
		t.Fatal(err)
	}
	client := model.Client{Email: email, ID: "a17e367c-2074-4d3e-aaeb-fbef5dfde7e7", Enable: true}
	if err := (&service.ClientService{}).SyncInbound(nil, ib.Id, []model.Client{client}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&xray.ClientTraffic{Email: email, InboundId: ib.Id, Enable: true}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TRIGGER reject_ldap_update BEFORE UPDATE ON inbounds BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer()
	recorder := &ldapRemovalRecorder{calls: make(chan struct{}, 2)}
	command.RegisterHandlerServiceServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	old := runtime.GetManager()
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
	t.Cleanup(func() { runtime.SetManager(old) })
	xrayService := &service.XrayService{}
	xrayService.IsNeedRestartAndSetFalse()
	t.Cleanup(func() { xrayService.IsNeedRestartAndSetFalse() })
	NewLdapSyncJob().batchSetEnable(&ib, []string{email}, false)
	if len(recorder.calls) != 1 {
		t.Fatalf("runtime removal calls: %d", len(recorder.calls))
	}
	if !xrayService.IsNeedRestartAndSetFalse() {
		t.Fatal("failed update lost restart request")
	}
	var persisted model.Inbound
	if err := db.First(&persisted, ib.Id).Error; err != nil || persisted.Settings != ib.Settings {
		t.Fatalf("failed update changed inbound: %v", err)
	}
}
