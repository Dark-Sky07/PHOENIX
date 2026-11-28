package service

import (
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

func initPhaseBTestDB(t *testing.T) {
	t.Helper()
	dbDir := t.TempDir()
	t.Setenv("XUI_DB_FOLDER", dbDir)
	if err := database.InitDB(filepath.Join(dbDir, "x-ui.db")); err != nil {
		t.Fatalf("InitDB: %v", err)
	}
	t.Cleanup(func() { _ = database.CloseDB() })
}

func TestRuleSetCatalogNormalizesAndRejectsUnsafeSources(t *testing.T) {
	row, err := validateRuleSetCatalogInput(RuleSetCatalogInput{
		Name: "Public domains", URL: "https://example.com/list.txt", Kind: "domain", Enabled: true, RefreshMinutes: 15,
	})
	if err != nil || row.URL != "https://example.com/list.txt" {
		t.Fatalf("valid catalog input = (%+v, %v)", row, err)
	}
	for _, raw := range []string{
		"http://example.com/list.txt",
		"https://example.com:8443/list.txt",
		"https://user@example.com/list.txt",
		"https://example.com/list.txt#fragment",
	} {
		if _, err := validateRuleSetURL(raw); err == nil {
			t.Fatalf("unsafe catalog URL %q was accepted", raw)
		}
	}
	entries, err := normalizeRuleSetEntries("domain", []byte("Example.com\n# comment\ngeosite:cn\nfull:WWW.Example.com\nexample.com\n"))
	if err != nil || len(entries) != 3 {
		t.Fatalf("normalized domain entries = (%v, %v)", entries, err)
	}
	for _, host := range []string{"127.0.0.1", "10.0.0.1", "100.64.0.1", "192.0.2.1", "198.18.0.1", "::1", "2001:db8::1", "fec0::1"} {
		if err := ensurePublicCatalogHost(host); err == nil {
			t.Fatalf("non-public catalog host %q was accepted", host)
		}
	}
}

func TestRuleSetCatalogUpdateClearsCacheFromChangedSource(t *testing.T) {
	initPhaseBTestDB(t)
	svc := RuleSetCatalogService{}
	created, err := svc.Create(RuleSetCatalogInput{Name: "catalog", URL: "https://example.com/one.txt", Kind: "domain", Enabled: true, RefreshMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(&model.RuleSetCatalog{}).Where("id = ?", created.ID).Updates(map[string]any{
		"entries": `["domain:example.com"]`, "sha256": "digest", "last_success_at": 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(created.ID, RuleSetCatalogInput{Name: "catalog", URL: "https://example.com/two.txt", Kind: "domain", Enabled: true, RefreshMinutes: 30})
	if err != nil {
		t.Fatal(err)
	}
	if updated.EntryCount != 0 || updated.SHA256 != "" || updated.LastSuccessAt != 0 {
		t.Fatalf("source update exposed stale cached catalog data: %+v", updated)
	}
}

func TestDashboardPreferenceIsPerUserAndKeepsSafeDefaults(t *testing.T) {
	initPhaseBTestDB(t)
	svc := DashboardPreferenceService{}
	first, err := svc.Get(42)
	if err != nil || len(first.Layout) != len(defaultDashboardLayout) || first.RefreshSeconds != 2 {
		t.Fatalf("default dashboard preference = (%+v, %v)", first, err)
	}
	saved, err := svc.Save(42, DashboardPreferenceInput{
		Layout: []string{"info", "resources"}, HiddenWidgets: []string{"network", "unknown"}, RefreshSeconds: 20,
	})
	if err != nil || saved.RefreshSeconds != 20 || !containsDashboardWidget(saved.HiddenWidgets, "network") || containsDashboardWidget(saved.HiddenWidgets, "unknown") {
		t.Fatalf("saved dashboard preference = (%+v, %v)", saved, err)
	}
	if len(saved.Layout) != len(defaultDashboardLayout) || saved.Layout[0] != "info" {
		t.Fatalf("saved layout did not preserve ordering/add new safe widgets: %+v", saved.Layout)
	}
	other, err := svc.Get(43)
	if err != nil || other.RefreshSeconds != 2 || len(other.HiddenWidgets) != 0 {
		t.Fatalf("another administrator inherited preferences: (%+v, %v)", other, err)
	}
	if _, err := svc.Save(42, DashboardPreferenceInput{RefreshSeconds: 61}); err == nil {
		t.Fatal("invalid refresh interval was accepted")
	}
}

func TestDNSCertificateProfileSupportsWildcardsAndDropsUnusedProviderSecret(t *testing.T) {
	manual, err := normalizeDNSCertificateProfile(DNSCertificateProfileInput{
		Name: "wildcard", Domains: []string{"*.Example.com", "example.com"}, Provider: "manual", Enabled: true,
	}, nil)
	if err != nil || manual.Provider != "manual" || manual.ProviderCredentialsEncrypted != "" {
		t.Fatalf("manual wildcard profile = (%+v, %v)", manual, err)
	}
	if _, err := normalizeDNSCertificateProfile(DNSCertificateProfileInput{
		Name: "bad", Domains: []string{"192.0.2.1"}, Provider: "manual",
	}, nil); err == nil {
		t.Fatal("IP DNS-01 profile was accepted")
	}
	previous := manual
	previous.Provider = "cloudflare"
	previous.ProviderCredentialsEncrypted = "ciphertext"
	updated, err := normalizeDNSCertificateProfile(DNSCertificateProfileInput{
		Name: "wildcard", Domains: []string{"example.com"}, Provider: "manual", Enabled: true,
	}, previous)
	if err != nil || updated.ProviderCredentialsEncrypted != "" {
		t.Fatalf("manual update retained a provider secret: (%+v, %v)", updated, err)
	}
}

func TestDNSCertificateProfileUpdateClearsObsoleteManualChallenge(t *testing.T) {
	initPhaseBTestDB(t)
	svc := DNSCertificateService{}
	created, err := svc.Create(DNSCertificateProfileInput{Name: "manual", Domains: []string{"example.com"}, Provider: "manual", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := database.GetDB().Model(&model.DNSCertificateProfile{}).Where("id = ?", created.ID).Updates(map[string]any{
		"pending_order": "https://acme.example/order/1", "pending_challenges": `[{"record":"_acme-challenge.example.com","value":"txt"}]`, "pending_at": 1,
	}).Error; err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(created.ID, DNSCertificateProfileInput{Name: "manual", Domains: []string{"new.example.com"}, Provider: "manual", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if updated.PendingAt != 0 {
		t.Fatalf("manual challenge survived an identifier change: %+v", updated)
	}
}

func TestDNSCertificateOnlyOneProfileCanApplyToPanel(t *testing.T) {
	initPhaseBTestDB(t)
	svc := DNSCertificateService{}
	if _, err := svc.Create(DNSCertificateProfileInput{Name: "panel", Domains: []string{"panel.example.com"}, Provider: "manual", ApplyToPanel: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Create(DNSCertificateProfileInput{Name: "other", Domains: []string{"other.example.com"}, Provider: "manual", ApplyToPanel: true, Enabled: true}); err == nil {
		t.Fatal("multiple DNS profiles were allowed to replace the panel certificate")
	}
}

func TestOIDCIdentityIndexDoesNotConstrainLegacyBlankUsers(t *testing.T) {
	initPhaseBTestDB(t)
	for _, username := range []string{"legacy-one", "legacy-two"} {
		if err := database.GetDB().Create(&model.User{Username: username, Password: "legacy"}).Error; err != nil {
			t.Fatalf("create legacy blank OIDC user %q: %v", username, err)
		}
	}
	first := &model.User{Username: "oidc-one", Password: "x", OIDCIssuer: "https://id.example.com", OIDCSubject: "subject"}
	if err := database.GetDB().Create(first).Error; err != nil {
		t.Fatalf("create first OIDC identity: %v", err)
	}
	if err := database.GetDB().Create(&model.User{Username: "oidc-two", Password: "x", OIDCIssuer: "https://id.example.com", OIDCSubject: "subject"}).Error; err == nil {
		t.Fatal("duplicate non-empty OIDC identity was accepted")
	}
}

func TestOIDCExistingAccountLinkingRequiresVerifiedEmail(t *testing.T) {
	initPhaseBTestDB(t)
	if err := database.GetDB().Create(&model.User{Username: "admin", Password: "local-password", Role: "admin"}).Error; err != nil {
		t.Fatal(err)
	}
	verified := true
	config := &model.OIDCProviderConfig{Issuer: "https://id.example.com", AllowedEmailDomains: `["example.com"]`, DefaultRole: "viewer"}
	claims := &oidcTokenClaims{Subject: "subject", Email: "attacker@example.com", EmailVerified: &verified, PreferredName: "admin", Raw: map[string]any{}}
	if _, err := resolveOIDCUser(config, claims); err == nil {
		t.Fatal("preferred_username was allowed to link a local administrator")
	}
	var local model.User
	if err := database.GetDB().Where("username = ?", "admin").First(&local).Error; err != nil {
		t.Fatal(err)
	}
	if local.OIDCIssuer != "" || local.OIDCSubject != "" {
		t.Fatal("local administrator was unexpectedly linked through preferred_username")
	}

	linked := model.User{Username: "linked@example.com", Password: "local-password", Role: "admin", OIDCIssuer: "https://id.example.com", OIDCSubject: "linked-subject"}
	if err := database.GetDB().Create(&linked).Error; err != nil {
		t.Fatal(err)
	}
	claims = &oidcTokenClaims{Subject: "linked-subject", Email: "linked@other.example", EmailVerified: &verified, Raw: map[string]any{}}
	if _, err := resolveOIDCUser(config, claims); err == nil {
		t.Fatal("allowed-email policy was bypassed by an existing linked identity")
	}
}

func TestOIDCConfigurationRequiresCompleteEnabledProviderAndRetainsSecret(t *testing.T) {
	if _, err := normalizeOIDCConfig(OIDCConfigInput{Enabled: true, ClientID: "panel", ClientSecret: "secret"}); err == nil {
		t.Fatal("enabled OIDC configuration without issuer was accepted")
	}
	initPhaseBTestDB(t)
	svc := OIDCService{}
	created, err := svc.SaveConfig(OIDCConfigInput{
		Enabled: true, Issuer: "https://id.example.com/realms/panel", ClientID: "panel", ClientSecret: "top-secret", AutoProvision: true,
		AllowedEmailDomains: []string{"example.com"}, DefaultRole: "viewer", GroupClaim: "groups", GroupRoleMappings: map[string]string{"operators": "admin"},
	})
	if err != nil || !created.HasClientSecret || created.Issuer == "" {
		t.Fatalf("saved OIDC configuration = (%+v, %v)", created, err)
	}
	updated, err := svc.SaveConfig(OIDCConfigInput{
		Enabled: true, Issuer: "https://id.example.com/realms/panel", ClientID: "panel", AutoProvision: true,
		AllowedEmailDomains: []string{"example.com"}, DefaultRole: "viewer", GroupClaim: "groups", GroupRoleMappings: map[string]string{"operators": "admin"},
	})
	if err != nil || !updated.HasClientSecret {
		t.Fatalf("OIDC secret was not retained on metadata-only update: (%+v, %v)", updated, err)
	}
	cleared, err := svc.SaveConfig(OIDCConfigInput{
		Enabled: true, Issuer: "https://id.example.com/realms/panel", ClientID: "panel", ClearClientSecret: true, AutoProvision: true,
		AllowedEmailDomains: []string{"example.com"}, DefaultRole: "viewer", GroupClaim: "groups", GroupRoleMappings: map[string]string{"operators": "admin"},
	})
	if err != nil || cleared.HasClientSecret {
		t.Fatalf("OIDC secret was not cleared explicitly: (%+v, %v)", cleared, err)
	}
}

func TestOIDCAudienceAndAlgorithmPolicy(t *testing.T) {
	if !containsOIDCAudience(oidcAudiences([]byte(`["other","panel"]`)), "panel") {
		t.Fatal("OIDC audience parser lost client audience")
	}
	if containsOIDCAudience(oidcAudiences([]byte(`null`)), "panel") {
		t.Fatal("invalid OIDC audience was accepted")
	}
	if err := verifyOIDCSignature("none", nil, []byte("header.payload"), nil); err == nil {
		t.Fatal("unsigned OIDC algorithm was accepted")
	}
}
