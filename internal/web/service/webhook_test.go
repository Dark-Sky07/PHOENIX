package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/config"
	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

type webhookTestRoundTripper func(*http.Request) (*http.Response, error)

func (f webhookTestRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func TestWebhookRejectsPrivateAndNonHTTPSDestinations(t *testing.T) {
	for _, raw := range []string{
		"http://example.com/hook",
		"https://127.0.0.1/hook",
		"https://[::1]/hook",
		"https://169.254.169.254/latest/meta-data",
		"https://10.0.0.1/hook",
		"https://localhost/hook",
		"https://user:pass@example.com/hook",
	} {
		if endpoint, err := validateWebhookURL(raw); err == nil || endpoint != nil {
			t.Fatalf("%q must be rejected as an unsafe webhook target", raw)
		}
	}
}

func TestWebhookRedactsSecretsAndEventValidation(t *testing.T) {
	data := safeWebhookData(map[string]any{"actor": "admin", "token": "secret", "password": "pw", "recoveryCode": "code"})
	if data["actor"] != "admin" || len(data) != 1 {
		t.Fatalf("redaction leaked sensitive event data: %#v", data)
	}
	if _, err := normalizeWebhookEvents([]string{"security.login.success", "audit.mutation"}); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeWebhookEvents([]string{"invalid event with spaces"}); err == nil {
		t.Fatal("unsafe event name must fail")
	}
}

func TestWebhookDeliveryRetryReplayLifecycle(t *testing.T) {
	initInitialSetupTestDB(t)

	originalFactory := webhookHTTPClientFactory
	var calls int
	var requestBody string
	var requestSignature string
	var requestEvent string
	webhookHTTPClientFactory = func() *http.Client {
		return &http.Client{Transport: webhookTestRoundTripper(func(request *http.Request) (*http.Response, error) {
			calls++
			body, err := io.ReadAll(request.Body)
			if err != nil {
				return nil, err
			}
			_ = request.Body.Close()
			requestBody = string(body)
			requestSignature = request.Header.Get("X-Phoenix-Signature")
			requestEvent = request.Header.Get("X-Phoenix-Event")
			status := http.StatusServiceUnavailable
			if calls == 2 {
				status = http.StatusNoContent
			}
			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader("")),
				Request:    request,
			}, nil
		})}
	}
	t.Cleanup(func() { webhookHTTPClientFactory = originalFactory })

	svc := WebhookService{}
	// A public numeric target passes the same production destination validation.
	created, err := svc.Create("outbox-lifecycle", "https://8.8.8.8/hooks/phase-a", []string{"audit.mutation"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.Secret == "" {
		t.Fatal("Create must return the signing secret exactly once")
	}
	listed, err := svc.List()
	if err != nil || len(listed) != 1 || listed[0].Secret != "" {
		t.Fatalf("List leaked the signing secret or returned unexpected subscriptions: %#v, %v", listed, err)
	}

	svc.Emit("audit.mutation", map[string]any{
		"actor":    "first-admin",
		"token":    "must-not-reach-outbox",
		"password": "must-not-reach-outbox",
	})
	var delivery model.WebhookDelivery
	if err := database.GetDB().First(&delivery).Error; err != nil {
		t.Fatalf("outbox delivery missing: %v", err)
	}
	if strings.Contains(delivery.Payload, "must-not-reach-outbox") {
		t.Fatalf("outbox payload leaked a sensitive value: %s", delivery.Payload)
	}

	// A delivery written before the process exits remains pending when the
	// panel reopens the same database, rather than being lost in process memory.
	if err := database.CloseDB(); err != nil {
		t.Fatalf("CloseDB before delivery restart: %v", err)
	}
	if err := database.InitDB(config.GetDBPath()); err != nil {
		t.Fatalf("InitDB after delivery restart: %v", err)
	}
	if err := database.GetDB().First(&delivery, delivery.ID).Error; err != nil || delivery.Status != "pending" {
		t.Fatalf("pending delivery did not survive restart: %#v, %v", delivery, err)
	}

	if err := svc.ProcessDue(10); err != nil {
		t.Fatalf("first ProcessDue: %v", err)
	}
	if err := database.GetDB().First(&delivery, delivery.ID).Error; err != nil {
		t.Fatal(err)
	}
	if calls != 1 || delivery.Status != "retry" || delivery.Attempts != 1 || delivery.ResponseStatus != http.StatusServiceUnavailable || !strings.Contains(delivery.LastError, "503") || delivery.NextAttemptAt <= time.Now().UnixMilli() {
		t.Fatalf("first delivery state = %#v, calls = %d; want retry with one failed attempt", delivery, calls)
	}
	if err := svc.ProcessDue(10); err != nil {
		t.Fatalf("retry must not run before its scheduled backoff: %v", err)
	}
	if calls != 1 {
		t.Fatalf("backoff ignored: calls = %d, want 1", calls)
	}

	if err := svc.Replay(delivery.ID); err != nil {
		t.Fatalf("Replay: %v", err)
	}
	if err := database.GetDB().First(&delivery, delivery.ID).Error; err != nil {
		t.Fatal(err)
	}
	if delivery.Status != "pending" || delivery.Attempts != 0 || delivery.ResponseStatus != 0 || delivery.DeliveredAt != 0 {
		t.Fatalf("replay did not reset delivery state: %#v", delivery)
	}
	if err := svc.ProcessDue(10); err != nil {
		t.Fatalf("replayed ProcessDue: %v", err)
	}
	if err := database.GetDB().First(&delivery, delivery.ID).Error; err != nil {
		t.Fatal(err)
	}
	if calls != 2 || delivery.Status != "delivered" || delivery.Attempts != 0 || delivery.ResponseStatus != http.StatusNoContent || delivery.DeliveredAt == 0 || delivery.LastError != "" {
		t.Fatalf("replayed delivery state = %#v, calls = %d; want delivered", delivery, calls)
	}
	if requestEvent != "audit.mutation" || strings.Contains(requestBody, "must-not-reach-outbox") {
		t.Fatalf("unexpected delivered event/body: event=%q body=%s", requestEvent, requestBody)
	}
	mac := hmac.New(sha256.New, []byte(created.Secret))
	_, _ = mac.Write([]byte(requestBody))
	wantSignature := "sha256=" + hex.EncodeToString(mac.Sum(nil))
	if !hmac.Equal([]byte(requestSignature), []byte(wantSignature)) {
		t.Fatalf("signature = %q, want valid HMAC-SHA256", requestSignature)
	}
}
