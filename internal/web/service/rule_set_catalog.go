package service

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/common"
)

const (
	ruleSetMaxBytes   = 4 << 20
	ruleSetMaxEntries = 100_000
)

var ruleSetNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._ -]{0,63}$`)
var ruleSetLabelPattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,253}$`)

// RFC 6890 and documentation/benchmark ranges are not internet-routable.
// netip's IsPrivate intentionally does not classify every one of these, so
// keep them explicit for the catalog SSRF boundary.
var nonPublicCatalogPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001:2::/48"),
	netip.MustParsePrefix("2001:10::/28"),
	netip.MustParsePrefix("2001:20::/28"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("64:ff9b:1::/48"),
	netip.MustParsePrefix("fec0::/10"),
}

// RuleSetCatalogService manages a safe cache of routing list data. Catalogs
// deliberately store data only; applying a catalog to an Xray routing rule is
// an explicit administrator action in the UI/API rather than an unreviewed
// remote-config execution path.
type RuleSetCatalogService struct{}

type RuleSetCatalogView struct {
	ID             int      `json:"id"`
	Name           string   `json:"name"`
	URL            string   `json:"url"`
	Kind           string   `json:"kind"`
	EntryCount     int      `json:"entryCount"`
	Enabled        bool     `json:"enabled"`
	RefreshMinutes int      `json:"refreshMinutes"`
	LastRefreshAt  int64    `json:"lastRefreshAt"`
	LastSuccessAt  int64    `json:"lastSuccessAt"`
	LastError      string   `json:"lastError,omitempty"`
	SHA256         string   `json:"sha256,omitempty"`
	Entries        []string `json:"entries,omitempty"`
	CreatedAt      int64    `json:"createdAt"`
	UpdatedAt      int64    `json:"updatedAt"`
}

type RuleSetCatalogInput struct {
	Name           string `json:"name" form:"name"`
	URL            string `json:"url" form:"url"`
	Kind           string `json:"kind" form:"kind"`
	Enabled        bool   `json:"enabled" form:"enabled"`
	RefreshMinutes int    `json:"refreshMinutes" form:"refreshMinutes"`
}

func (s RuleSetCatalogService) List(includeEntries bool) ([]RuleSetCatalogView, error) {
	var rows []model.RuleSetCatalog
	if err := database.GetDB().Order("name asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]RuleSetCatalogView, 0, len(rows))
	for i := range rows {
		out = append(out, ruleSetCatalogView(&rows[i], includeEntries))
	}
	return out, nil
}

func (s RuleSetCatalogService) Get(id int, includeEntries bool) (*RuleSetCatalogView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid rule-set catalog id")
	}
	var row model.RuleSetCatalog
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	view := ruleSetCatalogView(&row, includeEntries)
	return &view, nil
}

func (s RuleSetCatalogService) Create(in RuleSetCatalogInput) (*RuleSetCatalogView, error) {
	row, err := validateRuleSetCatalogInput(in)
	if err != nil {
		return nil, err
	}
	row.Entries = "[]"
	if err := database.GetDB().Create(row).Error; err != nil {
		return nil, err
	}
	view := ruleSetCatalogView(row, false)
	return &view, nil
}

func (s RuleSetCatalogService) Update(id int, in RuleSetCatalogInput) (*RuleSetCatalogView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid rule-set catalog id")
	}
	next, err := validateRuleSetCatalogInput(in)
	if err != nil {
		return nil, err
	}
	var row model.RuleSetCatalog
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	// Never present a cache from a different source or format as if it had
	// been reviewed from the newly configured catalog.
	if row.URL != next.URL || row.Kind != next.Kind {
		row.ETag = ""
		row.Entries = "[]"
		row.SHA256 = ""
		row.LastRefreshAt = 0
		row.LastSuccessAt = 0
		row.LastError = ""
	}
	row.Name, row.URL, row.Kind = next.Name, next.URL, next.Kind
	row.Enabled, row.RefreshMinutes = next.Enabled, next.RefreshMinutes
	if err := database.GetDB().Save(&row).Error; err != nil {
		return nil, err
	}
	view := ruleSetCatalogView(&row, false)
	return &view, nil
}

