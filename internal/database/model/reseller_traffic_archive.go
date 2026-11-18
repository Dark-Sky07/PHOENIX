package model

// ResellerTrafficArchive records a monotonic snapshot of measured usage at a
// client deletion boundary. It survives deletion of the client and mapping.
type ResellerTrafficArchive struct {
	Id             int    `json:"id" gorm:"primaryKey;autoIncrement"`
	ResellerId     int    `json:"resellerId" gorm:"column:reseller_id;uniqueIndex:idx_reseller_traffic_archive_client;index;not null"`
	ClientRecordId int    `json:"clientRecordId" gorm:"column:client_record_id;uniqueIndex:idx_reseller_traffic_archive_client;not null"`
	Email          string `json:"email" gorm:"size:254;not null"`
	ConsumedBytes  int64  `json:"consumedBytes" gorm:"column:consumed_bytes;not null;default:0"`
	CreatedAt      int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt      int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (ResellerTrafficArchive) TableName() string { return "reseller_traffic_archives" }
