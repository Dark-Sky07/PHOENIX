package trafficprotocol

import (
	"bytes"
	"testing"
)

func TestEnvelopeAuthenticationAndOrdering(t *testing.T) {
	key := bytes.Repeat([]byte{0x19}, 32)
	original := Envelope{Version: Version, Sender: "master-a", Generation: "epoch-1", Sequence: 8, Snapshot: []byte(`{"clients":[]}`)}
	signed, err := Sign(original, key)
	if err != nil {
		t.Fatal(err)
	}
	expected := Expected{Sender: "master-a", Generation: "epoch-1", LastSequence: 7}
	if err := Verify(signed, key, expected); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name     string
		change   func(*Envelope)
		expected Expected
		key      []byte
	}{
		{"snapshot", func(e *Envelope) { e.Snapshot = []byte(`{"clients":[1]}`) }, expected, key},
		{"sender", func(e *Envelope) { e.Sender = "master-b" }, expected, key},
		{"generation", func(e *Envelope) { e.Generation = "epoch-2" }, expected, key},
		{"sequence", func(e *Envelope) { e.Sequence++ }, expected, key},
		{"version", func(e *Envelope) { e.Version++ }, expected, key},
		{"wrong key", func(e *Envelope) {}, expected, bytes.Repeat([]byte{0x20}, 32)},
		{"replay", func(e *Envelope) {}, Expected{Sender: "master-a", Generation: "epoch-1", LastSequence: 8}, key},
		{"older", func(e *Envelope) {}, Expected{Sender: "master-a", Generation: "epoch-1", LastSequence: 9}, key},
		{"unregistered sender", func(e *Envelope) {}, Expected{Sender: "master-b", Generation: "epoch-1"}, key},
		{"unregistered generation", func(e *Envelope) {}, Expected{Sender: "master-a", Generation: "epoch-2"}, key},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := signed
			tc.change(&e)
			if Verify(e, tc.key, tc.expected) == nil {
				t.Fatal("accepted invalid snapshot")
			}
		})
	}
	// Signing makes a private copy so the caller cannot alter the signed bytes.
	original.Snapshot[0] ^= 1
	if err := Verify(signed, key, expected); err != nil {
		t.Fatal(err)
	}
}

func TestEnvelopeValidation(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	base := Envelope{Version: Version, Sender: "s", Generation: "g", Sequence: 1, Snapshot: []byte("x")}
	for _, mutate := range []func(*Envelope){
		func(e *Envelope) { e.Sender = "" }, func(e *Envelope) { e.Generation = "" },
		func(e *Envelope) { e.Sequence = 0 }, func(e *Envelope) { e.Snapshot = nil },
		func(e *Envelope) { e.Snapshot = make([]byte, MaxSnapshotBytes+1) },
		func(e *Envelope) { e.Sender = string(bytes.Repeat([]byte{'a'}, maxIdentityBytes+1)) },
	} {
		e := base
		mutate(&e)
		if _, err := Sign(e, key); err == nil {
			t.Fatal("accepted invalid envelope")
		}
	}
	if _, err := Sign(base, key[:31]); err == nil {
		t.Fatal("accepted short key")
	}
}
