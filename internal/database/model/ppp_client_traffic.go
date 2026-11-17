package model

// PPPClientTrafficCounter stores the last absolute kernel interface counters
// committed for one authenticated PPP session. The caller updates this row and
// ClientTraffic in the same DB transaction, making retries/restarts idempotent.
type PPPClientTrafficCounter struct {
	SessionID string `gorm:"primaryKey;size:128"`
	Email     string `gorm:"index;not null"`
	InboundID int    `gorm:"not null;default:0"`
	RxBytes   int64  `gorm:"not null;default:0"`
	TxBytes   int64  `gorm:"not null;default:0"`
}
