package model

// NodeId zero identifies the local Xray core; remote node IDs are positive.
// A counter decrease means traffic may have been lost between polls. This
// durable marker must not be cleared automatically when counters rise again.
type ResellerNodeCounterIncident struct {
	Id           int    `gorm:"primaryKey;autoIncrement"`
	NodeId       int    `gorm:"uniqueIndex:idx_reseller_node_counter_incident,priority:1;not null"`
	Direction    string // Empty for node snapshots; local Xray records one direction.
	Email        string `gorm:"uniqueIndex:idx_reseller_node_counter_incident,priority:2;not null"`
	PreviousUp   int64
	PreviousDown int64
	ObservedUp   int64
	ObservedDown int64
	CreatedAt    int64 `gorm:"autoCreateTime:milli"`
}
