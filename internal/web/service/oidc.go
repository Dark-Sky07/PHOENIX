package service

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	_ "crypto/sha512" // register SHA-384/SHA-512 for RS384/RS512 and PS384/PS512
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/mail"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/common"
	panelcrypto "github.com/Dark-Sky07/phoenix/internal/util/crypto"
	"github.com/Dark-Sky07/phoenix/internal/util/random"
)

const oidcSecretPurpose = "oidc-client-secret-v1"

type OIDCService struct{}

type OIDCConfigInput struct {
	Enabled             bool              `json:"enabled"`
	Issuer              string            `json:"issuer"`
	ClientID            string            `json:"clientId"`
	ClientSecret        string            `json:"clientSecret"`
	ClearClientSecret   bool              `json:"clearClientSecret"`
	AllowedEmailDomains []string          `json:"allowedEmailDomains"`
	AutoProvision       bool              `json:"autoProvision"`
	DefaultRole         string            `json:"defaultRole"`
	GroupClaim          string            `json:"groupClaim"`
	GroupRoleMappings   map[string]string `json:"groupRoleMappings"`
}

type OIDCConfigView struct {
	Enabled             bool              `json:"enabled"`
	Issuer              string            `json:"issuer"`
	ClientID            string            `json:"clientId"`
	HasClientSecret     bool              `json:"hasClientSecret"`
	AllowedEmailDomains []string          `json:"allowedEmailDomains"`
	AutoProvision       bool              `json:"autoProvision"`
	DefaultRole         string            `json:"defaultRole"`
	GroupClaim          string            `json:"groupClaim"`
	GroupRoleMappings   map[string]string `json:"groupRoleMappings"`
}

type OIDCBegin struct {
	AuthorizationURL string
	State            string
	Nonce            string
	CodeVerifier     string
}

type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
}

type oidcTokenClaims struct {
	Issuer          string          `json:"iss"`
	Subject         string          `json:"sub"`
	Audience        json.RawMessage `json:"aud"`
	AuthorizedParty string          `json:"azp"`
	ExpiresAt       int64           `json:"exp"`
	IssuedAt        int64           `json:"iat"`
	NotBefore       int64           `json:"nbf"`
	Nonce           string          `json:"nonce"`
	Email           string          `json:"email"`
	EmailVerified   *bool           `json:"email_verified"`
	PreferredName   string          `json:"preferred_username"`
	Name            string          `json:"name"`
	Raw             map[string]any  `json:"-"`
}

type oidcCacheEntry struct {
	discovery oidcDiscovery
	until     time.Time
}

var oidcDiscoveryCache = struct {
	sync.Mutex
	values map[string]oidcCacheEntry
}{values: map[string]oidcCacheEntry{}}

func (OIDCService) GetConfig() (*OIDCConfigView, error) {
	config, err := loadOIDCConfig()
	if err != nil {
		if database.IsNotFound(err) {
			view := defaultOIDCConfigView()
			return &view, nil
		}
		return nil, err
	}
	view := oidcConfigView(config)
	return &view, nil
}

func (OIDCService) SaveConfig(input OIDCConfigInput) (*OIDCConfigView, error) {
	var existing model.OIDCProviderConfig
	err := database.GetDB().First(&existing, 1).Error
	if err != nil && !database.IsNotFound(err) {
		return nil, err
	}
	config, err := normalizeOIDCConfig(input)
	if err != nil {
		return nil, err
	}
	if existing.ID != 0 && strings.TrimSpace(input.ClientSecret) == "" && !input.ClearClientSecret {
		config.ClientSecretEncrypted = existing.ClientSecretEncrypted
	}
	config.ID = 1
	if existing.ID != 0 {
		config.CreatedAt = existing.CreatedAt
	}
	if existing.ID == 0 {
		if err := database.GetDB().Create(config).Error; err != nil {
			return nil, err
		}
	} else if err := database.GetDB().Save(config).Error; err != nil {
		return nil, err
	}
	clearOIDCDiscoveryCache(config.Issuer)
	view := oidcConfigView(config)
	return &view, nil
}

