package model

// ResellerCreationIntent records an in-flight single-client creation across
// process crashes. Its presence requires reconciliation, not automatic replay.
type ResellerCreationIntent struct {
	Id             string `gorm:"primaryKey;size:36"`
	ResellerId     int    `gorm:"index;not null"`
	Email          string `gorm:"not null"`
	SubID          string `gorm:"not null"`
	RequestedBytes int64  `gorm:"not null"`
	InboundIDs     string `gorm:"not null"`
	AmountMinor    *int64
	Currency       string
	CreatedAt      int64 `gorm:"autoCreateTime:milli"`
}
