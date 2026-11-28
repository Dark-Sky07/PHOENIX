package model

// DNSCertificateProfile stores the non-secret configuration needed to renew a
// DNS-01 certificate. ProviderCredentialsEncrypted is AES-GCM encrypted with
// the panel secret; it is never returned through the API.
type DNSCertificateProfile struct {
	ID                           int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name                         string `json:"name" gorm:"size:64;uniqueIndex;not null"`
	Domains                      string `json:"domains" gorm:"type:text;not null"` // JSON string array, DNS names may include *.
	Provider                     string `json:"provider" gorm:"size:32;not null"`  // manual or cloudflare
	ProviderCredentialsEncrypted string `json:"-" gorm:"type:text"`
	Email                        string `json:"email" gorm:"size:254"`
	ApplyToPanel                 bool   `json:"applyToPanel" gorm:"not null;default:false"`
	Enabled                      bool   `json:"enabled" gorm:"not null;default:true;index"`
	LastIssuedAt                 int64  `json:"lastIssuedAt"`
	LastError                    string `json:"lastError" gorm:"size:512"`
	// PendingOrder and PendingChallenges exist only for the standards-based
	// manual DNS workflow; no provider credential is stored there.
	PendingOrder      string `json:"-" gorm:"size:2048"`
	PendingChallenges string `json:"-" gorm:"type:text"`
	PendingAt         int64  `json:"pendingAt"`
	CreatedAt         int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt         int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (DNSCertificateProfile) TableName() string { return "dns_certificate_profiles" }

// RuleSetCatalog is a cached, validated remote routing list. Entries are JSON
// rather than an executable config, so a compromised remote source cannot
// inject arbitrary Xray configuration into the panel.
type RuleSetCatalog struct {
	ID             int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name           string `json:"name" gorm:"size:64;uniqueIndex;not null"`
	URL            string `json:"url" gorm:"size:2048;not null"`
	Kind           string `json:"kind" gorm:"size:16;not null"` // domain or ip
	Entries        string `json:"-" gorm:"type:text;not null"`  // normalized JSON string array
	Enabled        bool   `json:"enabled" gorm:"not null;default:true;index"`
	RefreshMinutes int    `json:"refreshMinutes" gorm:"not null;default:360"`
	ETag           string `json:"-" gorm:"size:512"`
	LastRefreshAt  int64  `json:"lastRefreshAt"`
	LastSuccessAt  int64  `json:"lastSuccessAt"`
	LastError      string `json:"lastError" gorm:"size:512"`
	SHA256         string `json:"sha256" gorm:"size:64"`
	CreatedAt      int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt      int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (RuleSetCatalog) TableName() string { return "rule_set_catalog" }

// DashboardPreference is deliberately per-user. Missing rows retain the
// established dashboard, making this an additive upgrade for every operator.
type DashboardPreference struct {
	ID             int    `json:"id" gorm:"primaryKey;autoIncrement"`
	UserID         int    `json:"userId" gorm:"uniqueIndex;not null"`
	Layout         string `json:"layout" gorm:"type:text;not null"` // ordered widget IDs JSON
	HiddenWidgets  string `json:"hiddenWidgets" gorm:"type:text;not null"`
	RefreshSeconds int    `json:"refreshSeconds" gorm:"not null;default:2"`
	UpdatedAt      int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (DashboardPreference) TableName() string { return "dashboard_preferences" }

// OIDCProviderConfig is a single, optional generic OpenID Connect provider.
// It relies on discovery, so it works with standards-compliant providers such
// as Keycloak, Authentik, Google Workspace, Azure AD and GitLab. The client
// secret never leaves the database in plaintext.
type OIDCProviderConfig struct {
	ID                    int    `json:"id" gorm:"primaryKey"`
	Enabled               bool   `json:"enabled" gorm:"not null;default:false"`
	Issuer                string `json:"issuer" gorm:"size:2048"`
	ClientID              string `json:"clientId" gorm:"size:512"`
	ClientSecretEncrypted string `json:"-" gorm:"type:text"`
	AllowedEmailDomains   string `json:"allowedEmailDomains" gorm:"type:text"` // JSON string array; empty permits all
	AutoProvision         bool   `json:"autoProvision" gorm:"not null;default:false"`
	DefaultRole           string `json:"defaultRole" gorm:"size:32;not null;default:viewer"`
	GroupClaim            string `json:"groupClaim" gorm:"size:64"`
	GroupRoleMappings     string `json:"groupRoleMappings" gorm:"type:text"` // JSON map group => role
	CreatedAt             int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt             int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (OIDCProviderConfig) TableName() string { return "oidc_provider_config" }
