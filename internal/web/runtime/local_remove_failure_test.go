package runtime

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/xtls/xray-core/app/proxyman/command"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type removalFailureServer struct {
	command.UnimplementedHandlerServiceServer
	requests atomic.Int32
	code     codes.Code
}

func (s *removalFailureServer) AlterInbound(context.Context, *command.AlterInboundRequest) (*command.AlterInboundResponse, error) {
	s.requests.Add(1)
	return nil, status.Error(s.code, "User alice not found.")
}

func TestLocalRemovalFailureCannotAuthorizeReadd(t *testing.T) {
	for _, code := range []codes.Code{codes.NotFound, codes.DeadlineExceeded, codes.Unavailable} {
		t.Run(code.String(), func(t *testing.T) {
			lis, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer lis.Close()
			s := grpc.NewServer()
			handler := &removalFailureServer{code: code}
			command.RegisterHandlerServiceServer(s, handler)
			go func() { _ = s.Serve(lis) }()
			defer s.Stop()
			l := NewLocal(LocalDeps{APIPort: func() int { return lis.Addr().(*net.TCPAddr).Port }})
			ib := &model.Inbound{Tag: "test", Protocol: model.VLESS}
			if err := l.DeleteUser(context.Background(), ib, "alice"); status.Code(err) != code {
				t.Fatalf("delete error = %v", err)
			}
			if err := l.UpdateUser(context.Background(), ib, "alice", model.Client{Email: "alice", Enable: true, ID: "a17e367c-2074-4d3e-aaeb-fbef5dfde7e7"}); status.Code(err) != code {
				t.Fatalf("update error = %v", err)
			}
			if handler.requests.Load() != 2 {
				t.Fatalf("unexpected add after failed removal: %d requests", handler.requests.Load())
			}
		})
	}
}
