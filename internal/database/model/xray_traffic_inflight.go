package model

// XrayTrafficInFlight is created before a local Xray poll can advance the
// in-memory baseline. Its absence is not proof that unpolled traffic is safe.
// A marker remains until the corresponding journal has been applied and
// removed; an orphan marker requires evidence-based manual reconciliation.
type XrayTrafficInFlight struct {
	ID        string `gorm:"primaryKey"`
	CreatedAt int64  `gorm:"autoCreateTime:milli"`
}
