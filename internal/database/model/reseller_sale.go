package model

// ResellerSale is an immutable snapshot of client quota sold by a reseller at
// account creation. It intentionally records units (accounts and traffic), not
// currency: PHOENIX does not currently capture the reseller's retail sale price.
type ResellerSale struct {
	Id             int    `json:"id" gorm:"primaryKey;autoIncrement"`
	ResellerId     int    `json:"resellerId" gorm:"column:reseller_id;uniqueIndex:idx_reseller_sales_client;index"`
	ClientRecordId int    `json:"clientRecordId" gorm:"column:client_record_id;uniqueIndex:idx_reseller_sales_client"`
	Email          string `json:"email" gorm:"not null;index"`
	TrafficBytes   int64  `json:"trafficBytes" gorm:"column:traffic_bytes;not null;default:0"`
	CreatedAt      int64  `json:"createdAt" gorm:"column:created_at;index"`
}

func (ResellerSale) TableName() string { return "reseller_sales" }
