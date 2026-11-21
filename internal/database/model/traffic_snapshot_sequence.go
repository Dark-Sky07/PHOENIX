package model

// TrafficSnapshotReceiver is provisioned out of band for a single trusted
// sender/receiver relationship. Generation changes require reconciliation.
// It does not contain credentials or attest that a peer is upgraded.
type TrafficSnapshotReceiver struct {
	ID           string `gorm:"primaryKey"`
	Sender       string `gorm:"not null"`
	Generation   string `gorm:"not null"`
	LastSequence int64  `gorm:"not null"`
}

// TrafficSnapshotSender holds a durable sequence for one provisioned
// sender/receiver relationship. It must not be reset when rotating secrets.
type TrafficSnapshotSender struct {
	ID           string `gorm:"primaryKey"`
	Sender       string `gorm:"not null"`
	Generation   string `gorm:"not null"`
	LastSequence int64  `gorm:"not null"`
}
