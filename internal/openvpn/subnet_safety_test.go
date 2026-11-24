package openvpn

import (
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestLegacySubnetIDsAreUnique(t *testing.T) {
	seen := make(map[string]int, 63999)
	for id := 1; id < 64000; id++ {
		subnet, _ := (Instance{Id: id}).serverSubnet()
		if previous, ok := seen[subnet]; ok {
			t.Fatalf("IDs %d and %d share %s", previous, id, subnet)
		}
		seen[subnet] = id
	}
	// The historical clamp produces a real collision immediately above the
	// supported range; accepting either candidate would silently share a pool.
	a, _ := (Instance{Id: 63750}).serverSubnet()
	b, _ := (Instance{Id: 64000}).serverSubnet()
	if a != b {
		t.Fatal("fixture no longer demonstrates the legacy collision")
	}
}

func TestInvalidSubnetIDsRefusedBeforeSideEffects(t *testing.T) {
	tempBinFolder(t)
	firewall := newFakeFirewall()
	network := newTestNetworkManager(firewall)
	for _, id := range []int{-1, 0, 64000, 64249, 100000} {
		inst := Instance{Id: id, Port: 1194}
		if _, ok := InstanceFromInbound(&model.Inbound{Id: id, Protocol: model.OpenVPN}, nil); ok {
			t.Fatalf("accepted ID %d", id)
		}
		if err := (&Manager{procs: map[int]*managed{}}).Ensure(inst); err == nil {
			t.Fatalf("manager accepted ID %d", id)
		}
		if err := network.Apply(inst); err == nil {
			t.Fatalf("network accepted ID %d", id)
		}
		if err := writeConfig(inst, 1234); err == nil {
			t.Fatalf("config accepted ID %d", id)
		}
		if _, err := BuildProfile(inst, "alice@test", "host"); err == nil {
			t.Fatalf("profile accepted ID %d", id)
		}
		if _, err := os.Stat(dataDirForID(id)); !os.IsNotExist(err) {
			t.Fatalf("ID %d created data: %v", id, err)
		}
	}
	if len(firewall.calls) != 0 {
		t.Fatalf("invalid ID changed firewall: %v", firewall.calls)
	}
	for _, id := range []int{1, 63750, 63999} {
		if _, ok := InstanceFromInbound(&model.Inbound{Id: id, Protocol: model.OpenVPN}, nil); !ok {
			t.Fatalf("valid ID %d rejected", id)
		}
	}
}
