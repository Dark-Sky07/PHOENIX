package model

// XrayTrafficReceipt identifies a committed local counter batch. Receipts must
// outlive any possible retry; deleting one early could charge a batch twice.
type XrayTrafficReceipt struct {
	ID          string `gorm:"primaryKey"`
	PayloadHash string `gorm:"not null;default:''"`
}
