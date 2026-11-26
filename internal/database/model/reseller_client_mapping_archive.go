package model

// ResellerClientMappingArchive preserves a dangling reseller->client mapping
// before an operator-approved reconciliation removes it. It records evidence
// only; it never grants access, quota, or visibility by itself.
type ResellerClientMappingArchive struct {
	ID         int64  `gorm:"primaryKey;autoIncrement" json:"id"`
	ResellerID int    `gorm:"column:reseller_id;index;not null" json:"resellerId"`
	Email      string `gorm:"column:email;not null" json:"email"`
	CreatedAt  int64  `gorm:"column:created_at" json:"createdAt"`
	ArchivedAt int64  `gorm:"column:archived_at;autoCreateTime:milli" json:"archivedAt"`
	Reason     string `gorm:"column:reason" json:"reason"`
	BackupPath string `gorm:"column:backup_path" json:"backupPath"`
}

func (ResellerClientMappingArchive) TableName() string {
	return "reseller_client_mapping_archives"
}
