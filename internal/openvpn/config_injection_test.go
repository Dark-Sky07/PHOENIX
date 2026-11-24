package openvpn

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestInvalidConfigOperandsFailBeforeDaemonChanges(t *testing.T) {
	tempBinFolder(t)
	for _, inst := range []Instance{
		{Id: 101, Port: 1194, Listen: "127.0.0.1\nplugin /tmp/bad"},
		{Id: 102, Port: 1194, Listen: "host # comment"},
		{Id: 103, Port: 1194, PushDNS: true, DNS1: "1.1.1.1\"\nplugin /tmp/bad\n#"},
		{Id: 104, Port: 1194, PushDNS: true, DNS2: "8.8.8.8\nup /tmp/bad"},
		{Id: 105, Port: 0},
	} {
		dir := dataDirForID(inst.Id)
		if err := writeConfig(inst, 1234); err == nil {
			t.Fatalf("invalid config accepted: %+v", inst)
		}
		current := &managed{fingerprint: "old"}
		mgr := &Manager{procs: map[int]*managed{inst.Id: current}}
		if err := mgr.Ensure(inst); err == nil || mgr.procs[inst.Id] != current {
			t.Fatalf("invalid config changed daemon: %+v %v", inst, err)
		}
		if _, err := os.Stat(dir); !os.IsNotExist(err) {
			t.Fatalf("invalid config created files: %s %v", dir, err)
		}
	}
	for _, ib := range []*model.Inbound{
		{Id: 106, Protocol: model.OpenVPN, Port: 1194, Listen: "a\nplugin bad"},
		{Id: 107, Protocol: model.OpenVPN, Port: 1194, Settings: `{"pushDNS":true,"dns1":"1.1.1.1\nup bad"}`},
	} {
		if _, ok := InstanceFromInbound(ib, nil); ok {
			t.Fatalf("unsafe inbound accepted: %+v", ib)
		}
	}
}

func TestProfileHostMustBeSingleSafeOperand(t *testing.T) {
	tempBinFolder(t)
	inst := Instance{Id: 108, Port: 1194}
	for _, host := range []string{"", "host.test\nscript-security 2", "host.test #ignored", "host.test:1194", `host"\nup /tmp/bad`} {
		if p, err := BuildProfile(inst, "alice@test", host); err == nil || p != "" {
			t.Fatalf("unsafe host accepted: %q %v", host, err)
		}
		if _, err := os.Stat(dataDirForID(inst.Id)); !os.IsNotExist(err) {
			t.Fatalf("invalid host provisioned material: %v", err)
		}
	}
	for _, host := range []string{"vpn.example.test", "vpn.example.test.", "192.0.2.1", "2001:db8::1", "[2001:db8::1]"} {
		profile, err := BuildProfile(inst, "alice@test", host)
		if err != nil || !strings.Contains(profile, "remote "+host+" 1194\n") {
			t.Fatalf("valid host %q rejected: %v", host, err)
		}
	}
}

func TestConfigPathsAreQuotedAndControlsRejected(t *testing.T) {
	root := filepath.Join(t.TempDir(), `path with "quotes" and \\slashes`)
	t.Setenv("XUI_BIN_FOLDER", root)
	inst := Instance{Id: 109, Port: 1194}
	if err := writeConfig(inst, 1234); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(confPathForID(inst.Id))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ca", "cert", "key", "writepid", "log"} {
		if !strings.Contains(string(data), key+` "`) {
			t.Fatalf("unquoted %s path: %s", key, data)
		}
	}
	if !strings.Contains(string(data), `\"quotes\"`) || !strings.Contains(string(data), `\\\\slashes`) {
		t.Fatalf("path escaping missing: %s", data)
	}
	t.Setenv("XUI_BIN_FOLDER", filepath.Join(t.TempDir(), "bad\nup evil"))
	if err := writeConfig(inst, 1234); err == nil {
		t.Fatal("control character in path accepted")
	}
}
