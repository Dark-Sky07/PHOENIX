package service

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/common"
	"golang.org/x/crypto/acme"
)

const dnsProfileSecretPurpose = "dns-certificate-provider-v1"

type DNSCertificateService struct {
	DirectoryURL string
	// DNSLookupTXT allows deterministic tests without public DNS.
	DNSLookupTXT func(context.Context, string) ([]string, error)
}

type DNSCertificateProfileInput struct {
	Name          string   `json:"name" form:"name"`
	Domains       []string `json:"domains" form:"domains"`
	Provider      string   `json:"provider" form:"provider"` // cloudflare or manual
	ProviderToken string   `json:"providerToken" form:"providerToken"`
	Email         string   `json:"email" form:"email"`
	ApplyToPanel  bool     `json:"applyToPanel" form:"applyToPanel"`
	Enabled       bool     `json:"enabled" form:"enabled"`
}

type DNSCertificateProfileView struct {
	ID               int      `json:"id"`
	Name             string   `json:"name"`
	Domains          []string `json:"domains"`
	Provider         string   `json:"provider"`
	HasProviderToken bool     `json:"hasProviderToken"`
	Email            string   `json:"email"`
	ApplyToPanel     bool     `json:"applyToPanel"`
	Enabled          bool     `json:"enabled"`
	LastIssuedAt     int64    `json:"lastIssuedAt"`
	LastError        string   `json:"lastError,omitempty"`
	PendingAt        int64    `json:"pendingAt,omitempty"`
	CreatedAt        int64    `json:"createdAt"`
	UpdatedAt        int64    `json:"updatedAt"`
}

type DNSChallenge struct {
	Record string `json:"record"`
	Value  string `json:"value"`
}

type DNSIssueResult struct {
	Status      string         `json:"status"` // issued or pending_dns
	Certificate *CertStatus    `json:"certificate,omitempty"`
	Challenges  []DNSChallenge `json:"challenges,omitempty"`
}

type pendingDNSChallenge struct {
	AuthorizationURL string `json:"authorizationUrl"`
	ChallengeURL     string `json:"challengeUrl"`
	Record           string `json:"record"`
	Value            string `json:"value"`
}

func (s DNSCertificateService) List() ([]DNSCertificateProfileView, error) {
	var rows []model.DNSCertificateProfile
	if err := database.GetDB().Order("name asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]DNSCertificateProfileView, 0, len(rows))
	for i := range rows {
		out = append(out, dnsCertificateProfileView(&rows[i]))
	}
	return out, nil
}

func (s DNSCertificateService) Create(in DNSCertificateProfileInput) (*DNSCertificateProfileView, error) {
	row, err := normalizeDNSCertificateProfile(in, nil)
	if err != nil {
		return nil, err
	}
	if row.ApplyToPanel {
		if err := ensureDNSPanelProfileAvailable(0); err != nil {
			return nil, err
		}
	}
	if err := database.GetDB().Create(row).Error; err != nil {
		return nil, err
	}
	view := dnsCertificateProfileView(row)
	return &view, nil
}

func (s DNSCertificateService) Update(id int, in DNSCertificateProfileInput) (*DNSCertificateProfileView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid DNS certificate profile id")
	}
	var current model.DNSCertificateProfile
	if err := database.GetDB().First(&current, id).Error; err != nil {
		return nil, err
	}
	next, err := normalizeDNSCertificateProfile(in, &current)
	if err != nil {
		return nil, err
	}
	if next.ApplyToPanel {
		if err := ensureDNSPanelProfileAvailable(current.ID); err != nil {
			return nil, err
		}
	}
	next.ID, next.CreatedAt = current.ID, current.CreatedAt
	next.LastIssuedAt, next.LastError = current.LastIssuedAt, current.LastError
	// A manual order binds the exact identifiers and validation method at
	// creation time. Never retain it when either is edited, otherwise an old
	// challenge could be mistakenly shown for the new profile configuration.
	if next.Domains == current.Domains && next.Provider == current.Provider {
		next.PendingOrder, next.PendingChallenges, next.PendingAt = current.PendingOrder, current.PendingChallenges, current.PendingAt
	} else {
		next.LastError = ""
	}
	if err := database.GetDB().Save(next).Error; err != nil {
		return nil, err
	}
	view := dnsCertificateProfileView(next)
	return &view, nil
}

