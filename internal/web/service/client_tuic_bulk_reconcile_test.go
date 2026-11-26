package service

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"math/big"
	"net"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/tuic"
	"github.com/Dark-Sky07/phoenix/internal/web/runtime"
)

func testTUICCertificate(t *testing.T) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
}

func TestBulkTUICReconciliationAfterCommit(t *testing.T) {
	for _, mode := range []string{"detach", "delete", "detach-failed-save", "delete-failed-save"} {
		t.Run(mode, func(t *testing.T) {
			setupConflictDB(t)
			db := database.GetDB()
			cert, key := testTUICCertificate(t)
			pc, err := net.ListenPacket("udp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			port := pc.LocalAddr().(*net.UDPAddr).Port
			_ = pc.Close()
			client := model.Client{Email: "bulk-tuic@example.test", ID: "a0000000-0000-0000-0000-000000000001", Password: "secret", Enable: true}
			settings, err := json.Marshal(map[string]any{"certificate": cert, "private_key": key, "clients": []model.Client{client}})
			if err != nil {
				t.Fatal(err)
			}
			ib := model.Inbound{UserId: 1, Tag: "bulk-tuic", Listen: "127.0.0.1", Port: port, Protocol: model.TUIC, Enable: true, Settings: string(settings)}
			if err := db.Create(&ib).Error; err != nil {
				t.Fatal(err)
			}
			clientSvc := &ClientService{}
			if err := clientSvc.SyncInbound(nil, ib.Id, []model.Client{client}); err != nil {
				t.Fatal(err)
			}
			rec, err := clientSvc.GetRecordByEmail(nil, client.Email)
			if err != nil {
				t.Fatal(err)
			}
			mgr := tuic.GetManager()
			inst, ok := tuic.InstanceFromInbound(&ib)
			if !ok {
				t.Fatal("invalid TUIC instance")
			}
			if err := mgr.Ensure(inst); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { mgr.Remove(ib.Id) })
			if !mgr.HasRunning() {
				t.Fatal("TUIC server did not start")
			}
			old := runtime.GetManager()
			runtime.SetManager(runtime.NewManager(runtime.LocalDeps{APIPort: func() int { return 0 }}))
			t.Cleanup(func() { runtime.SetManager(old) })
			failed := mode == "detach-failed-save" || mode == "delete-failed-save"
			if failed {
				if err := db.Exec("CREATE TRIGGER reject_tuic_bulk BEFORE UPDATE ON inbounds BEGIN SELECT RAISE(ABORT, 'injected failure'); END").Error; err != nil {
					t.Fatal(err)
				}
			}
			if mode == "detach" || mode == "detach-failed-save" {
				_, err = clientSvc.delInboundClients(&InboundService{}, ib.Id, []*model.ClientRecord{rec}, true)
				if (err != nil) != failed {
					t.Fatalf("detach error = %v", err)
				}
			} else {
				result := clientSvc.bulkDelInboundClients(&InboundService{}, ib.Id, []string{client.Email}, map[string]*model.ClientRecord{client.Email: rec}, true)
				if (len(result.perEmailSkipped) > 0) != failed {
					t.Fatalf("delete result = %+v", result)
				}
			}
			if running := mgr.HasRunning(); running != failed {
				t.Fatalf("server running = %v after failed save = %v", running, failed)
			}
		})
	}
}
