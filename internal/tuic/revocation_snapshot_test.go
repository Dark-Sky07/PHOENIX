package tuic

import (
	"testing"
	"time"
)

func TestDelimiterCollisionCannotKeepLiveTUICSession(t *testing.T) {
	server, conn, victimID, victimPassword := startLifecycleTestServer(t, "127.0.0.1:1", "z")
	_, victim := authenticatedServerConnection(t, server, victimID)
	victim.Traffic.BytesUp.Add(13)
	const a = "a0000000-0000-0000-0000-000000000001"
	clients := []TuicClientSettings{{UUID: a, Email: "a", Password: "p"}, {UUID: victimID.String(), Email: "z", Password: victimPassword}}
	server.UpdateUsers(clients)
	inst := Instance{Id: 1, Tag: "collision", Clients: clients}
	m := &Manager{servers: map[int]*managed{1: {server: server, structuralFP: inst.StructuralFingerprint(), usersFP: inst.UsersFingerprint()}}, lastStartErr: map[int]string{}}
	t.Cleanup(m.StopAll)
	inst.Clients = []TuicClientSettings{{UUID: a, Email: "a", Password: "p|0:z=" + victimID.String() + ":" + victimPassword}}
	if err := m.Ensure(inst); err != nil {
		t.Fatal(err)
	}
	select {
	case <-conn.Context().Done():
	case <-time.After(time.Second):
		t.Fatal("removed user's QUIC connection still active")
	}
	deadline := time.Now().Add(time.Second)
	for victim.sessions.Load() > 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	deltas := server.CollectClientTraffic()
	if len(deltas) != 1 || deltas[0].Email != "z" || deltas[0].Up != 13 {
		t.Fatalf("retired traffic lost: %+v", deltas)
	}
}

func TestAudit3ActualSessionRevokedAndSnapshotDrain(t *testing.T) {
	for _, operation := range []string{"remove", "password", "email", "uuid"} {
		t.Run(operation, func(t *testing.T) {
			server, conn, id, password := startLifecycleTestServer(t, "127.0.0.1:1", "old@audit3")
			_, user := authenticatedServerConnection(t, server, id)
			user.Traffic.BytesUp.Add(123)
			user.Traffic.BytesDown.Add(456)
			client := TuicClientSettings{UUID: id.String(), Password: password, Email: "old@audit3"}
			switch operation {
			case "password":
				client.Password = "rotated"
			case "email":
				client.Email = "new@audit3"
			case "uuid":
				client.UUID = "10000000-0000-0000-0000-000000000001"
			}
			if operation == "remove" {
				server.UpdateUsers(nil)
			} else {
				server.UpdateUsers([]TuicClientSettings{client})
			}
			select {
			case <-conn.Context().Done():
			case <-time.After(time.Second):
				t.Fatal("revoked QUIC remains connected")
			}
			deadline := time.Now().Add(time.Second)
			for user.sessions.Load() > 0 && time.Now().Before(deadline) {
				time.Sleep(time.Millisecond)
			}
			deltas := server.CollectClientTraffic()
			if len(deltas) != 1 || deltas[0].Email != "old@audit3" || deltas[0].UUID != id.String() || deltas[0].Up != 123 || deltas[0].Down != 456 {
				t.Fatalf("retired snapshot=%+v", deltas)
			}
			if again := server.CollectClientTraffic(); len(again) != 0 {
				t.Fatalf("double drain=%+v", again)
			}
		})
	}
}