// RenewIfDue renews only automatic profiles. Manual DNS profiles are never
// silently retried because a human must publish their TXT records first.
// ensureDNSPanelProfileAvailable prevents competing renewal jobs from
// alternately replacing the single certificate configured for the panel.
func ensureDNSPanelProfileAvailable(excludeID int) error {
	query := database.GetDB().Model(&model.DNSCertificateProfile{}).Where("apply_to_panel = ?", true)
	if excludeID > 0 {
		query = query.Where("id <> ?", excludeID)
	}
	var count int64
	if err := query.Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return common.NewError("only one DNS certificate profile can apply to the panel")
	}
	return nil
}

func (s DNSCertificateService) RenewIfDue(ctx context.Context) error {
	var profiles []model.DNSCertificateProfile
	if err := database.GetDB().Where("enabled = ? AND provider = ?", true, "cloudflare").Find(&profiles).Error; err != nil {
		return err
	}
	var errs []error
	for i := range profiles {
		certDir := filepath.Join(CertDir(), "dns", fmt.Sprintf("profile-%d", profiles[i].ID))
		status := InspectFile(filepath.Join(certDir, "fullchain.pem"), filepath.Join(certDir, "privkey.pem"))
		if status.Error == "" && !status.Expired && status.DaysLeft > CertRenewBeforeDays {
			continue
		}
		if _, err := s.Issue(ctx, profiles[i].ID); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", profiles[i].Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s DNSCertificateService) Delete(id int) error {
	if id <= 0 {
		return common.NewError("invalid DNS certificate profile id")
	}
	result := database.GetDB().Delete(&model.DNSCertificateProfile{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("DNS certificate profile not found")
	}
	return nil
}

// Issue starts automatic Cloudflare issuance or a two-step manual DNS flow.
// Manual mode persists no secret DNS credential: it returns the exact TXT
// record(s) and CompleteManual verifies propagation before notifying the CA.
func (s DNSCertificateService) Issue(ctx context.Context, id int) (*DNSIssueResult, error) {
	// Share the ACME issuance slot with HTTP-01 so concurrent panel actions
	// cannot race the common ACME account key or profile state.
	if err := acquireACMEIssueSlot(ctx); err != nil {
		return nil, err
	}
	defer releaseACMEIssueSlot()
	profile, domains, client, err := s.loadProfileClient(ctx, id)
	if err != nil {
		return nil, err
	}
	if profile.Provider == "manual" {
		if profile.PendingAt > 0 && time.Since(time.UnixMilli(profile.PendingAt)) <= 2*time.Hour {
			if pending, pendingErr := manualPendingResult(profile); pendingErr == nil {
				return pending, nil
			}
		}
		if profile.PendingAt > 0 {
			if err := database.GetDB().Model(&model.DNSCertificateProfile{}).Where("id = ?", profile.ID).Updates(map[string]any{"pending_order": "", "pending_challenges": "", "pending_at": 0, "last_error": ""}).Error; err != nil {
				return nil, err
			}
		}
		return s.startManual(ctx, profile, domains, client)
	}
	if profile.Provider != "cloudflare" {
		return nil, common.NewError("unsupported DNS provider")
	}
	secret, err := decryptPanelSecret(dnsProfileSecretPurpose, profile.ProviderCredentialsEncrypted)
	if err != nil || secret == "" {
		return nil, common.NewError("DNS provider token is unavailable")
	}
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domains...))
	if err != nil {
		return nil, fmt.Errorf("create DNS-01 order: %w", err)
	}
	for _, authURL := range order.AuthzURLs {
		if err := s.fulfillCloudflareDNSChallenge(ctx, client, authURL, secret); err != nil {
			return nil, s.recordDNSProfileError(profile, err)
		}
	}
	cert, err := s.finalizeDNSOrder(ctx, profile, domains, client, order.URI)
	if err != nil {
		return nil, s.recordDNSProfileError(profile, err)
	}
	return &DNSIssueResult{Status: "issued", Certificate: cert}, nil
}

