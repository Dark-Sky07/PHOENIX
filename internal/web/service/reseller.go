package service

import (
	"errors"
	"fmt"
	"math"
	"math/big"
	"net"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/logger"
	"github.com/Dark-Sky07/phoenix/internal/util/crypto"
	moneycurrency "github.com/Dark-Sky07/phoenix/internal/util/currency"
	"github.com/Dark-Sky07/phoenix/internal/xray"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// ResellerService implements the نمایندگی (reseller) feature: panel sub-accounts
// that own a subset of inbounds/clients, bounded by inbounds/clients/traffic
// quotas and billed through a prepaid balance.
type ResellerService struct {
	inboundService InboundService
	clientService  ClientService
	settingService SettingService
}

const bytesPerGB = 1024 * 1024 * 1024

// ResellerStat is one reseller's live usage snapshot. It is what both the
// admin's reseller table and the reseller's own dashboard render.
type ResellerStat struct {
	Reseller           *model.Reseller `json:"reseller"`
	Currency           string          `json:"currency"` // Currency denomination for reseller financial values.
	InboundCount       int             `json:"inboundCount"`
	ClientCount        int             `json:"clientCount"`
	OnlineCount        int             `json:"onlineCount"`
	PendingCreations   int64           `json:"pendingCreations"`
	ProcessingRenewals int64           `json:"processingRenewals"`
	HasUnlimitedClient bool            `json:"hasUnlimitedClient"`
	UsedTraffic        int64           `json:"usedTraffic"`
	AllocatedTraffic   int64           `json:"allocatedTraffic"`
	RemainingTraffic   int64           `json:"remainingTraffic"` // TrafficLimit - AllocatedTraffic (0 when unlimited)
	Cost               float64         `json:"cost"`             // UsedTraffic priced at PricePerGB
	Balance            float64         `json:"balance"`          // Deposit - Cost (negative = debt)
	Expired            bool            `json:"expired"`
	OverQuota          bool            `json:"overQuota"` // Any quota exceeded
	Disabled           bool            `json:"disabled"`  // Auto-disabled because a quota ran out
}

// ResellerClientRow is a single client inside a reseller report.
type ResellerClientRow struct {
	Email      string  `json:"email"`
	Comment    string  `json:"comment,omitempty"`
	Group      string  `json:"group,omitempty"`
	Enable     bool    `json:"enable"`
	TotalBytes int64   `json:"totalBytes"`
	ExpiryTime int64   `json:"expiryTime"`
	Up         int64   `json:"up"`
	Down       int64   `json:"down"`
	Used       int64   `json:"used"`
	Cost       float64 `json:"cost"`
	InboundIds []int   `json:"inboundIds"`
	SubID      string  `json:"subId,omitempty"`
}

// ResellerReport is the full billing/usage report for one reseller.
type ResellerReport struct {
	Stat            *ResellerStat                  `json:"stat"`
	Clients         []ResellerClientRow            `json:"clients"`
	Transactions    []*model.ResellerTransaction   `json:"transactions"`
	TrafficArchives []model.ResellerTrafficArchive `json:"trafficArchives"`
	// Populated by the admin report handler only; self-service never exposes
	// operational details of potentially unowned client records.
	CreationIssues []ResellerCreationIssue  `json:"creationIssues,omitempty"`
	RenewalIssues  []ProcessingRenewalIssue `json:"renewalIssues,omitempty"`
}

// ResellerSalePrice is a reseller-declared price, in the currency's minor
// units. It is not a verified payment. Absence means the amount is unknown.
type ResellerSalePrice struct {
	AmountMinor int64  `json:"amountMinor"`
	Currency    string `json:"currency"`
}

func ValidateResellerSalePrice(price *ResellerSalePrice) error {
	if price == nil {
		return nil
	}
	if price.AmountMinor < 0 || price.AmountMinor > 9007199254740991 {
		return errors.New("invalid declared sale amount")
	}
	code, ok := moneycurrency.Normalize(price.Currency)
	if !ok {
		return errors.New("unsupported sale currency")
	}
	price.Currency = code
	return nil
}

// addMinorAmount preserves exact totals across int64 and JavaScript number limits.
func addMinorAmount(existing string, amount int64) string {
	value := new(big.Int)
	if existing != "" {
		value.SetString(existing, 10)
	}
	return value.Add(value, big.NewInt(amount)).String()
}

// ResellerSalesBucket is one UTC day, month, or year of newly created client
// accounts and the traffic quotas allocated to those accounts at sale time.
type ResellerSalesBucket struct {
	Period          string            `json:"period"`
	StartAt         int64             `json:"startAt"`
	Sales           int64             `json:"sales"`
	HistoricalSales int64             `json:"historicalSales"`
	TrafficBytes    int64             `json:"trafficBytes"`
	UnknownPrices   int64             `json:"unknownPrices"`
	AmountsMinor    map[string]string `json:"amountsMinor"`
}

// ResellerSalesReport reports snapshots and declared prices, not payments.
type ResellerSalesReport struct {
	ResellerId        int                   `json:"resellerId"`
	Period            string                `json:"period"`
	From              int64                 `json:"from,omitempty"`
	To                int64                 `json:"to,omitempty"`
	TotalSales        int64                 `json:"totalSales"`
	HistoricalSales   int64                 `json:"historicalSales"`
	TotalTrafficBytes int64                 `json:"totalTrafficBytes"`
	UnknownPrices     int64                 `json:"unknownPrices"`
	AmountsMinor      map[string]string     `json:"amountsMinor"`
	Buckets           []ResellerSalesBucket `json:"buckets"`
}

// ---------------------------------------------------------------------------
// CRUD
// ---------------------------------------------------------------------------

// List returns every reseller together with its live usage numbers.
func (s *ResellerService) List() ([]*ResellerStat, error) {
	db := database.GetDB()
	var resellers []*model.Reseller
	if err := db.Model(model.Reseller{}).Order("id ASC").Find(&resellers).Error; err != nil {
		return nil, err
	}
	out := make([]*ResellerStat, 0, len(resellers))
	for _, r := range resellers {
		stat, err := s.Stat(r)
		if err != nil {
			return nil, err
		}
		out = append(out, stat)
	}
	return out, nil
}

// Get loads one reseller by id.
func (s *ResellerService) Get(id int) (*model.Reseller, error) {
	db := database.GetDB()
	reseller := &model.Reseller{}
	err := db.Model(model.Reseller{}).Where("id = ?", id).First(reseller).Error
	if err != nil {
		return nil, err
	}
	return reseller, nil
}

// SubscriptionDomain returns the panel-wide subscription hostname for collision checks.
func (s *ResellerService) SubscriptionDomain() (string, error) {
	return s.settingService.GetSubDomain()
}

// GetByUsername loads one reseller by its login name.
func (s *ResellerService) GetByUsername(username string) (*model.Reseller, error) {
	db := database.GetDB()
	reseller := &model.Reseller{}
	err := db.Model(model.Reseller{}).Where("username = ?", username).First(reseller).Error
	if err != nil {
		return nil, err
	}
	return reseller, nil
}

func (s *ResellerService) StatFor(id int) (*ResellerStat, error) {
	reseller, err := s.Get(id)
	if err != nil {
		return nil, err
	}
	return s.Stat(reseller)
}

// Add creates a reseller. The password is stored as a bcrypt hash.
// Billing fields (pricePerGb, deposit) are intentionally ignored and forced to 0
// because the panel now uses attach-inbound instead of billing.
func (s *ResellerService) Add(reseller *model.Reseller, password string) error {
	if reseller == nil {
		return errors.New("reseller is required")
	}
	reseller.Username = strings.TrimSpace(reseller.Username)
	if reseller.Username == "" {
		return errors.New("username can not be empty")
	}
	if strings.TrimSpace(password) == "" {
		return errors.New("password can not be empty")
	}
	if err := s.validateUsername(reseller.Username, 0); err != nil {
		return err
	}
	hashed, err := crypto.HashPasswordAsBcrypt(password)
	if err != nil {
		return err
	}
	reseller.Password = hashed
	if reseller.Name == "" {
		reseller.Name = reseller.Username
	}
	// Billing: the per-GB price is part of the contract and may be set at
	// creation, but deposits only enter through the audited ledger
	// (AdjustBalance) so the transaction history stays truthful.
	reseller.Deposit = 0
	reseller.LoginEpoch = 1
	reseller.CreatedAt = time.Now().UnixMilli()
	reseller.UpdatedAt = reseller.CreatedAt
	db := database.GetDB()
	return db.Model(model.Reseller{}).Create(reseller).Error
}

// Update saves the editable fields of a reseller. An empty password leaves the
// stored one untouched.
func (s *ResellerService) Update(reseller *model.Reseller, password string) error {
	if reseller == nil || reseller.Id <= 0 {
		return errors.New("reseller is required")
	}
	existing, err := s.Get(reseller.Id)
	if err != nil {
		return err
	}
	reseller.Username = strings.TrimSpace(reseller.Username)
	if reseller.Username == "" {
		return errors.New("username can not be empty")
	}
	if err := s.validateUsername(reseller.Username, reseller.Id); err != nil {
		return err
	}
	if reseller.Name == "" {
		reseller.Name = reseller.Username
	}

	updates := map[string]any{
		"username":      reseller.Username,
		"name":          reseller.Name,
		"comment":       reseller.Comment,
		"enable":        reseller.Enable,
		"traffic_limit": reseller.TrafficLimit,
		"client_limit":  reseller.ClientLimit,
		"expiry_time":   reseller.ExpiryTime,
		"price_per_gb":  reseller.PricePerGB,
		"updated_at":    time.Now().UnixMilli(),
	}
	if strings.TrimSpace(password) != "" {
		hashed, err := crypto.HashPasswordAsBcrypt(password)
		if err != nil {
			return err
		}
		updates["password"] = hashed
		updates["login_epoch"] = existing.LoginEpoch + 1
	}
	db := database.GetDB()
	return db.Model(model.Reseller{}).Where("id = ?", reseller.Id).Updates(updates).Error
}

// NormalizeResellerBranding trims and validates reseller-controlled brand fields.
// Logos may be same-origin absolute paths or HTTPS URLs; colors are six-digit hex.
func NormalizeResellerBranding(name, logo, color string) (string, string, string, error) {
	name = strings.TrimSpace(name)
	if utf8.RuneCountInString(name) > 80 {
		return "", "", "", errors.New("brand name must be at most 80 characters")
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return "", "", "", errors.New("brand name contains invalid characters")
		}
	}

	logo = strings.TrimSpace(logo)
	if len(logo) > 2048 || strings.ContainsAny(logo, string([]byte{92, 13, 10})) {
		return "", "", "", errors.New("brand logo must be a valid URL")
	}
	if logo != "" {
		parsed, err := url.Parse(logo)
		if err != nil {
			return "", "", "", errors.New("brand logo must be a valid URL")
		}
		if strings.HasPrefix(logo, "/") {
			if strings.HasPrefix(logo, "//") || parsed.IsAbs() || parsed.Host != "" {
				return "", "", "", errors.New("brand logo path must stay on this site")
			}
		} else if parsed.Scheme != "https" || parsed.Hostname() == "" || parsed.User != nil {
			return "", "", "", errors.New("brand logo must use HTTPS or a same-site path")
		}
	}

	color = strings.TrimSpace(color)
	if color != "" {
		if len(color) != 7 || color[0] != '#' {
			return "", "", "", errors.New("brand color must be a six-digit hex color")
		}
		for _, r := range color[1:] {
			if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
				return "", "", "", errors.New("brand color must be a six-digit hex color")
			}
		}
		color = strings.ToUpper(color)
	}
	return name, logo, color, nil
}

