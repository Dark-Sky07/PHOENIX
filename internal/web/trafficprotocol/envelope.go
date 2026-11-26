// Package trafficprotocol defines the authenticated envelope for a future
// ordered traffic snapshot protocol. It does not itself provide persistence,
// key provisioning, transport security, or protection for legacy endpoints.
package trafficprotocol

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/binary"
	"errors"
	"fmt"
)

const (
	Version          = 1
	MaxSnapshotBytes = 16 << 20
	maxIdentityBytes = 256
	minKeyBytes      = 32
	domain           = "PHOENIX/traffic-snapshot/v1\x00"
)

// Envelope carries the exact snapshot bytes authenticated by MAC. Sequence is
// allocated durably by the sender, independently for each registered generation.
// Generation is an opaque installation/epoch identifier, not a timestamp.
type Envelope struct {
	Version    int    `json:"version"`
	Sender     string `json:"sender"`
	Generation string `json:"generation"`
	Sequence   uint64 `json:"sequence"`
	Snapshot   []byte `json:"snapshot"`
	MAC        []byte `json:"mac"`
}

// Expected is loaded from trusted receiver configuration and persisted state,
// not from the envelope. Generation changes require a separately authorized
// migration; accepting an arbitrary new generation would allow replay.
type Expected struct {
	Sender       string
	Generation   string
	LastSequence uint64
}

func validate(e Envelope, key []byte) error {
	if len(key) < minKeyBytes {
		return errors.New("traffic snapshot key must be at least 32 bytes")
	}
	if e.Version != Version || len(e.Sender) == 0 || len(e.Sender) > maxIdentityBytes || len(e.Generation) == 0 || len(e.Generation) > maxIdentityBytes || e.Sequence == 0 {
		return errors.New("invalid traffic snapshot envelope")
	}
	if len(e.Snapshot) == 0 || len(e.Snapshot) > MaxSnapshotBytes {
		return errors.New("invalid traffic snapshot size")
	}
	return nil
}

// digest uses explicit lengths and domain separation to avoid ambiguous fields
// and cross-protocol MAC reuse. The snapshot's exact bytes are authenticated.
func digest(e Envelope, key []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(domain))
	var b [8]byte
	for _, field := range [][]byte{[]byte(e.Sender), []byte(e.Generation)} {
		binary.BigEndian.PutUint64(b[:], uint64(len(field)))
		_, _ = m.Write(b[:])
		_, _ = m.Write(field)
	}
	binary.BigEndian.PutUint64(b[:], e.Sequence)
	_, _ = m.Write(b[:])
	binary.BigEndian.PutUint64(b[:], uint64(len(e.Snapshot)))
	_, _ = m.Write(b[:])
	_, _ = m.Write(e.Snapshot)
	return m.Sum(nil)
}

// Sign constructs a MAC. The caller must reserve the sequence durably before
// exposing the envelope; skipped sequences are safe, reuse is not.
func Sign(e Envelope, key []byte) (Envelope, error) {
	if err := validate(e, key); err != nil {
		return Envelope{}, err
	}
	e.Snapshot = append([]byte(nil), e.Snapshot...)
	e.MAC = digest(e, key)
	return e, nil
}

// Verify checks the authenticated identity and strictly increasing sequence.
// The receiver MUST lock the trusted state, verify and apply the snapshot,
// and update LastSequence atomically in one transaction. Verify alone does
// not prevent concurrent duplicate processing or a database rollback.
func Verify(e Envelope, key []byte, expected Expected) error {
	if err := validate(e, key); err != nil {
		return err
	}
	if len(e.MAC) != sha256.Size || subtle.ConstantTimeCompare(e.MAC, digest(e, key)) != 1 {
		return errors.New("invalid traffic snapshot authentication")
	}
	if e.Sender != expected.Sender || e.Generation != expected.Generation {
		return errors.New("unregistered traffic snapshot sender or generation")
	}
	if e.Sequence <= expected.LastSequence {
		return fmt.Errorf("stale or replayed traffic snapshot sequence %d", e.Sequence)
	}
	return nil
}

// KeyFromAPIToken derives the envelope key from the separately authenticated
// node API token. This is not a second factor: disclosure of that token allows
// signing and grants whatever API scope the token has. Use verified TLS, a
// distinct token per peer, explicit receiver registration and token rotation.
func KeyFromAPIToken(token string) ([]byte, error) {
	if len(token) < 32 {
		return nil, errors.New("node API token is too short")
	}
	m := hmac.New(sha256.New, []byte(token))
	_, _ = m.Write([]byte("PHOENIX/traffic-snapshot/api-token-key/v1\x00"))
	return m.Sum(nil), nil
}

// NodeSnapshotKeyFromAPIToken separates signed node-to-master snapshots from
// master-to-node global snapshots even when both use the same bearer token.
// It is not an independent credential and requires verified TLS.
func NodeSnapshotKeyFromAPIToken(token string) ([]byte, error) {
	key, err := KeyFromAPIToken(token)
	if err != nil {
		return nil, err
	}
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte("PHOENIX/node-inbound-snapshot/key/v1\x00"))
	return m.Sum(nil), nil
}