func (s RuleSetCatalogService) Delete(id int) error {
	if id <= 0 {
		return common.NewError("invalid rule-set catalog id")
	}
	result := database.GetDB().Delete(&model.RuleSetCatalog{}, id)
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected == 0 {
		return errors.New("rule-set catalog not found")
	}
	return nil
}

func (s RuleSetCatalogService) Refresh(id int) (*RuleSetCatalogView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid rule-set catalog id")
	}
	var row model.RuleSetCatalog
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	return s.refresh(&row)
}

// RefreshDue refreshes enabled catalogs whose interval elapsed. A failed
// source retains the last known-good cache and records a bounded error string.
func (s RuleSetCatalogService) RefreshDue(now time.Time) error {
	var rows []model.RuleSetCatalog
	if err := database.GetDB().Where("enabled = ?", true).Find(&rows).Error; err != nil {
		return err
	}
	var errs []error
	for i := range rows {
		interval := time.Duration(rows[i].RefreshMinutes) * time.Minute
		if interval <= 0 {
			interval = 6 * time.Hour
		}
		if rows[i].LastRefreshAt > 0 && now.Sub(time.UnixMilli(rows[i].LastRefreshAt)) < interval {
			continue
		}
		if _, err := s.refresh(&rows[i]); err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", rows[i].Name, err))
		}
	}
	return errors.Join(errs...)
}

func (s RuleSetCatalogService) refresh(row *model.RuleSetCatalog) (*RuleSetCatalogView, error) {
	now := time.Now().UnixMilli()
	source, err := validateRuleSetURL(row.URL)
	if err == nil {
		err = ensurePublicCatalogHost(source.Hostname())
	}
	if err == nil {
		err = s.fetchAndStore(row, source, now)
	}
	if err != nil {
		message := err.Error()
		if len(message) > 512 {
			message = message[:512]
		}
		_ = database.GetDB().Model(&model.RuleSetCatalog{}).Where("id = ?", row.ID).Updates(map[string]any{"last_refresh_at": now, "last_error": message}).Error
		return nil, err
	}
	var persisted model.RuleSetCatalog
	if err := database.GetDB().First(&persisted, row.ID).Error; err != nil {
		return nil, err
	}
	view := ruleSetCatalogView(&persisted, false)
	return &view, nil
}

func (s RuleSetCatalogService) fetchAndStore(row *model.RuleSetCatalog, source *url.URL, now int64) error {
	request, err := http.NewRequestWithContext(context.Background(), http.MethodGet, source.String(), nil)
	if err != nil {
		return err
	}
	request.Header.Set("Accept", "text/plain, application/json;q=0.9, */*;q=0.1")
	request.Header.Set("User-Agent", "PHOENIX-RuleSetCatalog/1")
	if row.ETag != "" {
		request.Header.Set("If-None-Match", row.ETag)
	}
	response, err := safeCatalogHTTPClient().Do(request)
	if err != nil {
		return fmt.Errorf("fetch catalog: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode == http.StatusNotModified {
		return database.GetDB().Model(&model.RuleSetCatalog{}).Where("id = ?", row.ID).Updates(map[string]any{"last_refresh_at": now, "last_success_at": now, "last_error": ""}).Error
	}
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("catalog returned HTTP %d", response.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, ruleSetMaxBytes+1))
	if err != nil {
		return err
	}
	if len(body) > ruleSetMaxBytes {
		return common.NewError("catalog exceeds 4 MiB limit")
	}
	entries, err := normalizeRuleSetEntries(row.Kind, body)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(encoded)
	etag := response.Header.Get("ETag")
	return database.GetDB().Model(&model.RuleSetCatalog{}).Where("id = ?", row.ID).Updates(map[string]any{
		"entries":         string(encoded),
		"etag":            etag,
		"sha256":          hex.EncodeToString(sum[:]),
		"last_refresh_at": now,
		"last_success_at": now,
		"last_error":      "",
	}).Error
}

