package runtime

import (
	"context"
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