func (s OIDCService) Begin(ctx context.Context, redirectURI string) (*OIDCBegin, error) {
	config, err := loadEnabledOIDCConfig()
	if err != nil {
		return nil, err
	}
	discovery, err := discoverOIDC(ctx, config.Issuer)
	if err != nil {
		return nil, err
	}
	state, err := secureURLToken(32)
	if err != nil {
		return nil, err
	}
	nonce, err := secureURLToken(32)
	if err != nil {
		return nil, err
	}
	verifier, err := secureURLToken(48)
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256([]byte(verifier))
	u, err := url.Parse(discovery.AuthorizationEndpoint)
	if err != nil {
		return nil, common.NewError("OIDC authorization endpoint is invalid")
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", config.ClientID)
	q.Set("redirect_uri", redirectURI)
	q.Set("scope", "openid profile email")
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", base64.RawURLEncoding.EncodeToString(sum[:]))
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return &OIDCBegin{AuthorizationURL: u.String(), State: state, Nonce: nonce, CodeVerifier: verifier}, nil
}

func (s OIDCService) Complete(ctx context.Context, redirectURI, code, nonce, verifier string) (*model.User, error) {
	if strings.TrimSpace(code) == "" || strings.TrimSpace(nonce) == "" || strings.TrimSpace(verifier) == "" {
		return nil, common.NewError("OIDC callback is incomplete")
	}
	config, err := loadEnabledOIDCConfig()
	if err != nil {
		return nil, err
	}
	discovery, err := discoverOIDC(ctx, config.Issuer)
	if err != nil {
		return nil, err
	}
	secret, err := decryptPanelSecret(oidcSecretPurpose, config.ClientSecretEncrypted)
	if err != nil {
		return nil, err
	}
	idToken, err := exchangeOIDCCode(ctx, discovery.TokenEndpoint, config.ClientID, secret, redirectURI, code, verifier)
	if err != nil {
		return nil, err
	}
	claims, err := validateOIDCIDToken(ctx, idToken, discovery, config.ClientID, nonce)
	if err != nil {
		return nil, err
	}
	return resolveOIDCUser(config, claims)
}

func loadOIDCConfig() (*model.OIDCProviderConfig, error) {
	config := &model.OIDCProviderConfig{}
	if err := database.GetDB().First(config, 1).Error; err != nil {
		return nil, err
	}
	return config, nil
}

func loadEnabledOIDCConfig() (*model.OIDCProviderConfig, error) {
	config, err := loadOIDCConfig()
	if err != nil {
		return nil, common.NewError("OIDC sign-in is not configured")
	}
	if !config.Enabled {
		return nil, common.NewError("OIDC sign-in is disabled")
	}
	if config.Issuer == "" || config.ClientID == "" {
		return nil, common.NewError("OIDC sign-in is incomplete")
	}
	return config, nil
}

func normalizeOIDCConfig(input OIDCConfigInput) (*model.OIDCProviderConfig, error) {
	issuer, err := validateOIDCIssuer(input.Issuer)
	if err != nil {
		return nil, err
	}
	clientID := strings.TrimSpace(input.ClientID)
	if input.Enabled && issuer == "" {
		return nil, common.NewError("OIDC issuer is required")
	}
	if input.Enabled && clientID == "" {
		return nil, common.NewError("OIDC client ID is required")
	}
	domains, err := normalizeOIDCDomains(input.AllowedEmailDomains)
	if err != nil {
		return nil, err
	}
	role := strings.ToLower(strings.TrimSpace(input.DefaultRole))
	if role == "" {
		role = "viewer"
	}
	if !validOIDCRole(role) || role == "superadmin" {
		return nil, common.NewError("OIDC default role must be viewer, support, or admin")
	}
	claim := strings.TrimSpace(input.GroupClaim)
	if len(claim) > 64 {
		return nil, common.NewError("OIDC group claim is too long")
	}
	mappings := map[string]string{}
	for group, mappedRole := range input.GroupRoleMappings {
		group = strings.TrimSpace(group)
		mappedRole = strings.ToLower(strings.TrimSpace(mappedRole))
		if group == "" || len(group) > 128 || !validOIDCRole(mappedRole) || mappedRole == "superadmin" {
			return nil, common.NewError("OIDC group role mapping is invalid")
		}
		mappings[group] = mappedRole
	}
	if len(mappings) > 0 && claim == "" {
		return nil, common.NewError("OIDC group claim is required when role mappings are configured")
	}
	encodedDomains, _ := json.Marshal(domains)
	encodedMappings, _ := json.Marshal(mappings)
	config := &model.OIDCProviderConfig{Enabled: input.Enabled, Issuer: issuer, ClientID: clientID, AllowedEmailDomains: string(encodedDomains), AutoProvision: input.AutoProvision, DefaultRole: role, GroupClaim: claim, GroupRoleMappings: string(encodedMappings)}
	if strings.TrimSpace(input.ClientSecret) != "" {
		ciphertext, err := encryptPanelSecret(oidcSecretPurpose, strings.TrimSpace(input.ClientSecret))
		if err != nil {
			return nil, err
		}
		config.ClientSecretEncrypted = ciphertext
	}
	// A client secret is optional: public OIDC clients can use the required
	// Authorization Code + PKCE flow without one. Confidential clients retain
	// their encrypted secret when a metadata-only update leaves this blank.
	return config, nil
}

func validateOIDCIssuer(raw string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", nil
	}
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return "", common.NewError("OIDC issuer must be an absolute HTTPS URL without query or fragment")
	}
	return strings.TrimSuffix(u.String(), "/"), nil
}

