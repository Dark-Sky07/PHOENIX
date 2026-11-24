package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestLocalOpenVPNRuntimeNeverFallsThroughToXray(t *testing.T) {
	apiCalled := false
	local := NewLocal(LocalDeps{
		APIPort: func() int {
			apiCalled = true
			return 1
		},
	})
	oldInbound := &model.Inbound{Id: 41, Tag: "openvpn-old", Protocol: model.OpenVPN, Enable: true}
	newInbound := &model.Inbound{Id: 41, Tag: "openvpn-new", Protocol: model.OpenVPN, Enable: true}
	client := model.Client{Email: "alice@example.test", ID: "client-id", Enable: true}
	ctx := context.Background()

	if err := local.AddInbound(ctx, oldInbound); err != nil {
		t.Fatalf("AddInbound: %v", err)
	}
	if err := local.UpdateInbound(ctx, oldInbound, newInbound); err != nil {
		t.Fatalf("UpdateInbound: %v", err)
	}
	if err := local.AddUser(ctx, newInbound, map[string]any{"email": client.Email}); err != nil {
		t.Fatalf("AddUser: %v", err)
	}
	if err := local.RemoveUser(ctx, newInbound, client.Email); err != nil {
		t.Fatalf("RemoveUser: %v", err)
	}
	if err := local.AddClient(ctx, newInbound, client); err != nil {
		t.Fatalf("AddClient: %v", err)
	}
	if err := local.DelInbound(ctx, newInbound); err != nil {
		t.Fatalf("DelInbound: %v", err)
	}
	if apiCalled {
		t.Fatal("OpenVPN runtime operation unexpectedly initialized the Xray API")
	}
}

func TestOpenVPNRuntimeRejectsRetainedPID(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("XUI_BIN_FOLDER", bin)
	dir := filepath.Join(bin, "openvpn", "98765")
	if err := os.MkdirAll(dir, 0750); err != nil {
		t.Fatal(err)
	}
	pid := filepath.Join(dir, "openvpn.pid")
	if err := os.WriteFile(pid, []byte("1234\n"), 0600); err != nil {
		t.Fatal(err)
	}
	local := NewLocal(LocalDeps{})
	old := &model.Inbound{Id: 98765, Protocol: model.OpenVPN, Enable: true}
	disabled := &model.Inbound{Id: 98765, Protocol: model.OpenVPN, Enable: false}
	for name, call := range map[string]func() error{
		"delete":  func() error { return local.DelInbound(context.Background(), old) },
		"disable": func() error { return local.UpdateInbound(context.Background(), old, disabled) },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "PID evidence") {
			t.Fatalf("%s reported success: %v", name, err)
		}
		if _, err := os.Stat(pid); err != nil {
			t.Fatalf("%s removed evidence: %v", name, err)
		}
	}
}
