package trafficprotocol

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
)

// ReadinessChallenge is single-use: callers must create a new challenge for
// every attempt, and verify its response against the locally retained request.
// It is separate from snapshot sequences and never changes accounting state.
type ReadinessChallenge struct {
	Version    int    `json:"version"`
	Sender     string `json:"sender"`
	Generation string `json:"generation"`
	Nonce      []byte `json:"nonce"`
	MAC        []byte `json:"mac"`
}

// ReadinessResponse attests the receiver's registered stream and its current
// sequence at the instant it was read. It is not a reservation, a guarantee of
// later availability, or evidence that historical usage was reconciled.
type ReadinessResponse struct {
	Challenge    ReadinessChallenge `json:"challenge"`
	LastSequence uint64             `json:"lastSequence"`
	MAC          []byte             `json:"mac"`
}

func validReadiness(c ReadinessChallenge, key []byte) error {
	if len(key) < minKeyBytes || c.Version != Version || len(c.Sender) == 0 || len(c.Sender) > maxIdentityBytes || len(c.Generation) == 0 || len(c.Generation) > maxIdentityBytes || len(c.Nonce) != 32 {
		return errors.New("invalid traffic readiness challenge")
	}
	return nil
}

func readinessMAC(domain string, c ReadinessChallenge, sequence uint64, key []byte) []byte {
	m := hmac.New(sha256.New, key)
	_, _ = m.Write([]byte(domain))
	var b [8]byte
	for _, field := range [][]byte{[]byte(c.Sender), []byte(c.Generation), c.Nonce} {
		binary.BigEndian.PutUint64(b[:], uint64(len(field)))
		_, _ = m.Write(b[:])
		_, _ = m.Write(field)
	}
	binary.BigEndian.PutUint64(b[:], uint64(c.Version))
	_, _ = m.Write(b[:])
	binary.BigEndian.PutUint64(b[:], sequence)
	_, _ = m.Write(b[:])
	return m.Sum(nil)
}

// NewReadinessChallenge creates and signs an unpredictable challenge. Keep the
// returned value locally; never trust an echoed challenge from the network.
func NewReadinessChallenge(sender, generation string, key []byte) (ReadinessChallenge, error) {
	c := ReadinessChallenge{Version: Version, Sender: sender, Generation: generation, Nonce: make([]byte, 32)}
	if err := validReadiness(c, key); err != nil {
		return ReadinessChallenge{}, err
	}
	if _, err := io.ReadFull(rand.Reader, c.Nonce); err != nil {
		return ReadinessChallenge{}, err
	}
	c.MAC = readinessMAC("PHOENIX/traffic-readiness/request/v1\x00", c, 0, key)
	return c, nil
}

// AnswerReadiness verifies the request and its identity against independently
// provisioned receiver state. Callers must load expected from trusted storage,
// not from the request, and authenticate the transport and token separately.
func AnswerReadiness(c ReadinessChallenge, key []byte, expected Expected) (ReadinessResponse, error) {
	if err := validReadiness(c, key); err != nil {
		return ReadinessResponse{}, err
	}
	if c.Sender != expected.Sender || c.Generation != expected.Generation || expected.LastSequence > math.MaxInt64 || !hmac.Equal(c.MAC, readinessMAC("PHOENIX/traffic-readiness/request/v1\x00", c, 0, key)) {
		return ReadinessResponse{}, errors.New("unregistered or unauthenticated readiness challenge")
	}
	c.Nonce = append([]byte(nil), c.Nonce...)
	c.MAC = append([]byte(nil), c.MAC...)
	return ReadinessResponse{Challenge: c, LastSequence: expected.LastSequence, MAC: readinessMAC("PHOENIX/traffic-readiness/response/v1\x00", c, expected.LastSequence, key)}, nil
}

// VerifyReadiness accepts only the response to this locally retained challenge.
// Sender sequence must not lag the receiver: that can indicate a restored
// sender database and must be reconciled rather than silently resetting state.
func VerifyReadiness(challenge ReadinessChallenge, response ReadinessResponse, key []byte, senderSequence uint64) error {
	if err := validReadiness(challenge, key); err != nil {
		return err
	}
	if senderSequence > math.MaxInt64 || response.LastSequence > senderSequence || !hmac.Equal(challenge.MAC, readinessMAC("PHOENIX/traffic-readiness/request/v1\x00", challenge, 0, key)) || response.Challenge.Version != challenge.Version || response.Challenge.Sender != challenge.Sender || response.Challenge.Generation != challenge.Generation || !hmac.Equal(response.Challenge.Nonce, challenge.Nonce) || !hmac.Equal(response.Challenge.MAC, challenge.MAC) || !hmac.Equal(response.MAC, readinessMAC("PHOENIX/traffic-readiness/response/v1\x00", challenge, response.LastSequence, key)) {
		return errors.New("invalid, stale, or inconsistent traffic readiness response")
	}
	return nil
}
