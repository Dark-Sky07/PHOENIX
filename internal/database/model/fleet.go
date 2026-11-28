package model

// FleetTemplate is the mutable identity and rollout switch for a fleet
// configuration. The actual configuration is kept in immutable revision rows.
type FleetTemplate struct {
	ID              int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name            string `json:"name" gorm:"size:64;uniqueIndex;not null"`
	Description     string `json:"description" gorm:"size:512"`
	CurrentRevision int    `json:"currentRevision" gorm:"not null;default:1"`
	Enabled         bool   `json:"enabled" gorm:"not null;default:true;index"`
	CreatedAt       int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt       int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (FleetTemplate) TableName() string { return "fleet_templates" }

// FleetTemplateRevision is append-only. Document contains canonical JSON and
// SHA256 is the digest of those canonical bytes, not an operator-supplied
// value. Keeping revisions immutable makes staged rollout and rollback
// deterministic even after a template is edited again.
type FleetTemplateRevision struct {
	ID         int    `json:"id" gorm:"primaryKey;autoIncrement"`
	TemplateID int    `json:"templateId" gorm:"not null;uniqueIndex:idx_fleet_template_revision,priority:1"`
	Revision   int    `json:"revision" gorm:"not null;uniqueIndex:idx_fleet_template_revision,priority:2"`
	Document   string `json:"document" gorm:"type:text;not null"`
	SHA256     string `json:"sha256" gorm:"size:64;not null"`
	CreatedAt  int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
}

func (FleetTemplateRevision) TableName() string { return "fleet_template_revisions" }

// FleetAgentEnrollment represents an outbound-only PHOENIX Node Agent.
// BootstrapTokenHash and CredentialHash are SHA-256 hashes of high-entropy
// bearer values. Plaintext values are deliberately never persisted or emitted
// by normal read APIs.
type FleetAgentEnrollment struct {
	ID     int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name   string `json:"name" gorm:"size:64;uniqueIndex;not null"`
	NodeID *int   `json:"nodeId,omitempty" gorm:"index"`
	// AgentID is nullable so multiple unconsumed enrollment rows coexist on
	// SQLite/PostgreSQL. A unique index still prevents two live enrollments
	// from claiming the same stable agent identity.
	AgentID             *string `json:"agentId,omitempty" gorm:"size:80;uniqueIndex"`
	BootstrapTokenHash  string  `json:"-" gorm:"column:bootstrap_token_hash;size:64;index"`
	BootstrapExpiresAt  int64   `json:"bootstrapExpiresAt"`
	BootstrapUsedAt     int64   `json:"bootstrapUsedAt"`
	CredentialHash      string  `json:"-" gorm:"column:credential_hash;size:64;index"`
	CredentialExpiresAt int64   `json:"credentialExpiresAt"`
	CredentialRotatedAt int64   `json:"credentialRotatedAt"`
	Enabled             bool    `json:"enabled" gorm:"not null;default:true;index"`
	Capabilities        string  `json:"capabilities" gorm:"type:text"`
	Version             string  `json:"version" gorm:"size:128"`
	Status              string  `json:"status" gorm:"size:24;not null;default:pending"`
	LastCheckInAt       int64   `json:"lastCheckInAt"`
	LastError           string  `json:"lastError" gorm:"size:512"`
	CreatedAt           int64   `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt           int64   `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (FleetAgentEnrollment) TableName() string { return "fleet_agent_enrollments" }

// FleetAssignment is deliberately one current assignment per agent. Switching
// templates and choosing a prior revision are explicit desired-state changes;
// no new template automatically reaches an agent.
type FleetAssignment struct {
	ID              int    `json:"id" gorm:"primaryKey;autoIncrement"`
	EnrollmentID    int    `json:"enrollmentId" gorm:"not null;uniqueIndex"`
	TemplateID      int    `json:"templateId" gorm:"not null;index"`
	DesiredRevision int    `json:"desiredRevision" gorm:"not null"`
	AppliedRevision int    `json:"appliedRevision"`
	Status          string `json:"status" gorm:"size:24;not null;default:pending;index"`
	LastError       string `json:"lastError" gorm:"size:512"`
	LastAttemptAt   int64  `json:"lastAttemptAt"`
	AppliedAt       int64  `json:"appliedAt"`
	CreatedAt       int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt       int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (FleetAssignment) TableName() string { return "fleet_assignments" }

// FleetPool is an explicit reusable cohort. A pool can optionally be bound to
// a template so an accidental deploy of a different template is rejected.
type FleetPool struct {
	ID          int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name        string `json:"name" gorm:"size:64;uniqueIndex;not null"`
	Description string `json:"description" gorm:"size:512"`
	TemplateID  *int   `json:"templateId,omitempty" gorm:"index"`
	Enabled     bool   `json:"enabled" gorm:"not null;default:true;index"`
	CreatedAt   int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt   int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (FleetPool) TableName() string { return "fleet_pools" }

// FleetPoolMember is intentionally separate rather than a pool_id column on
// enrollments: an agent can participate in an edge pool and a maintenance
// canary cohort without changing its legacy node record.
type FleetPoolMember struct {
	ID           int   `json:"id" gorm:"primaryKey;autoIncrement"`
	PoolID       int   `json:"poolId" gorm:"not null;uniqueIndex:idx_fleet_pool_member,priority:1"`
	EnrollmentID int   `json:"enrollmentId" gorm:"not null;uniqueIndex:idx_fleet_pool_member,priority:2;index"`
	CreatedAt    int64 `json:"createdAt" gorm:"autoCreateTime:milli"`
}

func (FleetPoolMember) TableName() string { return "fleet_pool_members" }

// FleetAgentTunnelStatus is the agent-reported runtime observation for an
// assigned managed tunnel. It is operational telemetry only: it never carries
// provider credentials or is used as a traffic/accounting authority.
type FleetAgentTunnelStatus struct {
	ID           int    `json:"id" gorm:"primaryKey;autoIncrement"`
	EnrollmentID int    `json:"enrollmentId" gorm:"not null;uniqueIndex:idx_fleet_agent_tunnel_status,priority:1;index"`
	TunnelID     int    `json:"tunnelId" gorm:"not null;uniqueIndex:idx_fleet_agent_tunnel_status,priority:2;index"`
	Provider     string `json:"provider" gorm:"size:32;not null"`
	Status       string `json:"status" gorm:"size:24;not null;default:unknown;index"`
	PID          int    `json:"pid" gorm:"column:pid"`
	LastError    string `json:"lastError" gorm:"size:512"`
	ObservedAt   int64  `json:"observedAt"`
	UpdatedAt    int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (FleetAgentTunnelStatus) TableName() string { return "fleet_agent_tunnel_status" }

// ManagedTunnel is a provider-neutral connector endpoint contract. The
// provider credential is encrypted with the panel secret and has no JSON tag
// so it cannot be returned to an operator by accident.
type ManagedTunnel struct {
	ID                      int    `json:"id" gorm:"primaryKey;autoIncrement"`
	Name                    string `json:"name" gorm:"size:64;uniqueIndex;not null"`
	Provider                string `json:"provider" gorm:"size:32;not null"`
	PublicHostname          string `json:"publicHostname" gorm:"column:public_hostname;size:253;not null"`
	TargetHost              string `json:"targetHost" gorm:"column:target_host;size:253;not null"`
	TargetPort              int    `json:"targetPort" gorm:"column:target_port;not null"`
	Protocol                string `json:"protocol" gorm:"size:8;not null"`
	ProviderConfigEncrypted string `json:"-" gorm:"column:provider_config_encrypted;type:text"`
	Enabled                 bool   `json:"enabled" gorm:"not null;default:true;index"`
	CreatedAt               int64  `json:"createdAt" gorm:"autoCreateTime:milli"`
	UpdatedAt               int64  `json:"updatedAt" gorm:"autoUpdateTime:milli"`
}

func (ManagedTunnel) TableName() string { return "managed_tunnels" }