func (s DNSCertificateService) startManual(ctx context.Context, profile *model.DNSCertificateProfile, domains []string, client *acme.Client) (*DNSIssueResult, error) {
	order, err := client.AuthorizeOrder(ctx, acme.DomainIDs(domains...))
	if err != nil {
		return nil, fmt.Errorf("create DNS-01 order: %w", err)
	}
	pending := make([]pendingDNSChallenge, 0, len(order.AuthzURLs))
	for _, authURL := range order.AuthzURLs {
		auth, err := client.GetAuthorization(ctx, authURL)
		if err != nil {
			return nil, err
		}
		if auth.Status == acme.StatusValid {
			continue
		}
		challenge, err := dnsChallenge(client, auth)
		if err != nil {
			return nil, err
		}
		value, err := client.DNS01ChallengeRecord(challenge.Token)
		if err != nil {
			return nil, err
		}
		pending = append(pending, pendingDNSChallenge{AuthorizationURL: auth.URI, ChallengeURL: challenge.URI, Record: "_acme-challenge." + auth.Identifier.Value, Value: value})
	}
	if len(pending) == 0 {
		// An existing valid authorization can make a freshly-created order ready
		// immediately. Do not leave an uncompletable empty manual challenge.
		cert, err := s.finalizeDNSOrder(ctx, profile, domains, client, order.URI)
		if err != nil {
			return nil, s.recordDNSProfileError(profile, err)
		}
		return &DNSIssueResult{Status: "issued", Certificate: cert}, nil
	}
	encoded, err := json.Marshal(pending)
	if err != nil {
		return nil, err
	}
	if err := database.GetDB().Model(&model.DNSCertificateProfile{}).Where("id = ?", profile.ID).Updates(map[string]any{"pending_order": order.URI, "pending_challenges": string(encoded), "pending_at": time.Now().UnixMilli(), "last_error": ""}).Error; err != nil {
		return nil, err
	}
	out := make([]DNSChallenge, len(pending))
	for i := range pending {
		out[i] = DNSChallenge{Record: pending[i].Record, Value: pending[i].Value}
	}
	return &DNSIssueResult{Status: "pending_dns", Challenges: out}, nil
}

func manualPendingResult(profile *model.DNSCertificateProfile) (*DNSIssueResult, error) {
	var pending []pendingDNSChallenge
	if err := json.Unmarshal([]byte(profile.PendingChallenges), &pending); err != nil || len(pending) == 0 {
		return nil, common.NewError("stored manual DNS challenge is invalid")
	}
	challenges := make([]DNSChallenge, len(pending))
	for i := range pending {
		challenges[i] = DNSChallenge{Record: pending[i].Record, Value: pending[i].Value}
	}
	return &DNSIssueResult{Status: "pending_dns", Challenges: challenges}, nil
}

func (s DNSCertificateService) CompleteManual(ctx context.Context, id int) (*DNSIssueResult, error) {
	if err := acquireACMEIssueSlot(ctx); err != nil {
		return nil, err
	}
	defer releaseACMEIssueSlot()
	profile, domains, client, err := s.loadProfileClient(ctx, id)
	if err != nil {
		return nil, err
	}
	if profile.Provider != "manual" || profile.PendingOrder == "" || profile.PendingChallenges == "" {
		return nil, common.NewError("there is no pending manual DNS certificate request")
	}
	if profile.PendingAt == 0 || time.Since(time.UnixMilli(profile.PendingAt)) > 2*time.Hour {
		return nil, s.clearPendingWithError(profile, "manual DNS challenge expired; start a new request")
	}
	var pending []pendingDNSChallenge
	if err := json.Unmarshal([]byte(profile.PendingChallenges), &pending); err != nil || len(pending) == 0 {
		return nil, s.clearPendingWithError(profile, "stored manual DNS challenge is invalid")
	}
	for _, item := range pending {
		if err := s.awaitDNSRecord(ctx, item.Record, item.Value); err != nil {
			return nil, s.recordDNSProfileError(profile, err)
		}
		if _, err := client.Accept(ctx, &acme.Challenge{URI: item.ChallengeURL}); err != nil {
			return nil, s.recordDNSProfileError(profile, fmt.Errorf("accept DNS challenge: %w", err))
		}
		if _, err := client.WaitAuthorization(ctx, item.AuthorizationURL); err != nil {
			return nil, s.recordDNSProfileError(profile, fmt.Errorf("validate DNS challenge: %w", err))
		}
	}
	cert, err := s.finalizeDNSOrder(ctx, profile, domains, client, profile.PendingOrder)
	if err != nil {
		return nil, s.recordDNSProfileError(profile, err)
	}
	return &DNSIssueResult{Status: "issued", Certificate: cert}, nil
}

