package model

// OpenVPNSessionCounter stores the last absolute counters committed for one
// OpenVPN session. It is updated in the same transaction as client usage, so a
// replayed hook record after a crash cannot charge the same bytes twice.
type OpenVPNSessionCounter struct {
	SessionID string `gorm:"primaryKey;size:192"`
	Email     string `gorm:"index;not null"`
	InboundID int    `gorm:"not null;default:0"`
	RxBytes   int64  `gorm:"not null;default:0"`
	TxBytes   int64  `gorm:"not null;default:0"`
}
