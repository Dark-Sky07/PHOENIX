package job

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestOpenvpnDesiredRejectsPartialSnapshot(t *testing.T) {
	good := &model.Inbound{Id: 7, Protocol: model.OpenVPN, Enable: true, Settings: `{}`}
	clients := func(*model.Inbound) ([]string, bool) { return []string{"alice@test"}, true }
	relay := func(string) int { return 0 }
	for _, bad := range []*model.Inbound{
		{Id: 64000, Protocol: model.OpenVPN, Enable: true, Settings: `{}`},
		{Id: 8, Protocol: model.OpenVPN, Enable: true, Settings: `{`},
		nil,
	} {
		got, err := buildOpenvpnDesired([]*model.Inbound{good, bad}, clients, relay)
		if err == nil || got != nil {
			t.Fatalf("accepted partial desired list: %+v %v", got, err)
		}
	}
	got, err := buildOpenvpnDesired([]*model.Inbound{good}, clients, relay)
	if err != nil || len(got) != 1 || got[0].Id != 7 {
		t.Fatalf("valid desired list rejected: %+v %v", got, err)
	}
	got, err = buildOpenvpnDesired([]*model.Inbound{good}, func(*model.Inbound) ([]string, bool) { return nil, false }, relay)
	if err == nil || got != nil {
		t.Fatalf("failed client lookup accepted: %+v %v", got, err)
	}
}