func (s DNSCertificateService) loadProfileClient(ctx context.Context, id int) (*model.DNSCertificateProfile, []string, *acme.Client, error) {
	if id <= 0 {
		return nil, nil, nil, common.NewError("invalid DNS certificate profile id")
	}
	profile := &model.DNSCertificateProfile{}
	if err := database.GetDB().First(profile, id).Error; err != nil {
		return nil, nil, nil, err
	}
	domains, err := profileDomains(profile)
	if err != nil {
		return nil, nil, nil, err
	}
	key, err := loadOrCreateAccountKey(filepath.Join(CertDir(), "dns"))
	if err != nil {
		return nil, nil, nil, fmt.Errorf("ACME account key: %w", err)
	}
	dirURL := s.DirectoryURL
	if dirURL == "" {
		dirURL = acme.LetsEncryptURL
	}
	client := &acme.Client{Key: key, DirectoryURL: dirURL}
	contact := []string{}
	if profile.Email != "" {
		contact = append(contact, "mailto:"+profile.Email)
	}
	if _, err := client.Register(ctx, &acme.Account{Contact: contact}, acme.AcceptTOS); err != nil && !errors.Is(err, acme.ErrAccountAlreadyExists) {
		return nil, nil, nil, fmt.Errorf("ACME register: %w", err)
	}
	return profile, domains, client, nil
}

func (s DNSCertificateService) fulfillCloudflareDNSChallenge(ctx context.Context, client *acme.Client, authURL, token string) error {
	auth, err := client.GetAuthorization(ctx, authURL)
	if err != nil {
		return err
	}
	if auth.Status == acme.StatusValid {
		return nil
	}
	challenge, err := dnsChallenge(client, auth)
	if err != nil {
		return err
	}
	value, err := client.DNS01ChallengeRecord(challenge.Token)
	if err != nil {
		return err
	}
	record := "_acme-challenge." + auth.Identifier.Value
	cloudflare := cloudflareDNS{token: token}
	recordID, err := cloudflare.createTXT(ctx, record, value)
	if err != nil {
		return err
	}
	defer func() { _ = cloudflare.deleteRecord(context.Background(), recordID) }()
	if err := s.awaitDNSRecord(ctx, record, value); err != nil {
		return err
	}
	if _, err := client.Accept(ctx, challenge); err != nil {
		return err
	}
	_, err = client.WaitAuthorization(ctx, auth.URI)
	return err
}

func (s DNSCertificateService) awaitDNSRecord(ctx context.Context, record, expected string) error {
	lookup := s.DNSLookupTXT
	if lookup == nil {
		lookup = net.DefaultResolver.LookupTXT
	}
	deadline := time.NewTimer(2 * time.Minute)
	defer deadline.Stop()
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		values, err := lookup(ctx, record)
		if err == nil {
			for _, value := range values {
				if strings.TrimSpace(value) == expected {
					return nil
				}
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("DNS TXT record %s has not propagated yet", record)
		case <-tick.C:
		}
	}
}