func normalizeOIDCDomains(domains []string) ([]string, error) {
	set := map[string]struct{}{}
	for _, domain := range domains {
		domain = strings.ToLower(strings.TrimSpace(domain))
		if domain == "" {
			continue
		}
		clean, err := ValidateCertDomain(domain)
		if err != nil || strings.Count(clean, ".") == 0 {
			return nil, common.NewError("invalid allowed OIDC email domain")
		}
		set[clean] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for d := range set {
		out = append(out, d)
	}
	sort.Strings(out)
	return out, nil
}

func validOIDCRole(role string) bool {
	return role == "viewer" || role == "support" || role == "admin" || role == "superadmin"
}

func defaultOIDCConfigView() OIDCConfigView {
	return OIDCConfigView{DefaultRole: "viewer", AllowedEmailDomains: []string{}, GroupRoleMappings: map[string]string{}}
}

func oidcConfigView(config *model.OIDCProviderConfig) OIDCConfigView {
	view := defaultOIDCConfigView()
	view.Enabled, view.Issuer, view.ClientID, view.HasClientSecret = config.Enabled, config.Issuer, config.ClientID, config.ClientSecretEncrypted != ""
	view.AutoProvision, view.DefaultRole, view.GroupClaim = config.AutoProvision, config.DefaultRole, config.GroupClaim
	_ = json.Unmarshal([]byte(config.AllowedEmailDomains), &view.AllowedEmailDomains)
	_ = json.Unmarshal([]byte(config.GroupRoleMappings), &view.GroupRoleMappings)
	if view.AllowedEmailDomains == nil {
		view.AllowedEmailDomains = []string{}
	}
	if view.GroupRoleMappings == nil {
		view.GroupRoleMappings = map[string]string{}
	}
	return view
}

func discoverOIDC(ctx context.Context, issuer string) (*oidcDiscovery, error) {
	issuer = strings.TrimSuffix(issuer, "/")
	oidcDiscoveryCache.Lock()
	cached, ok := oidcDiscoveryCache.values[issuer]
	oidcDiscoveryCache.Unlock()
	if ok && time.Now().Before(cached.until) {
		return &cached.discovery, nil
	}
	endpoint := issuer + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := oidcHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return nil, fmt.Errorf("OIDC discovery: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("OIDC discovery returned HTTP %d", resp.StatusCode)
	}
	var discovery oidcDiscovery
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&discovery); err != nil {
		return nil, err
	}
	if strings.TrimSuffix(discovery.Issuer, "/") != issuer || !validOIDCEndpoint(discovery.AuthorizationEndpoint) || !validOIDCEndpoint(discovery.TokenEndpoint) || !validOIDCEndpoint(discovery.JWKSURI) {
		return nil, common.NewError("OIDC discovery document is invalid")
	}
	oidcDiscoveryCache.Lock()
	oidcDiscoveryCache.values[issuer] = oidcCacheEntry{discovery: discovery, until: time.Now().Add(10 * time.Minute)}
	oidcDiscoveryCache.Unlock()
	return &discovery, nil
}

func clearOIDCDiscoveryCache(issuer string) {
	oidcDiscoveryCache.Lock()
	delete(oidcDiscoveryCache.values, strings.TrimSuffix(issuer, "/"))
	oidcDiscoveryCache.Unlock()
}
func validOIDCEndpoint(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Host != "" && u.User == nil && u.Fragment == ""
}

