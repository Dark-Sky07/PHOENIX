package service

import (
	"encoding/json"
	"net"
	"net/netip"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/amneziawg"
	"github.com/Dark-Sky07/phoenix/internal/amneziawgnet"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	wgutil "github.com/Dark-Sky07/phoenix/internal/util/wireguard"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
)

func TestLocalAWGClientChangesApplyCommittedPeerSet(t *testing.T) {
	for _, mode := range []string{"update", "detach", "delete", "add"} {
		t.Run(mode, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			server, err := defaultAmneziaWGServer()
			if err != nil {
				t.Fatal(err)
			}
			clients := make([]model.Client, 2)
			for i, email := range []string{"remove-awg@example.test", "keep-awg@example.test"} {
				priv, pub, err := wgutil.GenerateWireguardKeypair()
				if err != nil {
					t.Fatal(err)
				}
				clients[i] = model.Client{Email: email, Enable: true, PrivateKey: priv, PublicKey: pub, AllowedIPs: []string{[]string{"10.8.1.2/32", "10.8.1.3/32"}[i]}}
			}
			settings, err := json.Marshal(map[string]any{"server": server, "clients": clients})
			if err != nil {
				t.Fatal(err)
			}
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := pc.LocalAddr().(*net.UDPAddr).Port
			_ = pc.Close()
			ib := model.Inbound{UserId: 1, Tag: "test-awg", Port: port, Protocol: model.AmneziaWG, Enable: true, Settings: string(settings)}
			if err := db.Create(&ib).Error; err != nil {
				t.Fatal(err)
			}
			clientSvc := &ClientService{}
			if err := clientSvc.SyncInbound(nil, ib.Id, clients); err != nil {
				t.Fatal(err)
			}
			mgr := amneziawgnet.GetManager()
			inst, ok := amneziawg.InstanceFromInbound(&ib)
			if !ok {
				t.Fatal("invalid AWG instance")
			}
			if err := mgr.Ensure(amneziawgnet.Desired{Instance: inst}); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { mgr.Remove(ib.Id) })
			old := runtime.GetManager()
			runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return 0 }}))
			t.Cleanup(func() { runtime.SetManager(old) })
			_, peers, ok := mgr.Lookup(ib.Id)
			if !ok {
				t.Fatal("interface not running")
			}
			if _, found := peers.Lookup(netip.MustParseAddr("10.8.1.2")); !found {
				t.Fatal("initial peer absent")
			}
			if mode == "add" {
				priv, pub, err := wgutil.GenerateWireguardKeypair()
				if err != nil {
					t.Fatal(err)
				}
				added := model.Client{Email: "added-awg@example.test", Enable: true, PrivateKey: priv, PublicKey: pub, AllowedIPs: []string{"10.8.1.4/32"}}
				payload, err := json.Marshal(map[string]any{"clients": []model.Client{added}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := clientSvc.AddInboundClient(&InboundService{}, &model.Inbound{Id: ib.Id, Settings: string(payload)}); err != nil {
					t.Fatal(err)
				}
			} else if mode == "update" {
				updated := clients[0]
				updated.Enable = false
				payload, err := json.Marshal(map[string]any{"clients": []model.Client{updated}})
				if err != nil {
					t.Fatal(err)
				}
				if _, err := clientSvc.UpdateInboundClient(&InboundService{}, &model.Inbound{Id: ib.Id, Settings: string(payload)}, clients[0].Email); err != nil {
					t.Fatal(err)
				}
			} else {
				rec, err := clientSvc.GetRecordByEmail(nil, clients[0].Email)
				if err != nil {
					t.Fatal(err)
				}
				if mode == "detach" {
					if _, err := clientSvc.delInboundClients(&InboundService{}, ib.Id, []*model.ClientRecord{rec}, true); err != nil {
						t.Fatal(err)
					}
				} else {
					result := clientSvc.bulkDelInboundClients(&InboundService{}, ib.Id, []string{clients[0].Email}, map[string]*model.ClientRecord{clients[0].Email: rec}, true)
					if len(result.perEmailSkipped) > 0 {
						t.Fatalf("delete failed: %+v", result)
					}
				}
			}
			_, peers, ok = mgr.Lookup(ib.Id)
			if !ok {
				t.Fatal("remaining peer interface stopped")
			}
			if _, found := peers.Lookup(netip.MustParseAddr("10.8.1.2")); found != (mode == "add") {
				t.Fatalf("initial peer presence = %v, mode = %s", found, mode)
			}
			if mode == "add" {
				if _, found := peers.Lookup(netip.MustParseAddr("10.8.1.4")); !found {
					t.Fatal("new peer not applied after commit")
				}
			}
			if _, found := peers.Lookup(netip.MustParseAddr("10.8.1.3")); !found {
				t.Fatal("unrelated peer removed")
			}
		})
	}
}