func (s DNSCertificateService) finalizeDNSOrder(ctx context.Context, profile *model.DNSCertificateProfile, domains []string, client *acme.Client, orderURL string) (*CertStatus, error) {
	order, err := client.WaitOrder(ctx, orderURL)
	if err != nil {
		return nil, fmt.Errorf("wait for DNS order: %w", err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	csr, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{Subject: pkix.Name{CommonName: certificateCommonName(domains)}, DNSNames: domains}, leafKey)
	if err != nil {
		return nil, err
	}
	chain, _, err := client.CreateOrderCert(ctx, order.FinalizeURL, csr, true)
	if err != nil {
		return nil, fmt.Errorf("finalize DNS order: %w", err)
	}
	certFile, keyFile, err := writeCertAndKey(filepath.Join(CertDir(), "dns", fmt.Sprintf("profile-%d", profile.ID)), chain, leafKey)
	if err != nil {
		return nil, err
	}
	if profile.ApplyToPanel {
		if err := (&SettingService{}).SetCertFile(certFile); err != nil {
			return nil, err
		}
		if err := (&SettingService{}).SetKeyFile(keyFile); err != nil {
			return nil, err
		}
	}
	now := time.Now().UnixMilli()
	if err := database.GetDB().Model(&model.DNSCertificateProfile{}).Where("id = ?", profile.ID).Updates(map[string]any{"last_issued_at": now, "last_error": "", "pending_order": "", "pending_challenges": "", "pending_at": 0}).Error; err != nil {
		return nil, err
	}
	return InspectFile(certFile, keyFile), nil
}

func dnsChallenge(_ *acme.Client, auth *acme.Authorization) (*acme.Challenge, error) {
	for _, challenge := range auth.Challenges {
		if challenge.Type == "dns-01" {
			return challenge, nil
		}
	}
	return nil, common.NewError("ACME provider did not offer DNS-01 for this domain")
}

func normalizeDNSCertificateProfile(in DNSCertificateProfileInput, existing *model.DNSCertificateProfile) (*model.DNSCertificateProfile, error) {
	name := strings.TrimSpace(in.Name)
	if name == "" || len(name) > 64 {
		return nil, common.NewError("DNS certificate profile name must be 1 to 64 characters")
	}
	domains, err := normalizeDNSDomains(in.Domains)
	if err != nil {
		return nil, err
	}
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if provider != "manual" && provider != "cloudflare" {
		return nil, common.NewError("DNS certificate provider must be manual or cloudflare")
	}
	if in.ApplyToPanel && len(domains) == 0 {
		return nil, common.NewError("panel certificate requires at least one domain")
	}
	if provider == "cloudflare" && strings.TrimSpace(in.ProviderToken) == "" && (existing == nil || existing.ProviderCredentialsEncrypted == "") {
		return nil, common.NewError("Cloudflare API token is required")
	}
	email := strings.TrimSpace(in.Email)
	if email != "" {
		parsed, err := mail.ParseAddress(email)
		if err != nil || parsed.Address != email || len(email) > 254 {
			return nil, common.NewError("ACME account email is invalid")
		}
	}
	encodedDomains, _ := json.Marshal(domains)
	row := &model.DNSCertificateProfile{Name: name, Domains: string(encodedDomains), Provider: provider, Email: email, ApplyToPanel: in.ApplyToPanel, Enabled: in.Enabled}
	// Keep a Cloudflare credential only when this remains a Cloudflare profile.
	// Switching to manual DNS clears provider credentials rather than leaving an
	// unnecessary secret attached to a general-purpose manual workflow.
	if existing != nil && provider == "cloudflare" && existing.Provider == "cloudflare" {
		row.ProviderCredentialsEncrypted = existing.ProviderCredentialsEncrypted
	}
	if strings.TrimSpace(in.ProviderToken) != "" {
		ciphertext, err := encryptPanelSecret(dnsProfileSecretPurpose, strings.TrimSpace(in.ProviderToken))
		if err != nil {
			return nil, err
		}
		row.ProviderCredentialsEncrypted = ciphertext
	}
	return row, nil
}

func normalizeDNSDomains(domains []string) ([]string, error) {
	if len(domains) == 0 || len(domains) > 20 {
		return nil, common.NewError("select between 1 and 20 DNS certificate domains")
	}
	seen := map[string]struct{}{}
	out := make([]string, 0, len(domains))
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		wildcard := strings.HasPrefix(domain, "*.")
		if wildcard {
			domain = strings.TrimPrefix(domain, "*.")
		}
		clean, err := ValidateCertDomain(domain)
		if err != nil || net.ParseIP(clean) != nil {
			return nil, common.NewError("DNS-01 certificate domains must be valid DNS hostnames")
		}
		if wildcard {
			clean = "*." + clean
		}
		if _, ok := seen[clean]; !ok {
			seen[clean] = struct{}{}
			out = append(out, clean)
		}
	}
	sort.Strings(out)
	return out, nil
}

func profileDomains(profile *model.DNSCertificateProfile) ([]string, error) {
	var domains []string
	if err := json.Unmarshal([]byte(profile.Domains), &domains); err != nil {
		return nil, common.NewError("stored DNS certificate domains are invalid")
	}
	return normalizeDNSDomains(domains)
}

func certificateCommonName(domains []string) string {
	for _, domain := range domains {
		if !strings.HasPrefix(domain, "*.") {
			return domain
		}
	}
	return strings.TrimPrefix(domains[0], "*.")
}

func (s DNSCertificateService) recordDNSProfileError(profile *model.DNSCertificateProfile, err error) error {
	message := err.Error()
	if len(message) > 512 {
		message = message[:512]
	}
	_ = database.GetDB().Model(&model.DNSCertificateProfile{}).Where("id = ?", profile.ID).Updates(map[string]any{"last_error": message}).Error
	return err
}