// OIDC endpoint metadata is authenticated by the configured issuer's TLS
// connection. Do not silently follow a second, potentially unrelated URL from
// discovery, JWKS, or token exchange responses.
func oidcHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		// OIDC client secrets and authorization codes must not traverse an
		// ambient proxy configured in the panel process environment.
		Transport: &http.Transport{Proxy: nil, TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: timeout},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func exchangeOIDCCode(ctx context.Context, endpoint, clientID, secret, redirectURI, code, verifier string) (string, error) {
	form := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirectURI}, "client_id": {clientID}, "code_verifier": {verifier}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if secret != "" {
		req.SetBasicAuth(clientID, secret)
	}
	resp, err := oidcHTTPClient(20 * time.Second).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("OIDC token exchange returned HTTP %d", resp.StatusCode)
	}
	var payload struct {
		IDToken string `json:"id_token"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", err
	}
	if payload.IDToken == "" {
		return "", common.NewError("OIDC token response contains no ID token")
	}
	return payload.IDToken, nil
}

func validateOIDCIDToken(ctx context.Context, token string, discovery *oidcDiscovery, clientID, expectedNonce string) (*oidcTokenClaims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, common.NewError("OIDC ID token is malformed")
	}
	headerJSON, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	var header struct {
		Algorithm string `json:"alg"`
		KeyID     string `json:"kid"`
	}
	if err := json.Unmarshal(headerJSON, &header); err != nil {
		return nil, err
	}
	if header.Algorithm == "" || header.Algorithm == "none" || header.KeyID == "" {
		return nil, common.NewError("OIDC ID token header is invalid")
	}
	claimsJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	var claims oidcTokenClaims
	if err := json.Unmarshal(claimsJSON, &claims); err != nil {
		return nil, err
	}
	_ = json.Unmarshal(claimsJSON, &claims.Raw)
	key, err := oidcJWK(ctx, discovery.JWKSURI, header.KeyID)
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, err
	}
	if err := verifyOIDCSignature(header.Algorithm, key, []byte(parts[0]+"."+parts[1]), sig); err != nil {
		return nil, err
	}
	if strings.TrimSuffix(claims.Issuer, "/") != strings.TrimSuffix(discovery.Issuer, "/") || claims.Subject == "" || claims.Nonce != expectedNonce {
		return nil, common.NewError("OIDC ID token claims are invalid")
	}
	now := time.Now()
	if claims.ExpiresAt <= now.Add(-30*time.Second).Unix() || claims.IssuedAt <= 0 || claims.IssuedAt > now.Add(5*time.Minute).Unix() || (claims.NotBefore != 0 && claims.NotBefore > now.Add(30*time.Second).Unix()) {
		return nil, common.NewError("OIDC ID token is expired or not yet valid")
	}
	audiences := oidcAudiences(claims.Audience)
	if !containsOIDCAudience(audiences, clientID) {
		return nil, common.NewError("OIDC ID token was issued for another client")
	}
	// OIDC Core requires azp to identify this client when an ID token has
	// multiple audiences. Without this check, a token for another relying
	// party sharing an audience list could be replayed at the panel.
	if len(audiences) > 1 && claims.AuthorizedParty != clientID {
		return nil, common.NewError("OIDC ID token authorized party is invalid")
	}
	return &claims, nil
}

type oidcJWKKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	N   string `json:"n"`
	E   string `json:"e"`
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func oidcJWK(ctx context.Context, endpoint, kid string) (any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	resp, err := oidcHTTPClient(10 * time.Second).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("OIDC JWKS returned HTTP %d", resp.StatusCode)
	}
	var set struct {
		Keys []oidcJWKKey `json:"keys"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&set); err != nil {
		return nil, err
	}
	for _, jwk := range set.Keys {
		if jwk.Kid == kid {
			return parseOIDCJWK(jwk)
		}
	}
	return nil, common.NewError("OIDC signing key was not found")
}
func parseOIDCJWK(j oidcJWKKey) (any, error) {
	decode := func(raw string) ([]byte, error) { return base64.RawURLEncoding.DecodeString(raw) }
	switch j.Kty {
	case "RSA":
		n, err := decode(j.N)
		if err != nil {
			return nil, err
		}
		e, err := decode(j.E)
		if err != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("invalid OIDC RSA key")
		}
		exponent := 0
		for _, b := range e {
			exponent = exponent<<8 + int(b)
		}
		key := &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: exponent}
		if len(n) == 0 || exponent < 3 || exponent%2 == 0 || key.N.Sign() <= 0 {
			return nil, errors.New("invalid OIDC RSA key")
		}
		return key, nil
	case "EC":
		if j.Crv != "P-256" {
			return nil, errors.New("unsupported OIDC elliptic curve")
		}
		x, err := decode(j.X)
		if err != nil {
			return nil, err
		}
		y, err := decode(j.Y)
		if err != nil {
			return nil, err
		}
		key := &ecdsa.PublicKey{Curve: elliptic.P256(), X: new(big.Int).SetBytes(x), Y: new(big.Int).SetBytes(y)}
		if !key.Curve.IsOnCurve(key.X, key.Y) {
			return nil, errors.New("invalid OIDC EC key")
		}
		return key, nil
	case "OKP":
		if j.Crv != "Ed25519" {
			return nil, errors.New("unsupported OIDC OKP curve")
		}
		x, err := decode(j.X)
		if err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("invalid OIDC Ed25519 key")
		}
		return ed25519.PublicKey(x), nil
	default:
		return nil, errors.New("unsupported OIDC signing key")
	}
}
func verifyOIDCSignature(alg string, key any, payload, sig []byte) error {
	var hash crypto.Hash
	switch alg {
	case "RS256", "PS256", "ES256":
		hash = crypto.SHA256
	case "RS384", "PS384":
		hash = crypto.SHA384
	case "RS512", "PS512":
		hash = crypto.SHA512
	case "EdDSA":
		// Ed25519 signs the original payload rather than a digest.
	default:
		return errors.New("unsupported OIDC signing algorithm")
	}
	var digest []byte
	if alg != "EdDSA" {
		if !hash.Available() {
			return errors.New("OIDC hash is unavailable")
		}
		h := hash.New()
		_, _ = h.Write(payload)
		digest = h.Sum(nil)
	}
	switch k := key.(type) {
	case *rsa.PublicKey:
		if strings.HasPrefix(alg, "RS") {
			return rsa.VerifyPKCS1v15(k, hash, digest, sig)
		}
		if strings.HasPrefix(alg, "PS") {
			return rsa.VerifyPSS(k, hash, digest, sig, nil)
		}
		return errors.New("OIDC algorithm/key mismatch")
	case *ecdsa.PublicKey:
		if alg != "ES256" || len(sig) != 64 {
			return errors.New("OIDC algorithm/key mismatch")
		}
		if !ecdsa.VerifyASN1(k, digest, joseECDSAToASN1(sig)) {
			return errors.New("OIDC signature verification failed")
		}
		return nil
	case ed25519.PublicKey:
		if alg != "EdDSA" || !ed25519.Verify(k, payload, sig) {
			return errors.New("OIDC signature verification failed")
		}
		return nil
	default:
		return errors.New("unsupported OIDC signing key")
	}
}
func joseECDSAToASN1(raw []byte) []byte {
	n := len(raw) / 2
	r, s := new(big.Int).SetBytes(raw[:n]), new(big.Int).SetBytes(raw[n:])
	rb, sb := r.Bytes(), s.Bytes()
	if len(rb) == 0 {
		rb = []byte{0}
	}
	if len(sb) == 0 {
		sb = []byte{0}
	}
	if rb[0]&0x80 != 0 {
		rb = append([]byte{0}, rb...)
	}
	if sb[0]&0x80 != 0 {
		sb = append([]byte{0}, sb...)
	}
	body := append(append([]byte{0x02, byte(len(rb))}, rb...), append([]byte{0x02, byte(len(sb))}, sb...)...)
	return append([]byte{0x30, byte(len(body))}, body...)
}

