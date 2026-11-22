package trafficprotocol

import (
	"bytes"
	"testing"
)

func TestReadinessBindsChallengeIdentityAndSequence(t *testing.T) {
	key := bytes.Repeat([]byte{42}, 32)
	challenge, err := NewReadinessChallenge("sender", "generation", key)
	if err != nil {
		t.Fatal(err)
	}
	response, err := AnswerReadiness(challenge, key, Expected{Sender: "sender", Generation: "generation", LastSequence: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyReadiness(challenge, response, key, 5); err != nil {
		t.Fatal(err)
	}
	if err := VerifyReadiness(challenge, response, key, 3); err == nil {
		t.Fatal("receiver ahead of sender accepted")
	}
	if _, err := AnswerReadiness(challenge, key, Expected{Sender: "sender", Generation: "other"}); err == nil {
		t.Fatal("wrong generation accepted")
	}
	if _, err := AnswerReadiness(challenge, bytes.Repeat([]byte{43}, 32), Expected{Sender: "sender", Generation: "generation"}); err == nil {
		t.Fatal("wrong key accepted")
	}
	other, err := NewReadinessChallenge("sender", "generation", key)
	if err != nil {
		t.Fatal(err)
	}
	if err := VerifyReadiness(other, response, key, 5); err == nil {
		t.Fatal("replayed response accepted")
	}
	response.LastSequence = 3
	if err := VerifyReadiness(challenge, response, key, 5); err == nil {
		t.Fatal("modified sequence accepted")
	}
}

func TestReadinessCopiesRequestAndRejectsReflection(t *testing.T) {
	key := bytes.Repeat([]byte{1}, 32)
	challenge, err := NewReadinessChallenge("sender", "generation", key)
	if err != nil {
		t.Fatal(err)
	}
	response, err := AnswerReadiness(challenge, key, Expected{Sender: "sender", Generation: "generation"})
	if err != nil {
		t.Fatal(err)
	}
	response.Challenge.Nonce[0] ^= 1
	if err := VerifyReadiness(challenge, response, key, 0); err == nil {
		t.Fatal("modified nonce accepted")
	}
	response, err = AnswerReadiness(challenge, key, Expected{Sender: "sender", Generation: "generation"})
	if err != nil {
		t.Fatal(err)
	}
	response.MAC = challenge.MAC
	if err := VerifyReadiness(challenge, response, key, 0); err == nil {
		t.Fatal("request MAC reflected as response")
	}
}

func TestNodeSnapshotKeyIsSeparatedFromGlobalKey(t *testing.T) {
	token := "0123456789012345678901234567890123456789"
	global, err := KeyFromAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	node, err := NodeSnapshotKeyFromAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(global, node) {
		t.Fatal("directional keys are equal")
	}
	e, err := Sign(Envelope{Version: Version, Sender: "node", Generation: "epoch", Sequence: 1, Snapshot: []byte("snapshot")}, global)
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(e, node, Expected{Sender: "node", Generation: "epoch"}); err == nil {
		t.Fatal("global signature accepted as node snapshot")
	}
}
