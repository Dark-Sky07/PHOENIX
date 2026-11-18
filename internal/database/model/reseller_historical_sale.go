package model

// ResellerHistoricalSale is a manual administrator-supplied record. It is
// independent of live clients and is not evidence of a verified payment.
type ResellerHistoricalSale struct {
	Id           int    `json:"id" gorm:"primaryKey;autoIncrement"`
	ResellerId   int    `json:"resellerId" gorm:"column:reseller_id;uniqueIndex:idx_reseller_history_reference;index;not null"`
	Reference    string `json:"reference" gorm:"size:128;uniqueIndex:idx_reseller_history_reference;not null"`
	Source       string `json:"source" gorm:"size:512;not null"`
	Email        string `json:"email" gorm:"size:254;not null"`
	TrafficBytes int64  `json:"trafficBytes" gorm:"column:traffic_bytes;not null"`
	AmountMinor  *int64 `json:"amountMinor" gorm:"column:amount_minor"`
	Currency     string `json:"currency" gorm:"size:3"`
	SoldAt       int64  `json:"soldAt" gorm:"column:sold_at;index;not null"`
	RecordedAt   int64  `json:"recordedAt" gorm:"autoCreateTime:milli"`
	ActorType    string `json:"actorType" gorm:"size:32;not null"`
	ActorId      int    `json:"actorId"`
	ActorName    string `json:"actorName" gorm:"size:128;not null"`
}

func (ResellerHistoricalSale) TableName() string { return "reseller_historical_sales" }
