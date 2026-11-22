package runtime

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
)

type verifiedTestTransport func(*http.Request) (*http.Response, error)

func (f verifiedTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func TestVerifiedTrafficSenderNeverFallsBack(t *testing.T) {
	token := "0123456789012345678901234567890123456789"
	key, err := trafficprotocol.KeyFromAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := trafficprotocol.Sign(trafficprotocol.Envelope{Version: trafficprotocol.Version, Sender: "master", Generation: "g", Sequence: 1, Snapshot: []byte("[]")}, key)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	old := remoteHTTPClient
	remoteHTTPClient = &http.Client{Transport: verifiedTestTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/panel/api/inbounds/pushVerifiedClientTraffics" || req.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("unexpected request %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { remoteHTTPClient = old })
	r := NewRemote(&model.Node{Scheme: "https", Address: "example.com", Port: 443, ApiToken: token, TlsVerifyMode: "verify"})
	if err := r.PushVerifiedGlobalClientTraffics(context.Background(), &envelope); err == nil {
		t.Fatal("unsupported receiver accepted")
	}
	if calls != 1 {
		t.Fatalf("expected one verified request and no fallback, got %d", calls)
	}
	r.node.Scheme = "http"
	if err := r.PushVerifiedGlobalClientTraffics(context.Background(), &envelope); err == nil || calls != 1 {
		t.Fatal("insecure HTTP was used")
	}
	r.node.Scheme = "https"
	r.node.TlsVerifyMode = "skip"
	if err := r.PushVerifiedGlobalClientTraffics(context.Background(), &envelope); err == nil || calls != 1 {
		t.Fatal("unverified TLS was used")
	}
}

func TestTrafficReadinessVerifiesRemoteProofAndRequiresTLS(t *testing.T) {
	token := "0123456789012345678901234567890123456789"
	key, err := trafficprotocol.KeyFromAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	old := remoteHTTPClient
	remoteHTTPClient = &http.Client{Transport: verifiedTestTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/panel/api/inbounds/trafficReadiness" || req.Header.Get("Authorization") != "Bearer "+token {
			t.Errorf("unexpected request %s", req.URL.Path)
		}
		var challenge trafficprotocol.ReadinessChallenge
		if err := json.NewDecoder(req.Body).Decode(&challenge); err != nil {
			t.Fatal(err)
		}
		proof, err := trafficprotocol.AnswerReadiness(challenge, key, trafficprotocol.Expected{Sender: "master", Generation: "epoch", LastSequence: 3})
		if err != nil {
			t.Fatal(err)
		}
		body, err := json.Marshal(map[string]any{"success": true, "obj": proof})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { remoteHTTPClient = old })
	r := NewRemote(&model.Node{Scheme: "https", Address: "example.com", Port: 443, ApiToken: token, TlsVerifyMode: "verify"})
	if err := r.CheckTrafficReadiness(context.Background(), "master", "epoch", 3); err != nil {
		t.Fatal(err)
	}
	if err := r.CheckTrafficReadiness(context.Background(), "master", "epoch", 2); err == nil {
		t.Fatal("receiver ahead accepted")
	}
	r.node.TlsVerifyMode = "skip"
	if err := r.CheckTrafficReadiness(context.Background(), "master", "epoch", 3); err == nil || calls != 2 {
		t.Fatal("insecure TLS used")
	}
}

func TestTrafficReadinessRejectsRedirectAndOversizedResponse(t *testing.T) {
	old := remoteHTTPClient
	calls := 0
	redirect := true
	remoteHTTPClient = &http.Client{Transport: verifiedTestTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if redirect {
			return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://example.com/other"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(strings.Repeat("x", 8193)))}, nil
	})}
	t.Cleanup(func() { remoteHTTPClient = old })
	r := NewRemote(&model.Node{Scheme: "https", Address: "example.com", Port: 443, ApiToken: "0123456789012345678901234567890123456789"})
	if err := r.CheckTrafficReadiness(context.Background(), "master", "epoch", 0); err == nil || calls != 1 {
		t.Fatal("redirect followed or accepted")
	}
	redirect = false
	if err := r.CheckTrafficReadiness(context.Background(), "master", "epoch", 0); err == nil || calls != 2 {
		t.Fatal("oversized response accepted")
	}
}

func TestVerifiedNodeFetchDoesNotFallbackOrFollowRedirect(t *testing.T) {
	old := remoteHTTPClient
	calls := 0
	remoteHTTPClient = &http.Client{Transport: verifiedTestTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		if req.URL.Path != "/panel/api/inbounds/verifiedNodeSnapshot" {
			t.Errorf("unexpected endpoint %s", req.URL.Path)
		}
		return &http.Response{StatusCode: http.StatusFound, Header: http.Header{"Location": []string{"http://example.com/legacy"}}, Body: io.NopCloser(strings.NewReader(""))}, nil
	})}
	t.Cleanup(func() { remoteHTTPClient = old })
	r := NewRemote(&model.Node{Scheme: "https", Address: "example.com", Port: 443, ApiToken: "0123456789012345678901234567890123456789"})
	if _, err := r.FetchVerifiedNodeSnapshot(context.Background()); err == nil || calls != 1 {
		t.Fatalf("redirect or fallback: calls %d err %v", calls, err)
	}
	r.node.Scheme = "http"
	if _, err := r.FetchVerifiedNodeSnapshot(context.Background()); err == nil || calls != 1 {
		t.Fatal("insecure transport accepted")
	}
}

func TestVerifiedNodeSnapshotBindsFreshChallenge(t *testing.T) {
	old := remoteHTTPClient
	token := "0123456789012345678901234567890123456789"
	key, err := trafficprotocol.NodeSnapshotKeyFromAPIToken(token)
	if err != nil {
		t.Fatal(err)
	}
	var saved trafficprotocol.Envelope
	calls := 0
	remoteHTTPClient = &http.Client{Transport: verifiedTestTransport(func(req *http.Request) (*http.Response, error) {
		calls++
		var request struct {
			Challenge []byte `json:"challenge"`
		}
		if err := json.NewDecoder(req.Body).Decode(&request); err != nil || len(request.Challenge) != 32 {
			t.Fatalf("bad challenge: %+v %v", request, err)
		}
		if calls == 1 {
			payload, err := json.Marshal(TrafficSnapshot{Challenge: request.Challenge, Inbounds: []*model.Inbound{}})
			if err != nil {
				t.Fatal(err)
			}
			saved, err = trafficprotocol.Sign(trafficprotocol.Envelope{Version: trafficprotocol.Version, Sender: "node", Generation: "epoch", Sequence: 1, Snapshot: payload}, key)
			if err != nil {
				t.Fatal(err)
			}
		}
		body, err := json.Marshal(map[string]any{"success": true, "obj": saved})
		if err != nil {
			t.Fatal(err)
		}
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(string(body)))}, nil
	})}
	t.Cleanup(func() { remoteHTTPClient = old })
	r := NewRemote(&model.Node{Scheme: "https", Address: "example.com", Port: 443, ApiToken: token})
	env, err := r.FetchVerifiedNodeSnapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := trafficprotocol.Verify(*env, key, trafficprotocol.Expected{Sender: "node", Generation: "epoch"}); err != nil {
		t.Fatal(err)
	}
	if _, err := r.FetchVerifiedNodeSnapshot(context.Background()); err == nil {
		t.Fatal("old response to different challenge accepted")
	}
}
