package service

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/common"
	"github.com/Dark-Sky07/phoenix/internal/util/random"
	"gorm.io/gorm"
)

const (
	webhookMaxAttempts = 6
	webhookMaxPayload  = 32 * 1024
)

// webhookHTTPClientFactory keeps the production delivery transport private
// while allowing deterministic, no-network lifecycle tests for the durable
// outbox. Production code never overrides it.
var webhookHTTPClientFactory = safeWebhookHTTPClient

// WebhookService implements an opt-in event outbox. It keeps delivery data in
// the database before any request is attempted, so a restart cannot silently
// lose a security/audit event.
type WebhookService struct{}

type WebhookView struct {
	ID        int      `json:"id"`
	Name      string   `json:"name"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	Enabled   bool     `json:"enabled"`
	CreatedAt int64    `json:"createdAt"`
	UpdatedAt int64    `json:"updatedAt"`
	// Secret is supplied only by Create. List responses never include it.
	Secret string `json:"secret,omitempty"`
}

type webhookEnvelope struct {
	Event      string         `json:"event"`
	OccurredAt int64          `json:"occurredAt"`
	Data       map[string]any `json:"data"`
}

func (s WebhookService) List() ([]WebhookView, error) {
	var rows []model.WebhookSubscription
	if err := database.GetDB().Order("id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]WebhookView, 0, len(rows))
	for _, row := range rows {
		out = append(out, webhookView(&row))
	}
	return out, nil
}

func (s WebhookService) Create(name, rawURL string, events []string) (*WebhookView, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return nil, common.NewError("webhook name must be 1 to 64 characters")
	}
	endpoint, err := validateWebhookURL(rawURL)
	if err != nil {
		return nil, err
	}
	events, err = normalizeWebhookEvents(events)
	if err != nil {
		return nil, err
	}
	secret := random.Seq(48)
	ciphertext, err := encryptWebhookSecret(secret)
	if err != nil {
		return nil, err
	}
	encodedEvents, err := json.Marshal(events)
	if err != nil {
		return nil, err
	}
	row := &model.WebhookSubscription{Name: name, URL: endpoint.String(), Events: string(encodedEvents), SecretEncrypted: ciphertext, Enabled: true}
	if err := database.GetDB().Create(row).Error; err != nil {
		return nil, err
	}
	view := webhookView(row)
	view.Secret = secret
	return &view, nil
}

func (s WebhookService) Delete(id int) error {
	if id <= 0 {
		return common.NewError("invalid webhook id")
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("subscription_id = ?", id).Delete(&model.WebhookDelivery{}).Error; err != nil {
			return err
		}
		result := tx.Where("id = ?", id).Delete(&model.WebhookSubscription{})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return errors.New("webhook not found")
		}
		return nil
	})
}

func (s WebhookService) SetEnabled(id int, enabled bool) error {
	if id <= 0 {
		return common.NewError("invalid webhook id")
	}
	result := database.GetDB().Model(&model.WebhookSubscription{}).Where("id = ?", id).Update("enabled", enabled)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("webhook not found")
	}
	return nil
}

func (s WebhookService) ListDeliveries(limit int) ([]model.WebhookDelivery, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	var rows []model.WebhookDelivery
	err := database.GetDB().Order("id desc").Limit(limit).Find(&rows).Error
	return rows, err
}

func (s WebhookService) Replay(deliveryID int) error {
	if deliveryID <= 0 {
		return common.NewError("invalid delivery id")
	}
	result := database.GetDB().Model(&model.WebhookDelivery{}).Where("id = ?", deliveryID).Updates(map[string]any{
		"status":          "pending",
		"attempts":        0,
		"next_attempt_at": time.Now().UnixMilli(),
		"last_error":      "",
		"response_status": 0,
		"delivered_at":    0,
	})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("delivery not found")
	}
	return nil
}

// Emit writes one outbox record per matching enabled subscription. It does no
// network I/O on the request path and serializes only explicitly supplied,
// redacted metadata.
func (s WebhookService) Emit(event string, data map[string]any) {
	event = strings.TrimSpace(strings.ToLower(event))
	if event == "" || database.GetDB() == nil {
		return
	}
	body, err := json.Marshal(webhookEnvelope{Event: event, OccurredAt: time.Now().UnixMilli(), Data: safeWebhookData(data)})
	if err != nil || len(body) > webhookMaxPayload {
		return
	}
	var subscriptions []model.WebhookSubscription
	if err := database.GetDB().Where("enabled = ?", true).Find(&subscriptions).Error; err != nil {
		return
	}
	now := time.Now().UnixMilli()
	for _, subscription := range subscriptions {
		events, err := webhookEvents(&subscription)
		if err != nil || !webhookWants(events, event) {
			continue
		}
		_ = database.GetDB().Create(&model.WebhookDelivery{SubscriptionID: subscription.ID, Event: event, Payload: string(body), Status: "pending", NextAttemptAt: now}).Error
	}
}

// ProcessDue claims and delivers a bounded outbox batch. A process crash leaves
// a short lease that is picked up in the following pass rather than losing the
// row. It is safe for more than one panel process to run this method.
func (s WebhookService) ProcessDue(limit int) error {
	if limit <= 0 || limit > 100 {
		limit = 50
	}
	db := database.GetDB()
	if db == nil {
		return nil
	}
	now := time.Now().UnixMilli()
	// Reclaim interrupted leases after two minutes.
	if err := db.Model(&model.WebhookDelivery{}).Where("status = ? AND next_attempt_at <= ?", "processing", now).Updates(map[string]any{"status": "retry", "last_error": "delivery lease expired"}).Error; err != nil {
		return err
	}
	var rows []model.WebhookDelivery
	if err := db.Where("status IN ? AND next_attempt_at <= ?", []string{"pending", "retry"}, now).Order("id asc").Limit(limit).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		leaseUntil := time.Now().Add(2 * time.Minute).UnixMilli()
		claim := db.Model(&model.WebhookDelivery{}).Where("id = ? AND status IN ? AND next_attempt_at <= ?", row.ID, []string{"pending", "retry"}, time.Now().UnixMilli()).Updates(map[string]any{"status": "processing", "next_attempt_at": leaseUntil})
		if claim.Error != nil || claim.RowsAffected != 1 {
			continue
		}
		s.deliver(row.ID)
	}
	return nil
}

func (s WebhookService) deliver(deliveryID int) {
	db := database.GetDB()
	var delivery model.WebhookDelivery
	if err := db.First(&delivery, deliveryID).Error; err != nil {
		return
	}
	var subscription model.WebhookSubscription
	if err := db.First(&subscription, delivery.SubscriptionID).Error; err != nil || !subscription.Enabled {
		s.recordDeliveryFailure(&delivery, "subscription is unavailable", 0)
		return
	}
	endpoint, err := validateWebhookURL(subscription.URL)
	if err != nil {
		s.recordDeliveryFailure(&delivery, "endpoint no longer passes outbound safety checks", 0)
		return
	}
	secret, err := decryptWebhookSecret(subscription.SecretEncrypted)
	if err != nil {
		s.recordDeliveryFailure(&delivery, "unable to decrypt signing secret", 0)
		return
	}
	request, err := http.NewRequestWithContext(context.Background(), http.MethodPost, endpoint.String(), strings.NewReader(delivery.Payload))
	if err != nil {
		s.recordDeliveryFailure(&delivery, "invalid delivery request", 0)
		return
	}
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(delivery.Payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "PHOENIX-Webhook/1")
	request.Header.Set("X-Phoenix-Event", delivery.Event)
	request.Header.Set("X-Phoenix-Signature", "sha256="+fmt.Sprintf("%x", mac.Sum(nil)))

	client := webhookHTTPClientFactory()
	response, err := client.Do(request)
	status := 0
	if response != nil {
		status = response.StatusCode
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		_ = response.Body.Close()
	}
	if err == nil && status >= 200 && status < 300 {
		_ = db.Model(&model.WebhookDelivery{}).Where("id = ?", delivery.ID).Updates(map[string]any{"status": "delivered", "response_status": status, "delivered_at": time.Now().UnixMilli(), "last_error": ""}).Error
		return
	}
	if status != 0 {
		s.recordDeliveryFailure(&delivery, "remote endpoint returned HTTP "+strconv.Itoa(status), status)
		return
	}
	s.recordDeliveryFailure(&delivery, "delivery failed", 0)
}

func (s WebhookService) recordDeliveryFailure(delivery *model.WebhookDelivery, reason string, responseStatus int) {
	attempts := delivery.Attempts + 1
	values := map[string]any{"attempts": attempts, "last_error": reason, "response_status": responseStatus}
	if attempts >= webhookMaxAttempts {
		values["status"] = "failed"
	} else {
		values["status"] = "retry"
		backoff := time.Minute * time.Duration(1<<minWebhook(attempts-1, 6))
		values["next_attempt_at"] = time.Now().Add(backoff).UnixMilli()
	}
	_ = database.GetDB().Model(&model.WebhookDelivery{}).Where("id = ?", delivery.ID).Updates(values).Error
}

func webhookView(row *model.WebhookSubscription) WebhookView {
	events, _ := webhookEvents(row)
	return WebhookView{ID: row.ID, Name: row.Name, URL: row.URL, Events: events, Enabled: row.Enabled, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func normalizeWebhookEvents(events []string) ([]string, error) {
	if len(events) == 0 || len(events) > 32 {
		return nil, common.NewError("select between 1 and 32 webhook events")
	}
	out := make([]string, 0, len(events))
	seen := map[string]struct{}{}
	for _, event := range events {
		event = strings.TrimSpace(strings.ToLower(event))
		if event == "" || len(event) > 96 || !validWebhookEvent(event) {
			return nil, common.NewError("invalid webhook event")
		}
		if _, exists := seen[event]; !exists {
			seen[event] = struct{}{}
			out = append(out, event)
		}
	}
	return out, nil
}

func validWebhookEvent(event string) bool {
	for _, r := range event {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '.' && r != '_' && r != '*' {
			return false
		}
	}
	return true
}

func webhookEvents(row *model.WebhookSubscription) ([]string, error) {
	var events []string
	if err := json.Unmarshal([]byte(row.Events), &events); err != nil {
		return nil, err
	}
	return normalizeWebhookEvents(events)
}

func webhookWants(events []string, event string) bool {
	for _, wanted := range events {
		if wanted == "*" || wanted == event {
			return true
		}
	}
	return false
}

func safeWebhookData(data map[string]any) map[string]any {
	out := make(map[string]any, len(data))
	for key, value := range data {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "password") || strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "code") || strings.Contains(lower, "credential") {
			continue
		}
		out[key] = value
	}
	return out
}

func validateWebhookURL(raw string) (*url.URL, error) {
	endpoint, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || endpoint == nil || endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil || endpoint.Fragment != "" {
		return nil, common.NewError("webhook URL must be an absolute HTTPS URL without credentials or fragment")
	}
	if endpoint.Port() != "" && endpoint.Port() != "443" {
		return nil, common.NewError("webhook URL must use HTTPS port 443")
	}
	if err := ensurePublicWebhookHost(endpoint.Hostname()); err != nil {
		return nil, err
	}
	return endpoint, nil
}

func ensurePublicWebhookHost(host string) error {
	if host == "" || strings.EqualFold(host, "localhost") {
		return common.NewError("webhook host is not publicly routable")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !isPublicWebhookIP(ip) {
			return common.NewError("webhook host is not publicly routable")
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return common.NewError("webhook host cannot be resolved")
	}
	for _, ip := range ips {
		parsed, ok := netip.AddrFromSlice(ip)
		if !ok || !isPublicWebhookIP(parsed) {
			return common.NewError("webhook host resolves to a non-public address")
		}
	}
	return nil
}

func isPublicWebhookIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsValid() && ip.IsGlobalUnicast() && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast() && !ip.IsMulticast() && !ip.IsUnspecified()
}

// safeWebhookHTTPClient re-resolves and validates the target in DialContext so
// DNS rebinding cannot convert a previously accepted hostname into an internal
// destination between subscription creation and delivery.
func safeWebhookHTTPClient() *http.Client {
	dialer := &net.Dialer{Timeout: 10 * time.Second}
	transport := &http.Transport{
		Proxy:                 nil,
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 15 * time.Second,
		IdleConnTimeout:       30 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, err
			}
			if err := ensurePublicWebhookHost(host); err != nil {
				return nil, err
			}
			ips, err := net.LookupIP(host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				parsed, ok := netip.AddrFromSlice(ip)
				if !ok || !isPublicWebhookIP(parsed) {
					return nil, errors.New("webhook target became non-public")
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("webhook target has no public address")
			}
			return nil, lastErr
		},
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func encryptWebhookSecret(secret string) (string, error) {
	key, err := webhookEncryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	return base64.RawStdEncoding.EncodeToString(append(nonce, gcm.Seal(nil, nonce, []byte(secret), nil)...)), nil
}

func decryptWebhookSecret(ciphertext string) (string, error) {
	payload, err := base64.RawStdEncoding.DecodeString(ciphertext)
	if err != nil {
		return "", err
	}
	key, err := webhookEncryptionKey()
	if err != nil {
		return "", err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil || len(payload) < gcm.NonceSize() {
		return "", errors.New("invalid encrypted webhook secret")
	}
	plain, err := gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], nil)
	return string(plain), err
}

func webhookEncryptionKey() ([]byte, error) {
	secret, err := (&SettingService{}).GetSecret()
	if err != nil || len(secret) == 0 {
		return nil, errors.New("panel secret is unavailable")
	}
	sum := sha256.Sum256(append([]byte("PHOENIX webhook secret v1\x00"), secret...))
	return sum[:], nil
}

func minWebhook(a, b int) int {
	if a < b {
		return a
	}
	return b
}
