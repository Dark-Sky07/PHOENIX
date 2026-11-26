package runtime

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func TestRemoteRemovalDoesNotHideResolutionErrors(t *testing.T) {
	for _, op := range []string{"user", "inbound", "update"} {
		for _, scenario := range []string{"transport", "http", "missing", "canceled"} {
			t.Run(op+"/"+scenario, func(t *testing.T) {
				old := remoteHTTPClient
				calls := 0
				remoteHTTPClient = &http.Client{Transport: verifiedTestTransport(func(req *http.Request) (*http.Response, error) {
					calls++
					if scenario == "transport" {
						return nil, errors.New("lookup transport failed")
					}
					if scenario == "canceled" {
						return nil, context.Canceled
					}
					if scenario == "http" {
						return &http.Response{StatusCode: http.StatusUnauthorized, Body: io.NopCloser(strings.NewReader("no")), Header: make(http.Header)}, nil
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"success":true,"obj":[]}`)), Header: make(http.Header)}, nil
				})}
				t.Cleanup(func() { remoteHTTPClient = old })
				r := NewRemote(&model.Node{Id: 1, Name: "node", Scheme: "https", Address: "example.com", Port: 443, ApiToken: "token"})
				ib := &model.Inbound{Tag: "missing"}
				var err error
				switch op {
				case "user":
					err = r.DeleteUser(context.Background(), ib, "user@example.test")
				case "inbound":
					err = r.DelInbound(context.Background(), ib)
				case "update":
					err = r.UpdateInbound(context.Background(), ib, ib)
				}
				if err == nil {
					t.Fatal("unresolved remote mutation reported success")
				}
				if scenario == "canceled" && !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation lost: %v", err)
				}
				if calls != 1 {
					t.Fatalf("unexpected request count: %d", calls)
				}
			})
		}
	}
}

func TestRemoteDeleteUserPreservesRemoteFailure(t *testing.T) {
	for _, body := range []string{`{"success":false,"msg":"client not found"}`, `{"success":false,"msg":"node not found"}`, `{"success":false,"msg":"permission denied"}`} {
		t.Run(body, func(t *testing.T) {
			old := remoteHTTPClient
			remoteHTTPClient = &http.Client{Transport: verifiedTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.Method != http.MethodPost || req.URL.Path != "/panel/api/clients/user@example.test/detach" {
					t.Errorf("unexpected request: %s %s", req.Method, req.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			t.Cleanup(func() { remoteHTTPClient = old })
			r := NewRemote(&model.Node{Id: 1, Name: "node", Scheme: "https", Address: "example.com", Port: 443, ApiToken: "token"})
			r.cacheSet("tag", 42)
			if err := r.DeleteUser(context.Background(), &model.Inbound{Tag: "tag"}, "user@example.test"); err == nil {
				t.Fatal("remote failure reported as successful removal")
			}
		})
	}
}

func TestRemoteDeleteUserSuccessfulResponse(t *testing.T) {
	old := remoteHTTPClient
	remoteHTTPClient = &http.Client{Transport: verifiedTestTransport(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"success":true}`)), Header: make(http.Header)}, nil
	})}
	t.Cleanup(func() { remoteHTTPClient = old })
	r := NewRemote(&model.Node{Id: 1, Name: "node", Scheme: "https", Address: "example.com", Port: 443, ApiToken: "token"})
	r.cacheSet("tag", 42)
	if err := r.DeleteUser(context.Background(), &model.Inbound{Tag: "tag"}, "user@example.test"); err != nil {
		t.Fatal(err)
	}
}