func validateRuleSetCatalogInput(in RuleSetCatalogInput) (*model.RuleSetCatalog, error) {
	name := strings.TrimSpace(in.Name)
	if !ruleSetNamePattern.MatchString(name) {
		return nil, common.NewError("rule-set name must be 1 to 64 safe characters")
	}
	source, err := validateRuleSetURL(in.URL)
	if err != nil {
		return nil, err
	}
	kind := strings.ToLower(strings.TrimSpace(in.Kind))
	if kind != "domain" && kind != "ip" {
		return nil, common.NewError("rule-set kind must be domain or ip")
	}
	refresh := in.RefreshMinutes
	if refresh == 0 {
		refresh = 360
	}
	if refresh < 5 || refresh > 7*24*60 {
		return nil, common.NewError("refresh interval must be between 5 minutes and 7 days")
	}
	return &model.RuleSetCatalog{Name: name, URL: source.String(), Kind: kind, Enabled: in.Enabled, RefreshMinutes: refresh}, nil
}

func validateRuleSetURL(raw string) (*url.URL, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u == nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
		return nil, common.NewError("rule-set URL must be an absolute HTTPS URL without credentials or fragment")
	}
	if u.Port() != "" && u.Port() != "443" {
		return nil, common.NewError("rule-set URL must use HTTPS port 443")
	}
	return u, nil
}

func normalizeRuleSetEntries(kind string, body []byte) ([]string, error) {
	var jsonEntries []string
	if json.Unmarshal(body, &jsonEntries) == nil {
		return normalizeRuleSetStrings(kind, jsonEntries)
	}
	scanner := bufio.NewScanner(strings.NewReader(string(body)))
	scanner.Buffer(make([]byte, 4096), 16*1024)
	entries := make([]string, 0)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if idx := strings.IndexByte(line, '#'); idx >= 0 {
			line = strings.TrimSpace(line[:idx])
		}
		if idx := strings.Index(line, "//"); idx == 0 {
			continue
		}
		if line == "" || strings.HasPrefix(line, "!") || strings.HasPrefix(line, ";") {
			continue
		}
		entries = append(entries, strings.Fields(line)[0])
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return normalizeRuleSetStrings(kind, entries)
}

func normalizeRuleSetStrings(kind string, entries []string) ([]string, error) {
	if len(entries) > ruleSetMaxEntries {
		return nil, common.NewError("catalog has too many entries")
	}
	set := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		var normalized string
		var err error
		switch kind {
		case "domain":
			normalized, err = normalizeDomainRuleEntry(entry)
		case "ip":
			normalized, err = normalizeIPRuleEntry(entry)
		default:
			return nil, common.NewError("unknown rule-set kind")
		}
		if err != nil {
			return nil, err
		}
		set[normalized] = struct{}{}
	}
	out := make([]string, 0, len(set))
	for entry := range set {
		out = append(out, entry)
	}
	sort.Strings(out)
	return out, nil
}

func normalizeDomainRuleEntry(entry string) (string, error) {
	entry = strings.ToLower(strings.TrimSpace(entry))
	for _, prefix := range []string{"domain:", "full:"} {
		if suffix, ok := strings.CutPrefix(entry, prefix); ok {
			clean, err := ValidateCertDomain(suffix)
			if err != nil {
				return "", common.NewError("invalid domain catalog entry")
			}
			return prefix + clean, nil
		}
	}
	if suffix, ok := strings.CutPrefix(entry, "keyword:"); ok {
		suffix = strings.TrimSpace(suffix)
		if suffix == "" || len(suffix) > 253 || strings.ContainsAny(suffix, "\r\n\x00") {
			return "", common.NewError("invalid keyword catalog entry")
		}
		return "keyword:" + suffix, nil
	}
	if suffix, ok := strings.CutPrefix(entry, "geosite:"); ok {
		if !ruleSetLabelPattern.MatchString(suffix) {
			return "", common.NewError("invalid geosite catalog entry")
		}
		return "geosite:" + suffix, nil
	}
	clean, err := ValidateCertDomain(entry)
	if err != nil {
		return "", common.NewError("invalid domain catalog entry")
	}
	return "domain:" + clean, nil
}