func (s DNSCertificateService) clearPendingWithError(profile *model.DNSCertificateProfile, message string) error {
	_ = database.GetDB().Model(&model.DNSCertificateProfile{}).Where("id = ?", profile.ID).Updates(map[string]any{"pending_order": "", "pending_challenges": "", "pending_at": 0, "last_error": message}).Error
	return common.NewError(message)
}

func dnsCertificateProfileView(row *model.DNSCertificateProfile) DNSCertificateProfileView {
	var domains []string
	_ = json.Unmarshal([]byte(row.Domains), &domains)
	return DNSCertificateProfileView{ID: row.ID, Name: row.Name, Domains: domains, Provider: row.Provider, HasProviderToken: row.ProviderCredentialsEncrypted != "", Email: row.Email, ApplyToPanel: row.ApplyToPanel, Enabled: row.Enabled, LastIssuedAt: row.LastIssuedAt, LastError: row.LastError, PendingAt: row.PendingAt, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

// cloudflareDNS is intentionally a very small adapter around the public DNS
// Records API. Any other provider can use the same DNS-01 service by adding an
// adapter without coupling certificate issuance to a server distribution.
type cloudflareDNS struct{ token string }

// DNS provider credentials must not be sent through an ambient proxy.
var cloudflareHTTPClient = &http.Client{
	Timeout:   20 * time.Second,
	Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second},
	CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	},
}

type cloudflareResponse struct {
	Success bool `json:"success"`
	Errors  []struct {
		Message string `json:"message"`
	} `json:"errors"`
	Result struct {
		ID string `json:"id"`
	} `json:"result"`
}

func (c cloudflareDNS) createTXT(ctx context.Context, name, content string) (string, error) {
	zoneID, err := c.zoneID(ctx, name)
	if err != nil {
		return "", err
	}
	body, _ := json.Marshal(map[string]any{"type": "TXT", "name": name, "content": content, "ttl": 120})
	var result cloudflareResponse
	if err := c.request(ctx, http.MethodPost, "https://api.cloudflare.com/client/v4/zones/"+zoneID+"/dns_records", body, &result); err != nil {
		return "", err
	}
	if !result.Success || result.Result.ID == "" {
		return "", cloudflareError(result)
	}
	return zoneID + "/" + result.Result.ID, nil
}

func (c cloudflareDNS) deleteRecord(ctx context.Context, id string) error {
	zoneID, recordID, ok := strings.Cut(id, "/")
	if !ok || zoneID == "" || recordID == "" {
		return errors.New("invalid Cloudflare DNS record id")
	}
	var result cloudflareResponse
	if err := c.request(ctx, http.MethodDelete, "https://api.cloudflare.com/client/v4/zones/"+zoneID+"/dns_records/"+recordID, nil, &result); err != nil {
		return err
	}
	if !result.Success {
		return cloudflareError(result)
	}
	return nil
}

func (c cloudflareDNS) zoneID(ctx context.Context, fqdn string) (string, error) {
	labels := strings.Split(strings.TrimSuffix(fqdn, "."), ".")
	for i := 0; i < len(labels)-1; i++ {
		candidate := strings.Join(labels[i:], ".")
		var result struct {
			Success bool `json:"success"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
			Result []struct {
				ID string `json:"id"`
			} `json:"result"`
		}
		if err := c.request(ctx, http.MethodGet, "https://api.cloudflare.com/client/v4/zones?name="+urlQueryEscape(candidate)+"&status=active", nil, &result); err != nil {
			return "", err
		}
		if result.Success && len(result.Result) == 1 && result.Result[0].ID != "" {
			return result.Result[0].ID, nil
		}
	}
	return "", common.NewError("Cloudflare zone for DNS challenge was not found")
}

func (c cloudflareDNS) request(ctx context.Context, method, endpoint string, body []byte, out any) error {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := cloudflareHTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("Cloudflare API returned HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(out)
}

func cloudflareError(result cloudflareResponse) error {
	if len(result.Errors) > 0 && result.Errors[0].Message != "" {
		return errors.New("Cloudflare API: " + result.Errors[0].Message)
	}
	return errors.New("Cloudflare API request failed")
}

func urlQueryEscape(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "+", "%2B"), " ", "%20")
}
