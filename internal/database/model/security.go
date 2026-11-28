package model

// LoginProtection is the durable part of the login-throttling state. KeyHash
// is SHA-256(ip + NUL + normalized username); retaining only the digest avoids
// making the security table another source of account-name disclosure.
type LoginProtection struct {
	ID              int    `json:"id" gorm:"primaryKey;autoIncrement"`
	KeyHash         string `json:"-" gorm:"size:64;uniqueIndex;not null"`
	FailureCount    int    `json:"failureCount" gorm:"not null;default:0"`
	WindowStartedAt int64  `json:"windowStartedAt" gorm:"not null;default:0"`
	BlockedUntil    int64  `json:"blockedUntil" gorm:"not null;default:0;index"`
	UpdatedAt       int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (LoginProtection) TableName() string { return "login_protections" }

// UserSession is an opt-in registry for sessions created after the security
// upgrade. Old signed cookies intentionally remain valid until their normal
// expiry: a missing SessionHash is treated as a legacy session for upgrade
// compatibility. New sessions are individually revocable.
type UserSession struct {
	ID          int    `json:"id" gorm:"primaryKey;autoIncrement"`
	UserID      int    `json:"userId" gorm:"index;not null"`
	SessionHash string `json:"-" gorm:"size:64;uniqueIndex;not null"`
	LoginEpoch  int64  `json:"loginEpoch" gorm:"not null;default:0"`
	CreatedAt   int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	LastSeenAt  int64  `json:"lastSeenAt" gorm:"index;not null"`
	ExpiresAt   int64  `json:"expiresAt" gorm:"index;not null"`
	IP          string `json:"ip" gorm:"size:64"`
	UserAgent   string `json:"userAgent" gorm:"size:256"`
}

func (UserSession) TableName() string { return "user_sessions" }

// RecoveryCode stores only a digest of a high-entropy, single-use recovery
// code. It is scoped to an administrator even though legacy panel TOTP is a
// global setting, so using a recovery code cannot consume another admin's.
type RecoveryCode struct {
	ID        int    `json:"id" gorm:"primaryKey;autoIncrement"`
	UserID    int    `json:"userId" gorm:"index:idx_recovery_user_used,priority:1;not null"`
	CodeHash  string `json:"-" gorm:"size:64;uniqueIndex;not null"`
	UsedAt    int64  `json:"usedAt" gorm:"index:idx_recovery_user_used,priority:2;not null;default:0"`
	CreatedAt int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
}

func (RecoveryCode) TableName() string { return "recovery_codes" }

// WebhookSubscription contains an enabled, explicitly opted-in endpoint. The
// signing secret is AES-GCM encrypted with the panel secret; plaintext is only
// returned once on creation and is never recorded in delivery logs.
type WebhookSubscription struct {
	ID              int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name            string `json:"name" gorm:"size:64;uniqueIndex;not null"`
	URL             string `json:"url" gorm:"size:2048;not null"`
	Events          string `json:"events" gorm:"type:text;not null"`
	SecretEncrypted string `json:"-" gorm:"type:text;not null"`
	Enabled         bool   `json:"enabled" gorm:"not null;default:true;index"`
	CreatedAt       int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt       int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (WebhookSubscription) TableName() string { return "webhook_subscriptions" }

// WebhookDelivery is a durable outbox row. Payload contains safe event metadata
// only, never bearer tokens, passwords, setup/recovery codes, or secrets.
type WebhookDelivery struct {
	ID             int    `json:"id" gorm:"primaryKey;autoIncrement"`
	SubscriptionID int    `json:"subscriptionId" gorm:"index;not null"`
	Event          string `json:"event" gorm:"size:96;index;not null"`
	Payload        string `json:"-" gorm:"type:text;not null"`
	Status         string `json:"status" gorm:"size:16;index;not null;default:pending"`
	Attempts       int    `json:"attempts" gorm:"not null;default:0"`
	NextAttemptAt  int64  `json:"nextAttemptAt" gorm:"index;not null;default:0"`
	LastError      string `json:"lastError" gorm:"size:512"`
	ResponseStatus int    `json:"responseStatus"`
	CreatedAt      int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	DeliveredAt    int64  `json:"deliveredAt"`
}

func (WebhookDelivery) TableName() string { return "webhook_deliveries" }