// UpdateBranding lets a reseller customize the appearance of its own panel and
// public invitation pages without changing any account or quota settings.
func (s *ResellerService) UpdateBranding(resellerID int, name, logo, color string) error {
	if resellerID <= 0 {
		return errors.New("reseller is required")
	}
	name, logo, color, err := NormalizeResellerBranding(name, logo, color)
	if err != nil {
		return err
	}
	return database.GetDB().Model(&model.Reseller{}).Where("id = ?", resellerID).Updates(map[string]any{
		"brand_name": name, "brand_logo": logo, "brand_color": color, "updated_at": time.Now().UnixMilli(),
	}).Error
}

// CheckSubDomainAvailable validates and checks a reseller's custom subscription hostname.
func (s *ResellerService) CheckSubDomainAvailable(resellerID int, value string) (string, error) {
	domain, err := ValidateCertDomain(value)
	if err != nil {
		return "", err
	}
	if net.ParseIP(domain) != nil {
		return "", errors.New("subscription domain must be a valid DNS hostname")
	}
	globalDomain, err := s.SubscriptionDomain()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(globalDomain) != "" {
		globalDomain, err = ValidateCertDomain(globalDomain)
		if err != nil {
			return "", fmt.Errorf("configured panel subscription domain is invalid: %w", err)
		}
		if globalDomain == domain {
			return "", errors.New("subscription domain is already used by the panel")
		}
	}
	var existing []model.Reseller
	if err := database.GetDB().Select("id, sub_domain").Where("id <> ? AND sub_domain <> ''", resellerID).Find(&existing).Error; err != nil {
		return "", err
	}
	for _, reseller := range existing {
		if assigned, err := ValidateCertDomain(reseller.SubDomain); err == nil && assigned == domain {
			return "", errors.New("subscription domain is already assigned to another reseller")
		}
	}
	return domain, nil
}

// NormalizeResellerBrandSettings validates visual branding before it is
// persisted or a certificate is requested.
func NormalizeResellerBrandSettings(name, logo, color, theme string) (string, string, string, string, error) {
	name, logo, color, err := NormalizeResellerBranding(name, logo, color)
	if err != nil {
		return "", "", "", "", err
	}
	theme = strings.ToLower(strings.TrimSpace(theme))
	if theme != "" && theme != "default" && theme != "ocean" && theme != "forest" && theme != "sunset" {
		return "", "", "", "", errors.New("unsupported brand theme")
	}
	return name, logo, color, theme, nil
}

// UpdateBrandSettings validates and persists the visual brand plus an optional
// per-reseller subscription hostname. The hostname is unique across resellers;
// certificate issuance is performed by the controller before this is called.
func (s *ResellerService) UpdateBrandSettings(resellerID int, name, logo, color, theme, domain string) error {
	if resellerID <= 0 {
		return errors.New("reseller is required")
	}
	name, logo, color, theme, err := NormalizeResellerBrandSettings(name, logo, color, theme)
	if err != nil {
		return err
	}
	domain = strings.TrimSpace(domain)
	if domain != "" {
		domain, err = s.CheckSubDomainAvailable(resellerID, domain)
		if err != nil {
			return err
		}
	}
	return database.GetDB().Model(&model.Reseller{}).Where("id = ?", resellerID).Updates(map[string]any{
		"brand_name": name, "brand_logo": logo, "brand_color": color,
		"brand_theme": theme, "sub_domain": domain, "updated_at": time.Now().UnixMilli(),
	}).Error
}

