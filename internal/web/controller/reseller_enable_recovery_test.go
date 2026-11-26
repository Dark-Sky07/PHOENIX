package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"github.com/gin-gonic/gin"
	"github.com/op/go-logging"
	"github.com/xtls/xray-core/app/proxyman/command"
	"google.golang.org/grpc"
)

type resellerEnableRecorder struct {
	command.UnimplementedHandlerServiceServer
	calls chan struct{}
}

func (s *resellerEnableRecorder) AlterInbound(context.Context, *command.AlterInboundRequest) (*command.AlterInboundResponse, error) {
	s.calls <- struct{}{}
	return &command.AlterInboundResponse{}, nil
}

func TestResellerDisableReportsPartialFailureAndRequestsRecovery(t *testing.T) {
	logger.InitLogger(logging.ERROR)
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dir)
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
	db := database.GetDB()
	reseller := model.Reseller{Username: "enable-test", Password: "hash", Enable: true}
	other := model.Reseller{Username: "enable-other", Password: "hash", Enable: true}
	ib := model.Inbound{Tag: "enable-shared", Port: 24691, Enable: true, Protocol: model.VLESS, Settings: `{"clients":[{"email":"enable-test@example.test","id":"a17e367c-2074-4d3e-aaeb-fbef5dfde7e7","enable":true}]}`}
	for _, v := range []any{&reseller, &other, &ib} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	client := model.Client{Email: "enable-test@example.test", ID: "a17e367c-2074-4d3e-aaeb-fbef5dfde7e7", Enable: true}
	if err := (&service.ClientService{}).SyncInbound(nil, ib.Id, []model.Client{client}); err != nil {
		t.Fatal(err)
	}
	for _, v := range []any{&model.ResellerInbound{ResellerId: reseller.Id, InboundId: ib.Id}, &model.ResellerInbound{ResellerId: other.Id, InboundId: ib.Id}, &model.ResellerClient{ResellerId: reseller.Id, Email: client.Email}, &xray.ClientTraffic{Email: client.Email, InboundId: ib.Id, Enable: true}} {
		if err := db.Create(v).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Exec("CREATE TRIGGER reject_reseller_disable BEFORE UPDATE ON inbounds BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer()
	recorder := &resellerEnableRecorder{calls: make(chan struct{}, 4)}
	command.RegisterHandlerServiceServer(server, recorder)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	old := runtime.GetManager()
	runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return listener.Addr().(*net.TCPAddr).Port }}))
	t.Cleanup(func() { runtime.SetManager(old) })
	xrayService := &service.XrayService{}
	xrayService.IsNeedRestartAndSetFalse()
	t.Cleanup(func() { xrayService.IsNeedRestartAndSetFalse() })
	r := gin.New()
	controller := &ResellerController{}
	r.POST("/resellers/:id", controller.setEnable)
	call := func() resellerWriteContract {
		w := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/resellers/%d", reseller.Id), bytes.NewBufferString(`{"enable":false}`))
		req.Header.Set("Content-Type", "application/json")
		r.ServeHTTP(w, req)
		var body resellerWriteContract
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return body
	}
	if body := call(); body.Success {
		t.Fatalf("partial failure returned success: %+v", body)
	}
	if len(recorder.calls) != 1 {
		t.Fatalf("runtime removal calls: %d", len(recorder.calls))
	}
	if !xrayService.IsNeedRestartAndSetFalse() {
		t.Fatal("restart lost on failure")
	}
	var saved model.Reseller
	if err := db.First(&saved, reseller.Id).Error; err != nil || saved.Enable {
		t.Fatalf("reseller disable not persisted: %+v %v", saved, err)
	}
	firstEpoch := saved.LoginEpoch
	if err := db.Exec("DROP TRIGGER reject_reseller_disable").Error; err != nil {
		t.Fatal(err)
	}
	if body := call(); !body.Success {
		t.Fatalf("disable retry failed: %+v", body)
	}
	if err := db.First(&saved, reseller.Id).Error; err != nil || saved.LoginEpoch != firstEpoch {
		t.Fatalf("retry changed epoch: %+v %v", saved, err)
	}
	if err := db.First(&ib, ib.Id).Error; err != nil || !ib.Enable {
		t.Fatalf("shared inbound was disabled: %+v %v", ib, err)
	}
	var traffic xray.ClientTraffic
	if err := db.Where("email = ?", client.Email).First(&traffic).Error; err != nil || traffic.Enable {
		t.Fatalf("retry failed to disable client: %+v %v", traffic, err)
	}
}