func oidcAudiences(raw json.RawMessage) []string {
	var single string
	if json.Unmarshal(raw, &single) == nil && single != "" {
		return []string{single}
	}
	var many []string
	if json.Unmarshal(raw, &many) == nil {
		out := make([]string, 0, len(many))
		for _, value := range many {
			if value != "" {
				out = append(out, value)
			}
		}
		return out
	}
	return nil
}

func containsOIDCAudience(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func resolveOIDCUser(config *model.OIDCProviderConfig, claims *oidcTokenClaims) (*model.User, error) {
	email := strings.ToLower(strings.TrimSpace(claims.Email))
	verifiedEmail := email
	if !validOIDCEmail(verifiedEmail) || (claims.EmailVerified != nil && !*claims.EmailVerified) {
		// Do not use a malformed or explicitly unverified email to satisfy a
		// domain policy, link an existing username, or create an administrator.
		verifiedEmail = ""
	}
	domains := oidcAllowedDomains(config)
	if len(domains) > 0 && (verifiedEmail == "" || !matchesOIDCDomain(verifiedEmail, domains)) {
		return nil, common.NewError("OIDC email is not allowed")
	}

	var user model.User
	if err := database.GetDB().Where("oidc_issuer = ? AND oidc_subject = ?", config.Issuer, claims.Subject).First(&user).Error; err == nil {
		if user.Disabled {
			return nil, common.NewError("OIDC account is disabled")
		}
		return &user, nil
	} else if !database.IsNotFound(err) {
		return nil, err
	}
	// Automatic account linking is intentionally limited to a verified email.
	// preferred_username is not a stable, verified identifier at many providers
	// and must never be able to attach an external identity to a local admin.
	for _, identity := range []string{verifiedEmail} {
		if identity == "" {
			continue
		}
		if err := database.GetDB().Where("LOWER(username) = LOWER(?)", identity).First(&user).Error; err == nil {
			if user.Disabled {
				return nil, common.NewError("OIDC account is disabled")
			}
			if err := database.GetDB().Model(&user).Updates(map[string]any{"oidc_issuer": config.Issuer, "oidc_subject": claims.Subject}).Error; err != nil {
				return nil, err
			}
			user.OIDCIssuer, user.OIDCSubject = config.Issuer, claims.Subject
			return &user, nil
		} else if !database.IsNotFound(err) {
			return nil, err
		}
	}
	if !config.AutoProvision {
		return nil, common.NewError("no matching PHOENIX administrator is linked to this OIDC identity")
	}
	if verifiedEmail == "" {
		return nil, common.NewError("OIDC provider did not supply a verified email for auto-provisioning")
	}
	role := oidcMappedRole(config, claims.Raw)
	password, err := panelcrypto.HashPasswordAsBcrypt(random.Seq(48))
	if err != nil {
		return nil, err
	}
	user = model.User{Username: verifiedEmail, Password: password, Role: role, OIDCIssuer: config.Issuer, OIDCSubject: claims.Subject, CreatedAt: time.Now().UnixMilli()}
	if err := database.GetDB().Create(&user).Error; err != nil {
		return nil, err
	}
	return &user, nil
}
func oidcAllowedDomains(config *model.OIDCProviderConfig) []string {
	var values []string
	_ = json.Unmarshal([]byte(config.AllowedEmailDomains), &values)
	return values
}
func validOIDCEmail(email string) bool {
	parsed, err := mail.ParseAddress(email)
	if err != nil || parsed.Address != email {
		return false
	}
	local, domain, ok := strings.Cut(email, "@")
	return ok && local != "" && domain != "" && !strings.Contains(domain, "@")
}

func matchesOIDCDomain(email string, domains []string) bool {
	_, domain, ok := strings.Cut(email, "@")
	if !ok {
		return false
	}
	for _, allowed := range domains {
		if domain == allowed {
			return true
		}
	}
	return false
}
func oidcMappedRole(config *model.OIDCProviderConfig, raw map[string]any) string {
	role := config.DefaultRole
	var mappings map[string]string
	_ = json.Unmarshal([]byte(config.GroupRoleMappings), &mappings)
	groups := oidcClaimStrings(raw[config.GroupClaim])
	rank := map[string]int{"viewer": 1, "support": 2, "admin": 3}
	for _, group := range groups {
		if candidate, ok := mappings[group]; ok && rank[candidate] > rank[role] {
			role = candidate
		}
	}
	if !validOIDCRole(role) || role == "superadmin" {
		return "viewer"
	}
	return role
}

func oidcClaimStrings(value any) []string {
	switch values := value.(type) {
	case string:
		if values != "" {
			return []string{values}
		}
	case []string:
		return values
	case []any:
		out := make([]string, 0, len(values))
		for _, value := range values {
			if group, ok := value.(string); ok && group != "" {
				out = append(out, group)
			}
		}
		return out
	}
	return nil
}
func secureURLToken(n int) (string, error) {
	bytes := make([]byte, n)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(bytes), nil
}
