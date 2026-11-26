package model

// ResellerSale is a snapshot of an account created by a reseller. AmountMinor
// is self-declared, not proof of a customer payment; nil means unknown.
type ResellerSale struct {
	Id             int    `json:"id" gorm:"primaryKey;autoIncrement"`
	ResellerId     int    `json:"resellerId" gorm:"column:reseller_id;uniqueIndex:idx_reseller_sales_client;index"`
	ClientRecordId int    `json:"clientRecordId" gorm:"column:client_record_id;uniqueIndex:idx_reseller_sales_client"`
	Email          string `json:"email" gorm:"not null;index"`
	TrafficBytes   int64  `json:"trafficBytes" gorm:"column:traffic_bytes;not null;default:0"`
	AmountMinor    *int64 `json:"amountMinor" gorm:"column:amount_minor"`
	Currency       string `json:"currency" gorm:"column:currency;size:3"`
	CreatedAt      int64  `json:"createdAt" gorm:"column:created_at;index"`
}

func (ResellerSale) TableName() string { return "reseller_sales" }