// Delete removes a reseller, its ownership mappings and its ledger. The
// inbounds/clients themselves are kept: they simply fall back to the admin.
func (s *ResellerService) Delete(id int) error {
	db := database.GetDB()
	return db.Transaction(func(tx *gorm.DB) error {
		for _, mdl := range []any{&model.Reseller{}, &model.ResellerInbound{}, &model.ResellerClient{}, &model.ResellerTransaction{}, &model.ResellerSale{}, &model.ResellerHistoricalSale{}, &model.ResellerTrafficArchive{}} {
			q := tx
			if _, isReseller := mdl.(*model.Reseller); isReseller {
				if err := q.Where("id = ?", id).Delete(mdl).Error; err != nil {
					return err
				}
				continue
			}
			if err := q.Where("reseller_id = ?", id).Delete(mdl).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// SetEnable enables/disables a reseller. Disabling also cuts off every inbound
// the reseller owns and invalidates its active sessions.
func (s *ResellerService) SetEnable(id int, enable bool) (bool, error) {
	reseller, err := s.Get(id)
	if err != nil {
		return false, err
	}
	if reseller.Enable == enable {
		return false, nil
	}
	db := database.GetDB()
	updates := map[string]any{
		"enable":      enable,
		"updated_at":  time.Now().UnixMilli(),
		"login_epoch": reseller.LoginEpoch + 1,
	}
	if err := db.Model(model.Reseller{}).Where("id = ?", id).Updates(updates).Error; err != nil {
		return false, err
	}
	needRestart := false
	ids, err := s.OwnedInboundIds(id)
	if err != nil {
		return false, err
	}
	for _, inboundId := range ids {
		var otherResellers int64
		if err := db.Model(model.ResellerInbound{}).
			Where("inbound_id = ? AND reseller_id <> ?", inboundId, id).
			Count(&otherResellers).Error; err != nil {
			logger.Warningf("reseller %d: failed to check shared inbound %d: %v", id, inboundId, err)
			continue
		}
		if otherResellers > 0 {
			// Inbound is shared with other resellers — never toggle the whole
			// inbound; toggle only this reseller's own isolated clients below.
			continue
		}
		changed, err := s.inboundService.SetInboundEnable(inboundId, enable)
		if err != nil {
			logger.Warningf("reseller %d: failed to set inbound %d enable=%v: %v", id, inboundId, enable, err)
			continue
		}
		needRestart = needRestart || changed
	}
	if isolated, err := s.OwnedIsolatedAssociatedEmailSet(id); err == nil {
		for email := range isolated {
			if _, changed, err := s.clientService.SetClientEnableByEmail(&s.inboundService, email, enable); err == nil {
				needRestart = needRestart || changed
			}
		}
	}
	return needRestart, nil
}

// ChangePassword updates a reseller's login password and invalidates its other
// sessions.
func (s *ResellerService) ChangePassword(id int, newPassword string) error {
	if strings.TrimSpace(newPassword) == "" {
		return errors.New("password can not be empty")
	}
	hashed, err := crypto.HashPasswordAsBcrypt(newPassword)
	if err != nil {
		return err
	}
	db := database.GetDB()
	return db.Model(model.Reseller{}).Where("id = ?", id).Updates(map[string]any{
		"password":    hashed,
		"login_epoch": gorm.Expr("login_epoch + 1"),
		"updated_at":  time.Now().UnixMilli(),
	}).Error
}

// CheckReseller authenticates a reseller login.
func (s *ResellerService) CheckReseller(username string, password string) (*model.Reseller, error) {
	reseller, err := s.GetByUsername(strings.TrimSpace(username))
	if err != nil {
		if database.IsNotFound(err) {
			return nil, errors.New("invalid credentials")
		}
		return nil, err
	}
	if !crypto.CheckPasswordHash(reseller.Password, password) {
		return nil, errors.New("invalid credentials")
	}
	if !reseller.Enable {
		return nil, errors.New("reseller account is disabled")
	}
	if IsExpired(reseller.ExpiryTime) {
		return nil, errors.New("reseller account has expired")
	}
	return reseller, nil
}

// IsExpired reports whether a unix-ms expiry timestamp is in the past. Zero
// means "never expires".
func IsExpired(expiryTime int64) bool {
	return expiryTime > 0 && expiryTime <= time.Now().UnixMilli()
}

func (s *ResellerService) validateUsername(username string, selfId int) error {
	db := database.GetDB()
	var count int64
	if err := db.Model(model.Reseller{}).
		Where("username = ? AND id <> ?", username, selfId).
		Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("username %q is already used by another reseller", username)
	}
	// The admin account always wins at login, so a clashing reseller username
	// would be unreachable. Reject it up-front instead.
	if err := db.Model(model.User{}).Where("username = ?", username).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return fmt.Errorf("username %q is already used by the panel admin", username)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Ownership
// ---------------------------------------------------------------------------

// OwnedInboundIds returns the ids of the inbounds assigned to a reseller.
func (s *ResellerService) OwnedInboundIds(resellerId int) ([]int, error) {
	db := database.GetDB()
	var ids []int
	err := db.Model(model.ResellerInbound{}).
		Where("reseller_id = ?", resellerId).
		Order("inbound_id ASC").
		Pluck("inbound_id", &ids).Error
	if err != nil {
		return nil, err
	}
	return ids, nil
}

// OwnedInboundIdSet is the set-shaped variant of OwnedInboundIds.
func (s *ResellerService) OwnedInboundIdSet(resellerId int) (map[int]struct{}, error) {
	ids, err := s.OwnedInboundIds(resellerId)
	if err != nil {
		return nil, err
	}
	set := make(map[int]struct{}, len(ids))
	for _, id := range ids {
		set[id] = struct{}{}
	}
	return set, nil
}

// OwnedEmailSet returns only the client emails explicitly mapped to a
// reseller. Owning an inbound grants access to that inbound's configuration,
// but never implicitly grants access to every client attached to it.
func (s *ResellerService) OwnedEmailSet(resellerId int) (map[string]struct{}, error) {
	emails, err := s.OwnedEmails(resellerId)
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		key := strings.ToLower(strings.TrimSpace(email))
		if key != "" {
			set[key] = struct{}{}
		}
	}
	return set, nil
}

// OwnedAssociatedEmailSet returns the explicitly mapped clients that are also
// attached to at least one inbound owned by the same reseller. It is useful
// for association-aware identity/configuration paths. Aggregate traffic,
// online state, and last-online callers must use
// OwnedIsolatedAssociatedEmailSet because ClientTraffic is email-level and a
// shared admin association cannot be partitioned safely.
func (s *ResellerService) OwnedAssociatedEmailSet(resellerId int) (map[string]struct{}, error) {
	var emails []string
	db := database.GetDB()
	err := db.Table("reseller_clients AS rc").
		Select("DISTINCT LOWER(TRIM(c.email))").
		Joins("JOIN clients AS c ON LOWER(TRIM(c.email)) = LOWER(TRIM(rc.email))").
		Joins("JOIN client_inbounds AS ci ON ci.client_id = c.id").
		Joins("JOIN reseller_inbounds AS ri ON ri.inbound_id = ci.inbound_id AND ri.reseller_id = rc.reseller_id").
		Where("rc.reseller_id = ?", resellerId).
		Scan(&emails).Error
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		if key := transferEmailKey(email); key != "" {
			set[key] = struct{}{}
		}
	}
	return set, nil
}

// OwnedIsolatedAssociatedEmailSet returns mapped clients whose every current
// inbound association is owned by this reseller and that have at least one
// such association. ClientTraffic is one email-level aggregate, so a client
// shared with an admin-owned inbound cannot safely expose or mutate traffic,
// online state, or last-online data from a reseller session.
func (s *ResellerService) OwnedIsolatedAssociatedEmailSet(resellerId int) (map[string]struct{}, error) {
	var emails []string
	db := database.GetDB()
	err := db.Table("reseller_clients AS rc").
		Select("DISTINCT LOWER(TRIM(c.email))").
		Joins("JOIN clients AS c ON LOWER(TRIM(c.email)) = LOWER(TRIM(rc.email))").
		Joins("JOIN client_inbounds AS ci ON ci.client_id = c.id").
		Joins("JOIN reseller_inbounds AS ri ON ri.inbound_id = ci.inbound_id AND ri.reseller_id = rc.reseller_id").
		Where("rc.reseller_id = ?", resellerId).
		Where(`NOT EXISTS (
			SELECT 1 FROM client_inbounds AS foreign_ci
			WHERE foreign_ci.client_id = c.id
			  AND NOT EXISTS (
				SELECT 1 FROM reseller_inbounds AS foreign_ri
				WHERE foreign_ri.reseller_id = rc.reseller_id
				  AND foreign_ri.inbound_id = foreign_ci.inbound_id
			)
		)`).
		Scan(&emails).Error
	if err != nil {
		return nil, err
	}
	set := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		if key := transferEmailKey(email); key != "" {
			set[key] = struct{}{}
		}
	}
	return set, nil
}

// OwnedEmails lists only the client emails explicitly mapped to a reseller.
// Keep this query independent from reseller_inbounds: inbound ownership is a
// separate capability and must not become a transitive client grant.
func (s *ResellerService) OwnedEmails(resellerId int) ([]string, error) {
	db := database.GetDB()
	var mapped []string
	if err := db.Model(model.ResellerClient{}).
		Where("reseller_id = ?", resellerId).
		Order("email ASC").
		Pluck("email", &mapped).Error; err != nil {
		return nil, err
	}
	if len(mapped) == 0 {
		return []string{}, nil
	}

	// Older databases may contain a mapping with incidental whitespace or
	// different casing. Resolve it to the canonical client email so every
	// reseller representation (lists, reports, transfer scopes) uses the same
	// identity, while retaining a trimmed fallback for a dangling mapping.
	keys := make([]string, 0, len(mapped))
	seenKeys := make(map[string]struct{}, len(mapped))
	for _, email := range mapped {
		key := strings.ToLower(strings.TrimSpace(email))
		if key == "" {
			continue
		}
		if _, seen := seenKeys[key]; !seen {
			seenKeys[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	canonical := make(map[string]string, len(keys))
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var clients []model.ClientRecord
		if err := db.Model(model.ClientRecord{}).Where("LOWER(TRIM(email)) IN ?", batch).Find(&clients).Error; err != nil {
			return nil, err
		}
		for i := range clients {
			canonical[strings.ToLower(strings.TrimSpace(clients[i].Email))] = clients[i].Email
		}
	}

	out := make([]string, 0, len(keys))
	seen := make(map[string]struct{}, len(keys))
	for _, email := range mapped {
		trimmed := strings.TrimSpace(email)
		key := strings.ToLower(trimmed)
		if key == "" {
			continue
		}
		value := trimmed
		if canonicalEmail, ok := canonical[key]; ok {
			value = canonicalEmail
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, value)
	}
	return out, nil
}

// OwnsInbound reports whether the inbound is assigned to the reseller.
func (s *ResellerService) OwnsInbound(resellerId, inboundId int) (bool, error) {
	if resellerId <= 0 || inboundId <= 0 {
		return false, nil
	}
	db := database.GetDB()
	var count int64
	if err := db.Model(model.ResellerInbound{}).
		Where("reseller_id = ? AND inbound_id = ?", resellerId, inboundId).
		Count(&count).Error; err != nil {
		return false, err
	}
	return count > 0, nil
}

// OwnsClient reports whether the client (by email) has an explicit mapping
// to the reseller. Inbound ownership is deliberately not considered here.
func (s *ResellerService) OwnsClient(resellerId int, email string) (bool, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if resellerId <= 0 || email == "" {
		return false, nil
	}
	set, err := s.OwnedEmailSet(resellerId)
	if err != nil {
		return false, err
	}
	_, ok := set[email]
	return ok, nil
}

// EmailBySubID resolves a subscription id to its client email so reseller-
// scoped endpoints can verify ownership before serving subscription links.
func (s *ResellerService) EmailBySubID(subId string) (string, error) {
	subId = strings.TrimSpace(subId)
	if subId == "" {
		return "", nil
	}
	db := database.GetDB()
	var email string
	err := db.Model(model.ClientRecord{}).Where("sub_id = ?", subId).Limit(1).Pluck("email", &email).Error
	if err != nil {
		if database.IsNotFound(err) {
			return "", nil
		}
		return "", err
	}
	return email, nil
}

func normalizedPositiveIDs(values []int) ([]int, error) {
	out := make([]int, 0, len(values))
	seen := make(map[int]struct{}, len(values))
	for _, value := range values {
		if value <= 0 {
			return nil, errors.New("ids must be positive")
		}
		if _, duplicate := seen[value]; duplicate {
			continue
		}
		seen[value] = struct{}{}
		out = append(out, value)
	}
	if len(out) == 0 {
		return nil, errors.New("at least one id is required")
	}
	return out, nil
}

// AssignInbounds atomically assigns a set of inbounds to one reseller.
// Multiple resellers may share the same inbound while client visibility remains
// strictly isolated per reseller via ResellerClient. Repeating the same
// assignment is idempotent.
func (s *ResellerService) AssignInbounds(resellerId int, inboundIds []int) error {
	resellerScopeMutationMu.Lock()
	defer resellerScopeMutationMu.Unlock()
	ids, err := normalizedPositiveIDs(inboundIds)
	if err != nil {
		return err
	}
	db := database.GetDB()
	return db.Transaction(func(tx *gorm.DB) error {
		var reseller model.Reseller
		if err := tx.Where("id = ?", resellerId).First(&reseller).Error; err != nil {
			if database.IsNotFound(err) {
				return errors.New("reseller not found")
			}
			return err
		}
		var inbounds []model.Inbound
		if err := tx.Where("id IN ?", ids).Find(&inbounds).Error; err != nil {
			return err
		}
		found := make(map[int]struct{}, len(inbounds))
		for _, inbound := range inbounds {
			found[inbound.Id] = struct{}{}
		}
		for _, id := range ids {
			if _, ok := found[id]; !ok {
				return fmt.Errorf("inbound %d not found", id)
			}
		}
		now := time.Now().UnixMilli()
		for _, id := range ids {
			row := &model.ResellerInbound{ResellerId: resellerId, InboundId: id, CreatedAt: now}
			if err := tx.Where("reseller_id = ? AND inbound_id = ?", resellerId, id).
				FirstOrCreate(row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// SetInbounds replaces this reseller's complete inbound assignment set in one
// transaction. It is used by the multi-select form: an empty desired set is
// valid and clears only this reseller's mappings. Other resellers sharing any
// of these inbounds are never affected.
func (s *ResellerService) SetInbounds(resellerId int, inboundIds []int) error {
	resellerScopeMutationMu.Lock()
	defer resellerScopeMutationMu.Unlock()
	ids := make([]int, 0, len(inboundIds))
	seen := make(map[int]struct{}, len(inboundIds))
	for _, id := range inboundIds {
		if id <= 0 {
			return errors.New("ids must be positive")
		}
		if _, duplicate := seen[id]; duplicate {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	db := database.GetDB()
	return db.Transaction(func(tx *gorm.DB) error {
		var reseller model.Reseller
		if err := tx.Where("id = ?", resellerId).First(&reseller).Error; err != nil {
			if database.IsNotFound(err) {
				return errors.New("reseller not found")
			}
			return err
		}
		if len(ids) > 0 {
			var inbounds []model.Inbound
			if err := tx.Where("id IN ?", ids).Find(&inbounds).Error; err != nil {
				return err
			}
			found := make(map[int]struct{}, len(inbounds))
			for _, inbound := range inbounds {
				found[inbound.Id] = struct{}{}
			}
			for _, id := range ids {
				if _, ok := found[id]; !ok {
					return fmt.Errorf("inbound %d not found", id)
				}
			}
		}
		deleteQuery := tx.Where("reseller_id = ?", resellerId)
		if len(ids) > 0 {
			deleteQuery = deleteQuery.Where("inbound_id NOT IN ?", ids)
		}
		if err := deleteQuery.Delete(&model.ResellerInbound{}).Error; err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		for _, id := range ids {
			row := &model.ResellerInbound{ResellerId: resellerId, InboundId: id, CreatedAt: now}
			if err := tx.Where("reseller_id = ? AND inbound_id = ?", resellerId, id).
				FirstOrCreate(row).Error; err != nil {
				return err
			}
		}
		return nil
	})
}

// AssignInbound is the compatibility wrapper for the original single-item
// endpoint.
func (s *ResellerService) AssignInbound(resellerId, inboundId int) error {
	return s.AssignInbounds(resellerId, []int{inboundId})
}

// UnassignInbounds atomically removes only this reseller's inbound mappings.
// It never changes client records or inbound configuration.
func (s *ResellerService) UnassignInbounds(resellerId int, inboundIds []int) error {
	resellerScopeMutationMu.Lock()
	defer resellerScopeMutationMu.Unlock()
	ids, err := normalizedPositiveIDs(inboundIds)
	if err != nil {
		return err
	}
	db := database.GetDB()
	return db.Transaction(func(tx *gorm.DB) error {
		var reseller model.Reseller
		if err := tx.Where("id = ?", resellerId).First(&reseller).Error; err != nil {
			if database.IsNotFound(err) {
				return errors.New("reseller not found")
			}
			return err
		}
		return tx.Where("reseller_id = ? AND inbound_id IN ?", resellerId, ids).
			Delete(&model.ResellerInbound{}).Error
	})
}

// UnassignInbound is the compatibility wrapper for the original single-item
// endpoint.
func (s *ResellerService) UnassignInbound(resellerId, inboundId int) error {
	return s.UnassignInbounds(resellerId, []int{inboundId})
}

// ExplicitAssignedEmails lists only the clients the admin assigned to a
// reseller by hand (inherited ones are not included).
func (s *ResellerService) ExplicitAssignedEmails(resellerId int) ([]string, error) {
	return s.OwnedEmails(resellerId)
}

// HasForeignInboundAssociation reports whether a canonical client is attached
// to any inbound not owned by this reseller. This is used before a reseller
// attaches an existing client: the legacy email-level traffic/limit row cannot
// safely be shared across reseller and admin-owned associations.
func (s *ResellerService) HasForeignInboundAssociation(resellerId int, email string) (bool, error) {
	var count int64
	err := database.GetDB().Table("client_inbounds AS ci").
		Joins("JOIN clients AS c ON c.id = ci.client_id").
		Where("LOWER(TRIM(c.email)) = LOWER(?)", strings.TrimSpace(email)).
		Where(`NOT EXISTS (
			SELECT 1 FROM reseller_inbounds AS ri
			WHERE ri.reseller_id = ? AND ri.inbound_id = ci.inbound_id
		)`, resellerId).
		Count(&count).Error
	return count > 0, err
}

// AssignClients atomically creates explicit client mappings for a reseller.
// Inbound ownership is intentionally not consulted: this is the only operation
// that grants client credentials/stats visibility.
func (s *ResellerService) AssignClients(resellerId int, emails []string) error {
	return s.AssignClientsWithSales(resellerId, emails, nil)
}

// AssignClientsWithSales records immutable account/traffic sale snapshots for
// newly created reseller clients in the same transaction as their ownership
// mappings. Manually assigned pre-existing clients are passed only in emails.
func (s *ResellerService) AssignClientsWithSales(resellerId int, emails, saleEmails []string, declaredPrices ...map[string]*ResellerSalePrice) error {
	return s.assignClientsWithSales(resellerId, emails, saleEmails, "", declaredPrices...)
}

// CompleteResellerClientCreation removes the durable intent in the same
// transaction that creates the mapping and sale snapshot.
func (s *ResellerService) CompleteResellerClientCreation(resellerId int, email, intentID string, price *ResellerSalePrice) error {
	return s.assignClientsWithSales(resellerId, []string{email}, []string{email}, intentID, map[string]*ResellerSalePrice{email: price})
}

func (s *ResellerService) assignClientsWithSales(resellerId int, emails, saleEmails []string, intentID string, declaredPrices ...map[string]*ResellerSalePrice) error {
	prices := map[string]*ResellerSalePrice{}
	if len(declaredPrices) > 1 {
		return errors.New("multiple sale price maps are not allowed")
	}
	if len(declaredPrices) == 1 {
		for email, price := range declaredPrices[0] {
			if err := ValidateResellerSalePrice(price); err != nil {
				return err
			}
			prices[transferEmailKey(email)] = price
		}
	}
	resellerScopeMutationMu.Lock()
	defer resellerScopeMutationMu.Unlock()
	keys := make([]string, 0, len(emails))
	seen := make(map[string]struct{}, len(emails))
	for _, raw := range emails {
		key := transferEmailKey(raw)
		if key == "" {
			return errors.New("email can not be empty")
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return errors.New("at least one email is required")
	}
	keySet := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		keySet[key] = struct{}{}
	}
	saleKeys := make([]string, 0, len(saleEmails))
	saleSet := make(map[string]struct{}, len(saleEmails))
	for _, raw := range saleEmails {
		key := transferEmailKey(raw)
		if key == "" {
			return errors.New("sale email can not be empty")
		}
		if _, assigned := keySet[key]; !assigned {
			return fmt.Errorf("sale client %q is not in the assignment batch", key)
		}
		if _, duplicate := saleSet[key]; duplicate {
			continue
		}
		saleSet[key] = struct{}{}
		saleKeys = append(saleKeys, key)
	}
	for key := range prices {
		if _, ok := saleSet[key]; !ok {
			return fmt.Errorf("priced client %q is not a sale", key)
		}
	}
	db := database.GetDB()
	return db.Transaction(func(tx *gorm.DB) error {
		var reseller model.Reseller
		if err := tx.Where("id = ?", resellerId).First(&reseller).Error; err != nil {
			if database.IsNotFound(err) {
				return errors.New("reseller not found")
			}
			return err
		}
		var intent model.ResellerCreationIntent
		if intentID != "" {
			if len(keys) != 1 || len(saleKeys) != 1 {
				return errors.New("invalid creation intent batch")
			}
			if err := tx.Where("id = ? AND reseller_id = ?", intentID, resellerId).First(&intent).Error; err != nil {
				return err
			}
			if transferEmailKey(intent.Email) != keys[0] {
				return errors.New("creation intent email changed")
			}
		}
		var clients []model.ClientRecord
		if err := tx.Where("LOWER(TRIM(email)) IN ?", keys).Find(&clients).Error; err != nil {
			return err
		}
		canonical := make(map[string]string, len(clients))
		for _, client := range clients {
			if intentID != "" && (client.SubID != intent.SubID || client.TotalGB != intent.RequestedBytes) {
				return errors.New("creation intent identity or allocation changed")
			}
			canonical[transferEmailKey(client.Email)] = client.Email
		}
		for _, key := range keys {
			if _, ok := canonical[key]; !ok {
				return fmt.Errorf("client %q not found", key)
			}
		}
		var clientConflicts []model.ResellerClient
		if err := tx.Where("reseller_id <> ? AND LOWER(TRIM(email)) IN ?", resellerId, keys).
			Find(&clientConflicts).Error; err != nil {
			return err
		}
		if len(clientConflicts) > 0 {
			return fmt.Errorf("client %q is already assigned to another reseller", clientConflicts[0].Email)
		}
		for _, key := range keys {
			email := canonical[key]
			// Do not let the primary-key fields on row narrow the lookup to
			// the canonical spelling. Older databases may already have the
			// same mapping with case or whitespace differences; FirstOrCreate
			// would otherwise miss that row and create a duplicate mapping.
			var existing model.ResellerClient
			err := tx.Where("reseller_id = ? AND LOWER(TRIM(email)) = LOWER(?)", resellerId, email).
				First(&existing).Error
			if err == nil {
				continue
			}
			if !database.IsNotFound(err) {
				return err
			}
			row := &model.ResellerClient{
				ResellerId: resellerId,
				Email:      email,
				CreatedAt:  time.Now().UnixMilli(),
			}
			if err := tx.Create(row).Error; err != nil {
				return err
			}
		}
		if len(saleKeys) > 0 {
			for _, key := range saleKeys {
				var client model.ClientRecord
				if err := tx.Where("LOWER(TRIM(email)) = ?", key).First(&client).Error; err != nil {
					return err
				}
				// Client metadata can be supplied by the reseller. A live sale's
				// reporting date must instead be the server's recording time;
				// backdated entries use the audited historical-sale API.
				createdAt := time.Now().UnixMilli()
				sale := model.ResellerSale{
					ResellerId:     resellerId,
					ClientRecordId: client.Id,
					Email:          client.Email,
					TrafficBytes:   max(client.TotalGB, 0),
					CreatedAt:      createdAt,
				}
				if price := prices[key]; price != nil {
					amount := price.AmountMinor
					sale.AmountMinor = &amount
					sale.Currency = price.Currency
				}
				if err := tx.Clauses(clause.OnConflict{
					Columns:   []clause.Column{{Name: "reseller_id"}, {Name: "client_record_id"}},
					DoNothing: true,
				}).Create(&sale).Error; err != nil {
					return err
				}
			}
		}
		if intentID != "" {
			result := tx.Where("id = ? AND reseller_id = ?", intentID, resellerId).Delete(&model.ResellerCreationIntent{})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return errors.New("creation intent missing during completion")
			}
		}
		return nil
	})
}

// AssignClient is the compatibility wrapper for the original single-item
// endpoint.
func (s *ResellerService) AssignClient(resellerId int, email string) error {
	return s.AssignClients(resellerId, []string{email})
}

// UnassignClients atomically removes explicit mappings for the requested
// reseller. It does not delete the client or touch any inbound association.
func (s *ResellerService) UnassignClients(resellerId int, emails []string) error {
	resellerScopeMutationMu.Lock()
	defer resellerScopeMutationMu.Unlock()
	keys := make([]string, 0, len(emails))
	seen := make(map[string]struct{}, len(emails))
	for _, raw := range emails {
		key := transferEmailKey(raw)
		if key == "" {
			return errors.New("email can not be empty")
		}
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return errors.New("at least one email is required")
	}
	return submitTrafficWrite(func() error {
		return database.GetDB().Transaction(func(tx *gorm.DB) error {
			var reseller model.Reseller
			if err := tx.Where("id = ?", resellerId).First(&reseller).Error; err != nil {
				if database.IsNotFound(err) {
					return errors.New("reseller not found")
				}
				return err
			}
			for _, batch := range chunkStrings(keys, sqlInChunk) {
				var mappings []model.ResellerClient
				if err := tx.Where("reseller_id = ? AND LOWER(TRIM(email)) IN ?", resellerId, batch).Find(&mappings).Error; err != nil {
					return err
				}
				for _, mapping := range mappings {
					var clients []model.ClientRecord
					if err := tx.Where("LOWER(TRIM(email)) = ?", transferEmailKey(mapping.Email)).Find(&clients).Error; err != nil {
						return err
					}
					if len(clients) != 1 {
						return errors.New("cannot unassign client without a unique client record for usage accounting")
					}
					if err := archiveResellerClientUsageTx(tx, &clients[0], resellerId); err != nil {
						return err
					}
				}
				if err := tx.Where("reseller_id = ? AND LOWER(TRIM(email)) IN ?", resellerId, batch).Delete(&model.ResellerClient{}).Error; err != nil {
					return err
				}
			}
			return nil
		})
	})
}

// UnassignClient is the compatibility wrapper for the original single-item
// endpoint.
func (s *ResellerService) UnassignClient(resellerId int, email string) error {
	return s.UnassignClients(resellerId, []string{email})
}

// ---------------------------------------------------------------------------
// Usage & billing
// ---------------------------------------------------------------------------

// ClientUsage returns per-email traffic totals (up+down) for the given emails.
func (s *ResellerService) ClientUsage(emails []string) (map[string]*xray.ClientTraffic, error) {
	usage := make(map[string]*xray.ClientTraffic, len(emails))
	if len(emails) == 0 {
		return usage, nil
	}
	keys := make([]string, 0, len(emails))
	seen := make(map[string]struct{}, len(emails))
	for _, email := range emails {
		key := strings.ToLower(strings.TrimSpace(email))
		if key == "" {
			continue
		}
		if _, ok := seen[key]; !ok {
			seen[key] = struct{}{}
			keys = append(keys, key)
		}
	}
	db := database.GetDB()
	rows := make([]*xray.ClientTraffic, 0)
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var part []*xray.ClientTraffic
		if err := db.Model(xray.ClientTraffic{}).Where("LOWER(TRIM(email)) IN ?", batch).Find(&part).Error; err != nil {
			return nil, err
		}
		rows = append(rows, part...)
	}
	// Quota decisions must not silently fall back to local counters when a
	// global overlay query fails. Include global-only rows as well: a partially
	// missing local traffic row must not make measured usage disappear.
	globalUp, globalDown := map[string]int64{}, map[string]int64{}
	for _, batch := range chunkStrings(keys, sqlInChunk) {
		var globals []model.ClientGlobalTraffic
		if err := db.Where("LOWER(TRIM(email)) IN ?", batch).Find(&globals).Error; err != nil {
			return nil, fmt.Errorf("read reseller global traffic: %w", err)
		}
		for _, g := range globals {
			key := transferEmailKey(g.Email)
			globalUp[key] = max(globalUp[key], g.Up)
			globalDown[key] = max(globalDown[key], g.Down)
		}
	}
	// Exact-email uniqueness does not prevent case/whitespace variants. Their
	// ownership and counter overlap are ambiguous; applying the same global
	// total to each row would double-charge, while merging could undercount.
	localSeen := make(map[string]bool, len(rows))
	for _, row := range rows {
		key := transferEmailKey(row.Email)
		if localSeen[key] {
			return nil, errors.New("ambiguous client traffic counters require reconciliation")
		}
		localSeen[key] = true
		row.Up = max(row.Up, globalUp[key])
		row.Down = max(row.Down, globalDown[key])
	}
	for _, row := range rows {
		key := strings.ToLower(strings.TrimSpace(row.Email))
		agg, ok := usage[key]
		if !ok {
			agg = &xray.ClientTraffic{Email: row.Email}
			usage[key] = agg
		}
		agg.Up = saturatingAddBytes(agg.Up, row.Up)
		agg.Down = saturatingAddBytes(agg.Down, row.Down)
		if row.Enable {
			agg.Enable = true
		}
	}
	for _, key := range keys {
		if _, exists := usage[key]; !exists && (globalUp[key] > 0 || globalDown[key] > 0) {
			usage[key] = &xray.ClientTraffic{Email: key, Up: globalUp[key], Down: globalDown[key]}
		}
	}
	return usage, nil
}

// Stat builds the live usage snapshot of one reseller.
func (s *ResellerService) Stat(reseller *model.Reseller) (*ResellerStat, error) {
	if reseller == nil {
		return nil, errors.New("reseller is required")
	}
	db := database.GetDB()
	stat := &ResellerStat{Reseller: reseller, Currency: "USD"}
	if err := db.Model(&model.ResellerCreationIntent{}).Where("reseller_id = ?", reseller.Id).Count(&stat.PendingCreations).Error; err != nil {
		return nil, err
	}
	var err error
	stat.ProcessingRenewals, err = countResellerProcessingClientRenewals(db, reseller.Id)
	if err != nil {
		return nil, err
	}
	var settingSvc SettingService
	if configured, err := settingSvc.GetCheckoutCurrency(); err == nil && strings.TrimSpace(configured) != "" {
		if code, ok := moneycurrency.Normalize(configured); ok {
			stat.Currency = code
		} else {
			stat.Currency = strings.ToUpper(strings.TrimSpace(configured))
		}
	}

	ids, err := s.OwnedInboundIds(reseller.Id)
	if err != nil {
		return nil, err
	}
	stat.InboundCount = len(ids)

	emails, err := s.OwnedEmails(reseller.Id)
	if err != nil {
		return nil, err
	}
	stat.ClientCount = len(emails)
	// ResellerClient is the explicit visibility boundary. Do not require the
	// client to be isolated from every admin association before showing its
	// aggregate usage; a client explicitly attached to this reseller is
	// authorized, and scoped payloads redact the stale inbound identifier.
	visibleEmails, err := s.OwnedEmailSet(reseller.Id)
	if err != nil {
		return nil, err
	}
	usageEmails := make([]string, 0, len(emails))
	for _, email := range emails {
		if _, ok := visibleEmails[transferEmailKey(email)]; ok {
			usageEmails = append(usageEmails, email)
		}
	}

	activeClients := make(map[int]model.ClientRecord)
	clientKeys := make([]string, 0, len(emails))
	for _, email := range emails {
		clientKeys = append(clientKeys, transferEmailKey(email))
	}
	seenClients := make(map[string]bool, len(clientKeys))
	if len(clientKeys) > 0 {
		for _, batch := range chunkStrings(clientKeys, sqlInChunk) {
			var records []model.ClientRecord
			if err := db.Select("id", "email", "total_gb").Where("LOWER(TRIM(email)) IN ?", batch).Find(&records).Error; err != nil {
				return nil, err
			}
			for _, record := range records {
				key := transferEmailKey(record.Email)
				if seenClients[key] {
					return nil, errors.New("ambiguous reseller client requires reconciliation")
				}
				seenClients[key] = true
				activeClients[record.Id] = record
				// A zero client limit is unlimited, not a free allocation. Legacy
				// or administrator-assigned unlimited clients must exhaust a
				// finite reseller's available allocation until reconciled.
				if record.TotalGB <= 0 {
					stat.HasUnlimitedClient = true
				}
				stat.AllocatedTraffic = saturatingAddBytes(stat.AllocatedTraffic, record.TotalGB)
			}
		}
	}

	// A dangling ownership mapping can hide an allocation or usage. Do not
	// treat it as free capacity until an administrator reconciles the record.
	for _, key := range clientKeys {
		if !seenClients[key] {
			return nil, errors.New("reseller client mapping has no client record; reconciliation required")
		}
	}

	usage, err := s.ClientUsage(usageEmails)
	if err != nil {
		return nil, err
	}
	for _, row := range usage {
		stat.UsedTraffic = saturatingAddBytes(stat.UsedTraffic, row.Up)
		stat.UsedTraffic = saturatingAddBytes(stat.UsedTraffic, row.Down)
	}
	// Reducing a live client's quota must never release traffic already used.
	// Account for the excess per client rather than comparing only aggregate
	// totals, since unused allocation on another client cannot offset it.
	for _, client := range activeClients {
		liveUsed := int64(0)
		if row := usage[transferEmailKey(client.Email)]; row != nil {
			liveUsed = saturatingAddBytes(row.Up, row.Down)
		}
		if liveUsed > max(client.TotalGB, 0) {
			stat.AllocatedTraffic = saturatingAddBytes(stat.AllocatedTraffic, liveUsed-max(client.TotalGB, 0))
		}
	}
	var archived []model.ResellerTrafficArchive
	if err := db.Where("reseller_id = ?", reseller.Id).Find(&archived).Error; err != nil {
		return nil, err
	}
	for _, entry := range archived {
		// Completed usage windows are additive to the current window.
		stat.UsedTraffic = saturatingAddBytes(stat.UsedTraffic, entry.PriorConsumedBytes)
		stat.AllocatedTraffic = saturatingAddBytes(stat.AllocatedTraffic, entry.PriorConsumedBytes)
		if client, exists := activeClients[entry.ClientRecordId]; exists {
			// A failed or retried deletion can leave the same client live.
			// Count the greater snapshot, not both copies.
			liveUsed := int64(0)
			if row := usage[transferEmailKey(client.Email)]; row != nil {
				liveUsed = saturatingAddBytes(row.Up, row.Down)
			}
			if entry.ConsumedBytes > liveUsed {
				stat.UsedTraffic = saturatingAddBytes(stat.UsedTraffic, entry.ConsumedBytes-liveUsed)
			}
			liveAllocation := max(max(client.TotalGB, 0), liveUsed)
			if entry.ConsumedBytes > liveAllocation {
				stat.AllocatedTraffic = saturatingAddBytes(stat.AllocatedTraffic, entry.ConsumedBytes-liveAllocation)
			}
		} else {
			stat.UsedTraffic = saturatingAddBytes(stat.UsedTraffic, entry.ConsumedBytes)
			stat.AllocatedTraffic = saturatingAddBytes(stat.AllocatedTraffic, entry.ConsumedBytes)
		}
	}

	online := s.inboundService.GetOnlineClients()
	onlineSet := make(map[string]struct{}, len(online))
	for _, email := range online {
		onlineSet[transferEmailKey(email)] = struct{}{}
	}
	for email := range visibleEmails {
		if _, ok := onlineSet[email]; ok {
			stat.OnlineCount++
		}
	}

	// GB-based billing: cost accrues from measured usage at the reseller's
	// per-GB price; the balance is the prepaid deposit minus that cost
	// (negative = debt, shown red in the UI).
	stat.Cost = RoundMoney(float64(stat.UsedTraffic)/bytesPerGB*reseller.PricePerGB, stat.Currency)
	stat.Balance = RoundMoney(reseller.Deposit-stat.Cost, stat.Currency)
	stat.Expired = IsExpired(reseller.ExpiryTime)
	if reseller.TrafficLimit > 0 {
		stat.RemainingTraffic = reseller.TrafficLimit - stat.AllocatedTraffic
		if stat.HasUnlimitedClient {
			stat.RemainingTraffic = 0
		}
	}
	stat.OverQuota = stat.Expired || !reseller.Enable ||
		(reseller.TrafficLimit > 0 && (stat.HasUnlimitedClient || stat.AllocatedTraffic > reseller.TrafficLimit || stat.UsedTraffic >= reseller.TrafficLimit)) ||
		(reseller.ClientLimit > 0 && stat.ClientCount > reseller.ClientLimit)
	stat.Disabled = !reseller.Enable
	return stat, nil
}

// Report returns the detailed usage/billing report of one reseller.
func (s *ResellerService) Report(resellerId int) (*ResellerReport, error) {
	reseller, err := s.Get(resellerId)
	if err != nil {
		return nil, err
	}
	stat, err := s.Stat(reseller)
	if err != nil {
		return nil, err
	}
	emails, err := s.OwnedEmails(resellerId)
	if err != nil {
		return nil, err
	}
	ownedInboundIDs, err := s.OwnedInboundIdSet(resellerId)
	if err != nil {
		return nil, err
	}
	// Explicit ResellerClient mappings authorize the report's aggregate usage;
	// inbound ownership is used only to redact attachment ids below.
	visibleEmails, err := s.OwnedEmailSet(resellerId)
	if err != nil {
		return nil, err
	}
	usageEmails := make([]string, 0, len(emails))
	for _, email := range emails {
		if _, ok := visibleEmails[transferEmailKey(email)]; ok {
			usageEmails = append(usageEmails, email)
		}
	}
	usage, err := s.ClientUsage(usageEmails)
	if err != nil {
		return nil, err
	}

	db := database.GetDB()
	clientKeys := make([]string, 0, len(emails))
	for _, email := range emails {
		clientKeys = append(clientKeys, transferEmailKey(email))
	}
	records := make([]*model.ClientRecord, 0, len(emails))
	for _, batch := range chunkStrings(clientKeys, sqlInChunk) {
		var part []*model.ClientRecord
		if err := db.Model(model.ClientRecord{}).Where("LOWER(TRIM(email)) IN ?", batch).Find(&part).Error; err != nil {
			return nil, err
		}
		records = append(records, part...)
	}

	inboundIdsByEmail := map[string][]int{}
	if len(emails) > 0 {
		type row struct {
			Email     string
			InboundId int
		}
		rows := make([]row, 0)
		for _, batch := range chunkStrings(clientKeys, sqlInChunk) {
			var part []row
			if err := db.Raw(`
				SELECT c.email AS email, ci.inbound_id AS inbound_id
				FROM clients c JOIN client_inbounds ci ON ci.client_id = c.id
				WHERE LOWER(TRIM(c.email)) IN ?
			`, batch).Scan(&part).Error; err != nil {
				return nil, err
			}
			rows = append(rows, part...)
		}
		for _, r := range rows {
			if _, allowed := ownedInboundIDs[r.InboundId]; !allowed {
				continue
			}
			key := strings.ToLower(strings.TrimSpace(r.Email))
			inboundIdsByEmail[key] = append(inboundIdsByEmail[key], r.InboundId)
		}
	}

	rows := make([]ResellerClientRow, 0, len(records))
	for _, rec := range records {
		up, down, used := int64(0), int64(0), int64(0)
		if t, ok := usage[strings.ToLower(strings.TrimSpace(rec.Email))]; ok && t != nil {
			up, down = t.Up, t.Down
			used = saturatingAddBytes(up, down)
		}
		ids := inboundIdsByEmail[strings.ToLower(strings.TrimSpace(rec.Email))]
		sort.Ints(ids)
		rows = append(rows, ResellerClientRow{
			Email:      rec.Email,
			Comment:    rec.Comment,
			Group:      rec.Group,
			Enable:     rec.Enable,
			TotalBytes: rec.TotalGB,
			ExpiryTime: rec.ExpiryTime,
			Up:         up,
			Down:       down,
			Used:       used,
			Cost:       0,
			InboundIds: ids,
			SubID:      rec.SubID,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Email < rows[j].Email })

	transactions, err := s.Transactions(resellerId)
	if err != nil {
		return nil, err
	}
	var archives []model.ResellerTrafficArchive
	if err := db.Where("reseller_id = ?", resellerId).Order("id DESC").Limit(500).Find(&archives).Error; err != nil {
		return nil, err
	}
	return &ResellerReport{Stat: stat, Clients: rows, Transactions: transactions, TrafficArchives: archives}, nil
}

// Transactions lists a reseller's ledger, newest first.
func (s *ResellerService) Transactions(resellerId int) ([]*model.ResellerTransaction, error) {
	db := database.GetDB()
	rows := make([]*model.ResellerTransaction, 0)
	err := db.Model(model.ResellerTransaction{}).
		Where("reseller_id = ?", resellerId).
		Order("id DESC").
		Limit(500).
		Find(&rows).Error
	if err != nil {
		return nil, err
	}
	return rows, nil
}

// SalesReport groups immutable client-creation sale snapshots in UTC. The time
// range is half-open [from, to); zero bounds mean unbounded.
func (s *ResellerService) SalesReport(resellerId int, period string, from, to int64) (*ResellerSalesReport, error) {
	period = strings.ToLower(strings.TrimSpace(period))
	if period != "daily" && period != "monthly" && period != "yearly" {
		return nil, errors.New("period must be daily, monthly, or yearly")
	}
	if resellerId <= 0 {
		return nil, errors.New("reseller id must be positive")
	}
	if from < 0 || to < 0 || (from > 0 && to > 0 && from >= to) {
		return nil, errors.New("invalid sales report time range")
	}
	if _, err := s.Get(resellerId); err != nil {
		return nil, err
	}
	query := database.GetDB().Model(&model.ResellerSale{}).Where("reseller_id = ?", resellerId)
	if from > 0 {
		query = query.Where("created_at >= ?", from)
	}
	if to > 0 {
		query = query.Where("created_at < ?", to)
	}
	var sales []model.ResellerSale
	if err := query.Order("created_at ASC, id ASC").Find(&sales).Error; err != nil {
		return nil, err
	}
	historyQuery := database.GetDB().Where("reseller_id = ?", resellerId)
	if from > 0 {
		historyQuery = historyQuery.Where("sold_at >= ?", from)
	}
	if to > 0 {
		historyQuery = historyQuery.Where("sold_at < ?", to)
	}
	var historical []model.ResellerHistoricalSale
	if err := historyQuery.Find(&historical).Error; err != nil {
		return nil, err
	}
	type saleEvent struct {
		model.ResellerSale
		historical bool
	}
	events := make([]saleEvent, 0, len(sales)+len(historical))
	for _, sale := range sales {
		events = append(events, saleEvent{ResellerSale: sale})
	}
	for _, sale := range historical {
		events = append(events, saleEvent{ResellerSale: model.ResellerSale{CreatedAt: sale.SoldAt, TrafficBytes: sale.TrafficBytes, AmountMinor: sale.AmountMinor, Currency: sale.Currency}, historical: true})
	}
	buckets := make(map[string]*ResellerSalesBucket)
	report := &ResellerSalesReport{ResellerId: resellerId, Period: period, From: from, To: to, AmountsMinor: map[string]string{}}
	for _, sale := range events {
		if sale.CreatedAt <= 0 {
			continue
		}
		at := time.UnixMilli(sale.CreatedAt).UTC()
		var key string
		var start time.Time
		switch period {
		case "daily":
			key = at.Format("2006-01-02")
			start = time.Date(at.Year(), at.Month(), at.Day(), 0, 0, 0, 0, time.UTC)
		case "monthly":
			key = at.Format("2006-01")
			start = time.Date(at.Year(), at.Month(), 1, 0, 0, 0, 0, time.UTC)
		case "yearly":
			key = at.Format("2006")
			start = time.Date(at.Year(), time.January, 1, 0, 0, 0, 0, time.UTC)
		}
		bucket := buckets[key]
		if bucket == nil {
			bucket = &ResellerSalesBucket{Period: key, StartAt: start.UnixMilli(), AmountsMinor: map[string]string{}}
			buckets[key] = bucket
		}
		bucket.Sales++
		if sale.historical {
			bucket.HistoricalSales++
			report.HistoricalSales++
		}
		if sale.AmountMinor == nil || sale.Currency == "" {
			bucket.UnknownPrices++
			report.UnknownPrices++
		} else {
			bucket.AmountsMinor[sale.Currency] = addMinorAmount(bucket.AmountsMinor[sale.Currency], *sale.AmountMinor)
			report.AmountsMinor[sale.Currency] = addMinorAmount(report.AmountsMinor[sale.Currency], *sale.AmountMinor)
		}
		bucket.TrafficBytes = saturatingAddBytes(bucket.TrafficBytes, sale.TrafficBytes)
		report.TotalSales++
		report.TotalTrafficBytes = saturatingAddBytes(report.TotalTrafficBytes, sale.TrafficBytes)
	}
	keys := make([]string, 0, len(buckets))
	for key := range buckets {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	report.Buckets = make([]ResellerSalesBucket, 0, len(keys))
	for _, key := range keys {
		report.Buckets = append(report.Buckets, *buckets[key])
	}
	return report, nil
}

// AdjustBalance moves money on a reseller's account and records the movement.
// A negative amount withdraws (settlement), a positive amount tops the account
// up.
func (s *ResellerService) AdjustBalance(resellerId int, amount float64, comment string) (*model.Reseller, error) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || amount == 0 {
		return nil, errors.New("amount must be a non-zero number")
	}
	var settingSvc SettingService
	configuredCurrency, _ := settingSvc.GetCheckoutCurrency()
	currencyCode, ok := moneycurrency.Normalize(configuredCurrency)
	if !ok {
		currencyCode = "USD"
	}
	amount = RoundMoney(amount, currencyCode)
	if amount == 0 {
		return nil, errors.New("amount is below the minimum currency unit")
	}
	reseller, err := s.Get(resellerId)
	if err != nil {
		return nil, err
	}
	if reseller.Deposit+amount < 0 {
		return nil, fmt.Errorf("balance can not go below zero (current: %s)", FormatNumber(reseller.Deposit))
	}
	newBalance := RoundMoney(reseller.Deposit+amount, currencyCode)
	if math.IsNaN(newBalance) || math.IsInf(newBalance, 0) {
		return nil, errors.New("balance exceeds supported range")
	}
	txType := "deposit"
	if amount < 0 {
		txType = "withdraw"
	}
	db := database.GetDB()
	err = db.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(model.Reseller{}).Where("id = ? AND deposit = ?", resellerId, reseller.Deposit).Updates(map[string]any{
			"deposit":    newBalance,
			"updated_at": time.Now().UnixMilli(),
		})
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("reseller balance changed concurrently; retry adjustment")
		}
		return tx.Create(&model.ResellerTransaction{
			ResellerId: resellerId,
			Type:       txType,
			Amount:     amount,
			Balance:    newBalance,
			Comment:    strings.TrimSpace(comment),
			CreatedAt:  time.Now().UnixMilli(),
		}).Error
	})
	if err != nil {
		return nil, err
	}
	return s.Get(resellerId)
}

// ---------------------------------------------------------------------------
// Quota checks (used by the reseller-scoped write paths)
// ---------------------------------------------------------------------------

// EnsureActive rejects writes from disabled/expired resellers.
func (s *ResellerService) EnsureActive(reseller *model.Reseller) error {
	if reseller == nil {
		return errors.New("reseller is required")
	}
	if !reseller.Enable {
		return errors.New("your reseller account is disabled, please contact the administrator")
	}
	if IsExpired(reseller.ExpiryTime) {
		return errors.New("your reseller account has expired, please contact the administrator")
	}
	return nil
}

// CheckTrafficQuota verifies that allocating addBytes more client quota keeps
// the reseller inside its purchased volume (and that the volume is not already
// used up).
func (s *ResellerService) CheckTrafficQuota(reseller *model.Reseller, addBytes int64) error {
	if reseller == nil {
		return errors.New("reseller is required")
	}
	if err := CheckPendingResellerCreations(reseller.Id); err != nil {
		return err
	}
	if err := CheckResellerProcessingClientRenewals(reseller.Id); err != nil {
		return err
	}
	if addBytes < 0 {
		return errors.New("traffic allocation cannot be negative")
	}
	if reseller.TrafficLimit <= 0 {
		return nil
	}
	stat, err := s.Stat(reseller)
	if err != nil {
		return err
	}
	return checkTrafficQuotaSnapshot(reseller, stat, addBytes)
}

func checkTrafficQuotaSnapshot(reseller *model.Reseller, stat *ResellerStat, addBytes int64) error {
	if reseller == nil || stat == nil {
		return errors.New("reseller quota snapshot is required")
	}
	if addBytes < 0 {
		return errors.New("traffic allocation cannot be negative")
	}
	if stat.HasUnlimitedClient {
		return errors.New("limited reseller has an unlimited client; administrator reconciliation required")
	}
	if stat.UsedTraffic >= reseller.TrafficLimit {
		return fmt.Errorf("traffic quota exhausted: %s of %s used", FormatBytes(stat.UsedTraffic), FormatBytes(reseller.TrafficLimit))
	}
	remaining := int64(0)
	if stat.AllocatedTraffic < reseller.TrafficLimit {
		remaining = reseller.TrafficLimit - stat.AllocatedTraffic
	}
	if addBytes > 0 && addBytes > remaining {
		return fmt.Errorf("traffic quota exceeded: only %s left out of %s (you are allocating %s)",
			FormatBytes(remaining), FormatBytes(reseller.TrafficLimit), FormatBytes(addBytes))
	}
	return nil
}

// ValidateResellerAllocation rejects an unlimited client under a finite reseller
// traffic limit. A zero client quota means unlimited, not zero bytes allocated.
func ValidateResellerAllocation(reseller *model.Reseller, allocation int64) error {
	if reseller == nil {
		return errors.New("reseller is required")
	}
	if allocation < 0 {
		return errors.New("client traffic quota cannot be negative")
	}
	if reseller.TrafficLimit > 0 && allocation == 0 {
		return errors.New("unlimited client traffic is not allowed for a limited reseller")
	}
	return nil
}

// CheckClientQuota verifies that adding `adding` clients is allowed and that
// their quota fits into the traffic volume.
func (s *ResellerService) CheckClientQuota(reseller *model.Reseller, adding int, addBytes int64) error {
	if reseller == nil {
		return errors.New("reseller is required")
	}
	if err := CheckPendingResellerCreations(reseller.Id); err != nil {
		return err
	}
	if err := CheckResellerProcessingClientRenewals(reseller.Id); err != nil {
		return err
	}
	if adding < 0 {
		return errors.New("client count increment cannot be negative")
	}
	if addBytes < 0 {
		return errors.New("traffic allocation cannot be negative")
	}
	if reseller.ClientLimit <= 0 && reseller.TrafficLimit <= 0 {
		return nil
	}
	stat, err := s.Stat(reseller)
	if err != nil {
		return err
	}
	if adding > 0 && reseller.ClientLimit > 0 {
		remaining := reseller.ClientLimit - stat.ClientCount
		if remaining < 0 {
			remaining = 0
		}
		if adding > remaining {
			return fmt.Errorf("client limit reached: %d of %d used", stat.ClientCount, reseller.ClientLimit)
		}
	}
	if reseller.TrafficLimit <= 0 {
		return nil
	}
	return checkTrafficQuotaSnapshot(reseller, stat, addBytes)
}

// ---------------------------------------------------------------------------
// helpers
// ---------------------------------------------------------------------------

// Round2 remains for legacy callers that explicitly use a two-decimal amount.
func Round2(v float64) float64 {
	return math.Round(v*100) / 100
}

// RoundMoney rounds to the configured currency's ISO minor-unit precision.
func RoundMoney(v float64, code string) float64 {
	factor := math.Pow10(moneycurrency.FractionDigits(code))
	return math.Round(v*factor) / factor
}

// saturatingAddBytes safely sums persisted byte counters. Negative legacy or
// corrupt values are ignored rather than reducing a quota usage total.
func saturatingAddBytes(total, value int64) int64 {
	if total < 0 {
		total = 0
	}
	if value <= 0 {
		return total
	}
	if total > math.MaxInt64-value {
		return math.MaxInt64
	}
	return total + value
}

// FormatBytes renders a byte count using binary units.
func FormatBytes(v int64) string {
	const unit = 1024
	if v < unit {
		return fmt.Sprintf("%d B", v)
	}
	units := []string{"KiB", "MiB", "GiB", "TiB", "PiB"}
	value := float64(v)
	idx := -1
	for value >= unit && idx < len(units)-1 {
		value /= unit
		idx++
	}
	return fmt.Sprintf("%.2f %s", value, units[idx])
}

// FormatNumber renders a money amount with two decimals.
func FormatNumber(v float64) string {
	return fmt.Sprintf("%.2f", v)
}