func normalizeIPRuleEntry(entry string) (string, error) {
	entry = strings.ToLower(strings.TrimSpace(entry))
	if suffix, ok := strings.CutPrefix(entry, "geoip:"); ok {
		if !ruleSetLabelPattern.MatchString(suffix) {
			return "", common.NewError("invalid geoip catalog entry")
		}
		return "geoip:" + suffix, nil
	}
	if prefix, err := netip.ParsePrefix(entry); err == nil {
		return prefix.Masked().String(), nil
	}
	if addr, err := netip.ParseAddr(entry); err == nil {
		return addr.Unmap().String(), nil
	}
	return "", common.NewError("invalid IP catalog entry")
}

func ruleSetCatalogView(row *model.RuleSetCatalog, includeEntries bool) RuleSetCatalogView {
	var entries []string
	_ = json.Unmarshal([]byte(row.Entries), &entries)
	view := RuleSetCatalogView{ID: row.ID, Name: row.Name, URL: row.URL, Kind: row.Kind, EntryCount: len(entries), Enabled: row.Enabled, RefreshMinutes: row.RefreshMinutes, LastRefreshAt: row.LastRefreshAt, LastSuccessAt: row.LastSuccessAt, LastError: row.LastError, SHA256: row.SHA256, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
	if includeEntries {
		view.Entries = entries
	}
	return view
}

func ensurePublicCatalogHost(host string) error {
	if host == "" || strings.EqualFold(host, "localhost") {
		return common.NewError("rule-set host is not publicly routable")
	}
	if ip, err := netip.ParseAddr(host); err == nil {
		if !isPublicCatalogIP(ip) {
			return common.NewError("rule-set host is not publicly routable")
		}
		return nil
	}
	ips, err := net.LookupIP(host)
	if err != nil || len(ips) == 0 {
		return common.NewError("rule-set host cannot be resolved")
	}
	for _, ip := range ips {
		parsed, ok := netip.AddrFromSlice(ip)
		if !ok || !isPublicCatalogIP(parsed) {
			return common.NewError("rule-set host resolves to a non-public address")
		}
	}
	return nil
}

func isPublicCatalogIP(ip netip.Addr) bool {
	ip = ip.Unmap()
	if !ip.IsValid() || !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
		return false
	}
	for _, prefix := range nonPublicCatalogPrefixes {
		if prefix.Contains(ip) {
			return false
		}
	}
	if ip.Is4() && ip.As4()[0] >= 240 {
		return false
	}
	return true
}

func safeCatalogHTTPClient() *http.Client {
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
			if err := ensurePublicCatalogHost(host); err != nil {
				return nil, err
			}
			ips, err := net.LookupIP(host)
			if err != nil {
				return nil, err
			}
			var lastErr error
			for _, ip := range ips {
				parsed, ok := netip.AddrFromSlice(ip)
				if !ok || !isPublicCatalogIP(parsed) {
					return nil, errors.New("rule-set target became non-public")
				}
				conn, err := dialer.DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
				if err == nil {
					return conn, nil
				}
				lastErr = err
			}
			if lastErr == nil {
				lastErr = errors.New("rule-set target has no public address")
			}
			return nil, lastErr
		},
	}
	return &http.Client{Transport: transport, Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 3 {
			return errors.New("too many catalog redirects")
		}
		if _, err := validateRuleSetURL(req.URL.String()); err != nil {
			return err
		}
		return ensurePublicCatalogHost(req.URL.Hostname())
	}}
}
