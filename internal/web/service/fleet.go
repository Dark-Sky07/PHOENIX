package service

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const (
	fleetTemplateSchemaVersion = 1
	fleetBootstrapLifetime     = 15 * time.Minute
	fleetCredentialLifetime    = 90 * 24 * time.Hour
	fleetCredentialRotateAhead = 7 * 24 * time.Hour
	managedTunnelSecretPurpose = "managed-tunnel-provider-v1"
)

var fleetIdentifierRE = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)

// FleetTemplateDocument is intentionally a small declarative contract. There
// is no command, URL, filesystem path, or arbitrary execution surface here.
type FleetTemplateDocument struct {
	SchemaVersion    int               `json:"schemaVersion"`
	Labels           map[string]string `json:"labels,omitempty"`
	Node             FleetNodePolicy   `json:"node"`
	ManagedTunnelIDs []int             `json:"managedTunnelIds,omitempty"`
}

type FleetNodePolicy struct {
	InboundSyncMode string   `json:"inboundSyncMode"`
	InboundTags     []string `json:"inboundTags,omitempty"`
}

type FleetTemplateInput struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Document    json.RawMessage `json:"document"`
}

type FleetTemplateRevisionInput struct {
	Document json.RawMessage `json:"document"`
}

type FleetTemplateView struct {
	ID              int                         `json:"id"`
	Name            string                      `json:"name"`
	Description     string                      `json:"description"`
	CurrentRevision int                         `json:"currentRevision"`
	Enabled         bool                        `json:"enabled"`
	CreatedAt       int64                       `json:"createdAt"`
	UpdatedAt       int64                       `json:"updatedAt"`
	Revisions       []FleetTemplateRevisionView `json:"revisions,omitempty"`
}

type FleetTemplateRevisionView struct {
	Revision  int                   `json:"revision"`
	Document  FleetTemplateDocument `json:"document"`
	SHA256    string                `json:"sha256"`
	CreatedAt int64                 `json:"createdAt"`
}

type FleetEnrollmentInput struct {
	Name   string `json:"name"`
	NodeID *int   `json:"nodeId,omitempty"`
}

type FleetEnrollmentView struct {
	ID                  int                  `json:"id"`
	Name                string               `json:"name"`
	NodeID              *int                 `json:"nodeId,omitempty"`
	AgentID             string               `json:"agentId,omitempty"`
	BootstrapExpiresAt  int64                `json:"bootstrapExpiresAt,omitempty"`
	BootstrapUsedAt     int64                `json:"bootstrapUsedAt,omitempty"`
	CredentialExpiresAt int64                `json:"credentialExpiresAt,omitempty"`
	CredentialRotatedAt int64                `json:"credentialRotatedAt,omitempty"`
	HasCredential       bool                 `json:"hasCredential"`
	Enabled             bool                 `json:"enabled"`
	Capabilities        any                  `json:"capabilities,omitempty"`
	Version             string               `json:"version,omitempty"`
	Status              string               `json:"status"`
	LastCheckInAt       int64                `json:"lastCheckInAt,omitempty"`
	LastError           string               `json:"lastError,omitempty"`
	Assignment          *FleetAssignmentView `json:"assignment,omitempty"`
	CreatedAt           int64                `json:"createdAt"`
	UpdatedAt           int64                `json:"updatedAt"`
}

type FleetAssignmentView struct {
	TemplateID      int    `json:"templateId"`
	TemplateName    string `json:"templateName,omitempty"`
	DesiredRevision int    `json:"desiredRevision"`
	AppliedRevision int    `json:"appliedRevision"`
	Status          string `json:"status"`
	LastError       string `json:"lastError,omitempty"`
	LastAttemptAt   int64  `json:"lastAttemptAt,omitempty"`
	AppliedAt       int64  `json:"appliedAt,omitempty"`
}

type FleetEnrollmentCreateResult struct {
	Enrollment     FleetEnrollmentView `json:"enrollment"`
	BootstrapToken string              `json:"bootstrapToken"`
}

type FleetCredentialRotateResult struct {
	Enrollment FleetEnrollmentView `json:"enrollment"`
	Credential string              `json:"credential"`
}

type FleetDeploymentInput struct {
	Revision      int   `json:"revision"`
	PoolID        int   `json:"poolId,omitempty"`
	EnrollmentIDs []int `json:"enrollmentIds,omitempty"`
}

type FleetPoolInput struct {
	Name          string `json:"name"`
	Description   string `json:"description"`
	TemplateID    *int   `json:"templateId,omitempty"`
	EnrollmentIDs []int  `json:"enrollmentIds"`
	Enabled       bool   `json:"enabled"`
}

type FleetPoolView struct {
	ID            int    `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	TemplateID    *int   `json:"templateId,omitempty"`
	Enabled       bool   `json:"enabled"`
	EnrollmentIDs []int  `json:"enrollmentIds"`
	CreatedAt     int64  `json:"createdAt"`
	UpdatedAt     int64  `json:"updatedAt"`
}

type ManagedTunnelInput struct {
	Name           string          `json:"name"`
	Provider       string          `json:"provider"`
	PublicHostname string          `json:"publicHostname"`
	TargetHost     string          `json:"targetHost"`
	TargetPort     int             `json:"targetPort"`
	Protocol       string          `json:"protocol"`
	ProviderConfig json.RawMessage `json:"providerConfig,omitempty"`
	Enabled        bool            `json:"enabled"`
}

type ManagedTunnelView struct {
	ID                int    `json:"id"`
	Name              string `json:"name"`
	Provider          string `json:"provider"`
	PublicHostname    string `json:"publicHostname"`
	TargetHost        string `json:"targetHost"`
	TargetPort        int    `json:"targetPort"`
	Protocol          string `json:"protocol"`
	HasProviderConfig bool   `json:"hasProviderConfig"`
	Enabled           bool   `json:"enabled"`
	CreatedAt         int64  `json:"createdAt"`
	UpdatedAt         int64  `json:"updatedAt"`
}

// AgentEnrollRequest and AgentCheckInRequest are separate from operator API
// shapes. In particular, only the one-time bootstrap endpoint receives the
// bootstrap value and it is never included in a loggable URL.
type AgentEnrollRequest struct {
	BootstrapToken string          `json:"bootstrapToken"`
	AgentID        string          `json:"agentId"`
	Version        string          `json:"version"`
	Capabilities   json.RawMessage `json:"capabilities,omitempty"`
}

type AgentCheckInRequest struct {
	AgentID         string          `json:"agentId"`
	Version         string          `json:"version"`
	Capabilities    json.RawMessage `json:"capabilities,omitempty"`
	AppliedRevision int             `json:"appliedRevision"`
	ApplyError      string          `json:"applyError,omitempty"`
}

type AgentTunnelDelivery struct {
	ID             int             `json:"id"`
	Name           string          `json:"name"`
	Provider       string          `json:"provider"`
	PublicHostname string          `json:"publicHostname"`
	TargetHost     string          `json:"targetHost"`
	TargetPort     int             `json:"targetPort"`
	Protocol       string          `json:"protocol"`
	ProviderConfig json.RawMessage `json:"providerConfig,omitempty"`
}

type AgentDesiredState struct {
	TemplateID int                   `json:"templateId"`
	Revision   int                   `json:"revision"`
	Document   FleetTemplateDocument `json:"document"`
	Tunnels    []AgentTunnelDelivery `json:"tunnels,omitempty"`
}

type AgentEnrollResponse struct {
	Credential          string             `json:"credential"`
	CredentialExpiresAt int64              `json:"credentialExpiresAt"`
	DesiredState        *AgentDesiredState `json:"desiredState,omitempty"`
}

type AgentCheckInResponse struct {
	Credential          string             `json:"credential,omitempty"`
	CredentialExpiresAt int64              `json:"credentialExpiresAt"`
	DesiredState        *AgentDesiredState `json:"desiredState,omitempty"`
}

type FleetService struct{}

func (FleetService) ListTemplates() ([]FleetTemplateView, error) {
	var rows []model.FleetTemplate
	if err := database.GetDB().Order("name asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]FleetTemplateView, 0, len(rows))
	for i := range rows {
		out = append(out, fleetTemplateView(&rows[i]))
	}
	return out, nil
}

func (FleetService) GetTemplate(id int) (*FleetTemplateView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid fleet template id")
	}
	var row model.FleetTemplate
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	view := fleetTemplateView(&row)
	var revisions []model.FleetTemplateRevision
	if err := database.GetDB().Where("template_id = ?", id).Order("revision asc").Find(&revisions).Error; err != nil {
		return nil, err
	}
	view.Revisions = make([]FleetTemplateRevisionView, 0, len(revisions))
	for i := range revisions {
		doc, err := decodeFleetDocument(revisions[i].Document)
		if err != nil {
			return nil, fmt.Errorf("stored fleet template revision %d is invalid: %w", revisions[i].Revision, err)
		}
		view.Revisions = append(view.Revisions, FleetTemplateRevisionView{
			Revision: revisions[i].Revision, Document: doc, SHA256: revisions[i].SHA256, CreatedAt: revisions[i].CreatedAt,
		})
	}
	return &view, nil
}

func (FleetService) CreateTemplate(in FleetTemplateInput) (*FleetTemplateView, error) {
	name, description, doc, canonical, digest, err := normalizeFleetTemplateInput(in)
	if err != nil {
		return nil, err
	}
	row := model.FleetTemplate{Name: name, Description: description, CurrentRevision: 1, Enabled: true}
	if err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return tx.Create(&model.FleetTemplateRevision{TemplateID: row.ID, Revision: 1, Document: canonical, SHA256: digest}).Error
	}); err != nil {
		return nil, err
	}
	return &FleetTemplateView{ID: row.ID, Name: row.Name, Description: row.Description, CurrentRevision: 1, Enabled: row.Enabled, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt,
		Revisions: []FleetTemplateRevisionView{{Revision: 1, Document: doc, SHA256: digest}}}, nil
}

func (FleetService) SetTemplateEnabled(id int, enabled bool) (*FleetTemplateView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid fleet template id")
	}
	var row model.FleetTemplate
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	if err := database.GetDB().Model(&row).Update("enabled", enabled).Error; err != nil {
		return nil, err
	}
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	view := fleetTemplateView(&row)
	return &view, nil
}

func (FleetService) CreateRevision(id int, in FleetTemplateRevisionInput) (*FleetTemplateView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid fleet template id")
	}
	doc, canonical, digest, err := normalizeFleetDocument(in.Document)
	if err != nil {
		return nil, err
	}
	if err := validateManagedTunnelReferences(doc.ManagedTunnelIDs); err != nil {
		return nil, err
	}
	if err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		var tmpl model.FleetTemplate
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&tmpl, id).Error; err != nil {
			return err
		}
		next := tmpl.CurrentRevision + 1
		if err := tx.Create(&model.FleetTemplateRevision{TemplateID: id, Revision: next, Document: canonical, SHA256: digest}).Error; err != nil {
			return err
		}
		return tx.Model(&tmpl).Update("current_revision", next).Error
	}); err != nil {
		return nil, err
	}
	return (FleetService{}).GetTemplate(id)
}

func (FleetService) ListEnrollments() ([]FleetEnrollmentView, error) {
	var rows []model.FleetAgentEnrollment
	if err := database.GetDB().Order("name asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	assignments, err := fleetAssignmentsByEnrollment()
	if err != nil {
		return nil, err
	}
	out := make([]FleetEnrollmentView, 0, len(rows))
	for i := range rows {
		view := fleetEnrollmentView(&rows[i])
		view.Assignment = assignments[rows[i].ID]
		out = append(out, view)
	}
	return out, nil
}

func (FleetService) ListPools() ([]FleetPoolView, error) {
	var rows []model.FleetPool
	if err := database.GetDB().Order("name asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	members, err := fleetPoolMembers()
	if err != nil {
		return nil, err
	}
	out := make([]FleetPoolView, 0, len(rows))
	for i := range rows {
		out = append(out, fleetPoolView(&rows[i], members[rows[i].ID]))
	}
	return out, nil
}

func (FleetService) CreatePool(in FleetPoolInput) (*FleetPoolView, error) {
	name, description, templateID, members, err := normalizeFleetPoolInput(in)
	if err != nil {
		return nil, err
	}
	row := model.FleetPool{Name: name, Description: description, TemplateID: templateID, Enabled: in.Enabled}
	if err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&row).Error; err != nil {
			return err
		}
		return replaceFleetPoolMembers(tx, row.ID, members)
	}); err != nil {
		return nil, err
	}
	return &FleetPoolView{ID: row.ID, Name: row.Name, Description: row.Description, TemplateID: row.TemplateID, Enabled: row.Enabled, EnrollmentIDs: members, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}, nil
}

func (FleetService) UpdatePool(id int, in FleetPoolInput) (*FleetPoolView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid fleet pool id")
	}
	name, description, templateID, members, err := normalizeFleetPoolInput(in)
	if err != nil {
		return nil, err
	}
	var row model.FleetPool
	if err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := tx.First(&row, id).Error; err != nil {
			return err
		}
		if err := tx.Model(&row).Updates(map[string]any{"name": name, "description": description, "template_id": templateID, "enabled": in.Enabled}).Error; err != nil {
			return err
		}
		return replaceFleetPoolMembers(tx, id, members)
	}); err != nil {
		return nil, err
	}
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	view := fleetPoolView(&row, members)
	return &view, nil
}

func (FleetService) DeletePool(id int) error {
	if id <= 0 {
		return common.NewError("invalid fleet pool id")
	}
	return database.GetDB().Transaction(func(tx *gorm.DB) error {
		if err := tx.Where("pool_id = ?", id).Delete(&model.FleetPoolMember{}).Error; err != nil {
			return err
		}
		result := tx.Delete(&model.FleetPool{}, id)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return gorm.ErrRecordNotFound
		}
		return nil
	})
}

func (FleetService) CreateEnrollment(in FleetEnrollmentInput) (*FleetEnrollmentCreateResult, error) {
	name, err := normalizeFleetName(in.Name, "agent enrollment")
	if err != nil {
		return nil, err
	}
	if in.NodeID != nil {
		if *in.NodeID <= 0 {
			return nil, common.NewError("invalid legacy node id")
		}
		var count int64
		if err := database.GetDB().Model(&model.Node{}).Where("id = ?", *in.NodeID).Count(&count).Error; err != nil {
			return nil, err
		}
		if count == 0 {
			return nil, common.NewError("legacy node does not exist")
		}
	}
	bootstrap, err := fleetRandomToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	row := model.FleetAgentEnrollment{
		Name: name, NodeID: in.NodeID, BootstrapTokenHash: fleetTokenHash(bootstrap),
		BootstrapExpiresAt: now.Add(fleetBootstrapLifetime).UnixMilli(), Enabled: true, Status: "pending",
	}
	if err := database.GetDB().Create(&row).Error; err != nil {
		return nil, err
	}
	return &FleetEnrollmentCreateResult{Enrollment: fleetEnrollmentView(&row), BootstrapToken: bootstrap}, nil
}

func (FleetService) RotateEnrollmentCredential(id int) (*FleetCredentialRotateResult, error) {
	if id <= 0 {
		return nil, common.NewError("invalid fleet enrollment id")
	}
	credential, err := fleetRandomToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var row model.FleetAgentEnrollment
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	if row.AgentID == nil || *row.AgentID == "" {
		return nil, common.NewError("agent has not enrolled; create a new bootstrap enrollment instead")
	}
	updates := map[string]any{
		"credential_hash":       fleetTokenHash(credential),
		"credential_expires_at": now.Add(fleetCredentialLifetime).UnixMilli(),
		"credential_rotated_at": now.UnixMilli(),
	}
	if err := database.GetDB().Model(&row).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	return &FleetCredentialRotateResult{Enrollment: fleetEnrollmentView(&row), Credential: credential}, nil
}

func (FleetService) SetEnrollmentEnabled(id int, enabled bool) (*FleetEnrollmentView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid fleet enrollment id")
	}
	var row model.FleetAgentEnrollment
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	status := row.Status
	if !enabled {
		status = "disabled"
	} else if status == "disabled" {
		status = "pending"
	}
	if err := database.GetDB().Model(&row).Updates(map[string]any{"enabled": enabled, "status": status}).Error; err != nil {
		return nil, err
	}
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return nil, err
	}
	view := fleetEnrollmentView(&row)
	assignments, err := fleetAssignmentsByEnrollment()
	if err == nil {
		view.Assignment = assignments[id]
	}
	return &view, err
}

// Deploy changes only explicit enrollment targets. It is intentionally not an
// implicit "all nodes following this template" action, which lets operators
// stage a canary rollout before selecting the next cohort.
func (FleetService) Deploy(templateID int, in FleetDeploymentInput) ([]FleetAssignmentView, error) {
	if templateID <= 0 || in.Revision <= 0 || len(in.EnrollmentIDs) > 500 || (in.PoolID <= 0 && len(in.EnrollmentIDs) == 0) || (in.PoolID > 0 && len(in.EnrollmentIDs) > 0) {
		return nil, common.NewError("a revision and either a fleet pool or 1-500 enrollment ids are required")
	}
	var result []FleetAssignmentView
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		var revision model.FleetTemplateRevision
		if err := tx.Where("template_id = ? AND revision = ?", templateID, in.Revision).First(&revision).Error; err != nil {
			return common.NewError("fleet template revision does not exist")
		}
		ids, err := deploymentEnrollmentIDs(tx, templateID, in)
		if err != nil {
			return err
		}
		var enrollments []model.FleetAgentEnrollment
		if err := tx.Where("id IN ?", ids).Find(&enrollments).Error; err != nil {
			return err
		}
		if len(enrollments) != len(ids) {
			return common.NewError("one or more fleet enrollments do not exist")
		}
		for i := range enrollments {
			if !enrollments[i].Enabled {
				return common.NewError("disabled fleet enrollment cannot receive a rollout")
			}
		}
		for _, enrollmentID := range ids {
			var assignment model.FleetAssignment
			err := tx.Where("enrollment_id = ?", enrollmentID).First(&assignment).Error
			if errors.Is(err, gorm.ErrRecordNotFound) {
				assignment = model.FleetAssignment{EnrollmentID: enrollmentID, TemplateID: templateID, DesiredRevision: in.Revision, Status: "pending"}
				if err := tx.Create(&assignment).Error; err != nil {
					return err
				}
			} else if err != nil {
				return err
			} else if err := tx.Model(&assignment).Updates(map[string]any{
				"template_id": templateID, "desired_revision": in.Revision, "status": "pending", "last_error": "", "last_attempt_at": int64(0),
			}).Error; err != nil {
				return err
			}
			result = append(result, FleetAssignmentView{TemplateID: templateID, DesiredRevision: in.Revision, AppliedRevision: assignment.AppliedRevision, Status: "pending"})
		}
		return nil
	})
	return result, err
}

func (FleetService) ListTunnels() ([]ManagedTunnelView, error) {
	var rows []model.ManagedTunnel
	if err := database.GetDB().Order("name asc, id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make([]ManagedTunnelView, 0, len(rows))
	for i := range rows {
		out = append(out, managedTunnelView(&rows[i]))
	}
	return out, nil
}

func (FleetService) CreateTunnel(in ManagedTunnelInput) (*ManagedTunnelView, error) {
	row, err := normalizeManagedTunnel(in, nil)
	if err != nil {
		return nil, err
	}
	if err := database.GetDB().Create(row).Error; err != nil {
		return nil, err
	}
	view := managedTunnelView(row)
	return &view, nil
}

func (FleetService) UpdateTunnel(id int, in ManagedTunnelInput) (*ManagedTunnelView, error) {
	if id <= 0 {
		return nil, common.NewError("invalid managed tunnel id")
	}
	var current model.ManagedTunnel
	if err := database.GetDB().First(&current, id).Error; err != nil {
		return nil, err
	}
	row, err := normalizeManagedTunnel(in, &current)
	if err != nil {
		return nil, err
	}
	row.ID, row.CreatedAt = current.ID, current.CreatedAt
	if err := database.GetDB().Save(row).Error; err != nil {
		return nil, err
	}
	view := managedTunnelView(row)
	return &view, nil
}

func (FleetService) DeleteTunnel(id int) error {
	if id <= 0 {
		return common.NewError("invalid managed tunnel id")
	}
	var row model.ManagedTunnel
	if err := database.GetDB().First(&row, id).Error; err != nil {
		return err
	}
	// Revisions are immutable, so deletion would make a historical rollout
	// impossible to reconstruct. Operators disable a referenced tunnel instead;
	// it then disappears from desired state without rewriting history.
	var revisions []model.FleetTemplateRevision
	if err := database.GetDB().Select("document").Find(&revisions).Error; err != nil {
		return err
	}
	for _, revision := range revisions {
		doc, err := decodeFleetDocument(revision.Document)
		if err != nil {
			return fmt.Errorf("inspect fleet template revision: %w", err)
		}
		for _, tunnelID := range doc.ManagedTunnelIDs {
			if tunnelID == id {
				return common.NewError("managed tunnel is referenced by an immutable fleet template revision; disable it instead")
			}
		}
	}
	return database.GetDB().Delete(&row).Error
}

// AgentEnroll atomically consumes a one-time bootstrap credential and mints a
// rotating agent credential. A lost enrollment response is intentionally not
// replayable; the operator can issue a new enrollment rather than exposing a
// retrievable credential in the database.
func (FleetService) AgentEnroll(ctx context.Context, in AgentEnrollRequest) (*AgentEnrollResponse, error) {
	_ = ctx
	if len(in.BootstrapToken) < 32 || !validAgentID(in.AgentID) {
		return nil, errFleetAgentUnauthorized
	}
	capabilities, err := normalizeAgentCapabilities(in.Capabilities)
	if err != nil {
		return nil, errFleetAgentUnauthorized
	}
	version := trimBounded(in.Version, 128)
	if version == "" {
		return nil, errFleetAgentUnauthorized
	}
	bootstrapHash := fleetTokenHash(in.BootstrapToken)
	credential, err := fleetRandomToken()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	var enrollment model.FleetAgentEnrollment
	if err := database.GetDB().Where("bootstrap_token_hash = ?", bootstrapHash).First(&enrollment).Error; err != nil {
		return nil, errFleetAgentUnauthorized
	}
	if !enrollment.Enabled || enrollment.BootstrapUsedAt != 0 || enrollment.BootstrapExpiresAt <= now.UnixMilli() || (enrollment.AgentID != nil && *enrollment.AgentID != in.AgentID) || !constantHashEqual(enrollment.BootstrapTokenHash, bootstrapHash) {
		return nil, errFleetAgentUnauthorized
	}
	updates := map[string]any{
		"agent_id": in.AgentID, "bootstrap_used_at": now.UnixMilli(), "credential_hash": fleetTokenHash(credential),
		"credential_expires_at": now.Add(fleetCredentialLifetime).UnixMilli(), "credential_rotated_at": now.UnixMilli(),
		"capabilities": capabilities, "version": version, "status": "online", "last_check_in_at": now.UnixMilli(), "last_error": "",
	}
	res := database.GetDB().Model(&model.FleetAgentEnrollment{}).Where("id = ? AND bootstrap_token_hash = ? AND bootstrap_used_at = ? AND enabled = ?", enrollment.ID, bootstrapHash, 0, true).Updates(updates)
	if res.Error != nil {
		return nil, res.Error
	}
	if res.RowsAffected != 1 {
		return nil, errFleetAgentUnauthorized
	}
	if err := database.GetDB().First(&enrollment, enrollment.ID).Error; err != nil {
		return nil, err
	}
	desired, err := (FleetService{}).desiredStateForEnrollment(&enrollment)
	if err != nil {
		return nil, err
	}
	return &AgentEnrollResponse{Credential: credential, CredentialExpiresAt: enrollment.CredentialExpiresAt, DesiredState: desired}, nil
}

func (FleetService) AgentCheckIn(ctx context.Context, credential string, in AgentCheckInRequest) (*AgentCheckInResponse, error) {
	_ = ctx
	enrollment, err := authenticateFleetAgent(credential)
	if err != nil {
		return nil, err
	}
	if enrollment.AgentID == nil || in.AgentID != *enrollment.AgentID || !validAgentID(in.AgentID) {
		return nil, errFleetAgentUnauthorized
	}
	capabilities, capErr := normalizeAgentCapabilities(in.Capabilities)
	if capErr != nil {
		return nil, common.NewError("invalid agent capabilities")
	}
	if in.AppliedRevision < 0 || len(in.ApplyError) > 512 {
		return nil, common.NewError("invalid agent acknowledgement")
	}
	now := time.Now()
	version := trimBounded(in.Version, 128)
	updates := map[string]any{"status": "online", "last_check_in_at": now.UnixMilli(), "capabilities": capabilities}
	if version != "" {
		updates["version"] = version
	}
	if err := database.GetDB().Model(enrollment).Updates(updates).Error; err != nil {
		return nil, err
	}
	if err := updateAssignmentAcknowledgement(enrollment.ID, in.AppliedRevision, in.ApplyError, now); err != nil {
		return nil, err
	}
	if err := database.GetDB().First(enrollment, enrollment.ID).Error; err != nil {
		return nil, err
	}
	desired, err := (FleetService{}).desiredStateForEnrollment(enrollment)
	if err != nil {
		_ = database.GetDB().Model(enrollment).Updates(map[string]any{"last_error": trimBounded(err.Error(), 512)}).Error
		return nil, err
	}
	response := &AgentCheckInResponse{CredentialExpiresAt: enrollment.CredentialExpiresAt, DesiredState: desired}
	if time.UnixMilli(enrollment.CredentialExpiresAt).Before(now.Add(fleetCredentialRotateAhead)) {
		next, rotateErr := rotateFleetAgentCredential(enrollment)
		if rotateErr != nil {
			return nil, rotateErr
		}
		response.Credential = next
		response.CredentialExpiresAt = now.Add(fleetCredentialLifetime).UnixMilli()
	}
	return response, nil
}

func (FleetService) desiredStateForEnrollment(enrollment *model.FleetAgentEnrollment) (*AgentDesiredState, error) {
	if enrollment == nil || !enrollment.Enabled {
		return nil, nil
	}
	var assignment model.FleetAssignment
	if err := database.GetDB().Where("enrollment_id = ?", enrollment.ID).First(&assignment).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var template model.FleetTemplate
	if err := database.GetDB().First(&template, assignment.TemplateID).Error; err != nil {
		return nil, err
	}
	if !template.Enabled {
		return nil, nil
	}
	var revision model.FleetTemplateRevision
	if err := database.GetDB().Where("template_id = ? AND revision = ?", assignment.TemplateID, assignment.DesiredRevision).First(&revision).Error; err != nil {
		return nil, err
	}
	doc, err := decodeFleetDocument(revision.Document)
	if err != nil {
		return nil, err
	}
	state := &AgentDesiredState{TemplateID: assignment.TemplateID, Revision: assignment.DesiredRevision, Document: doc}
	for _, id := range doc.ManagedTunnelIDs {
		var tunnel model.ManagedTunnel
		if err := database.GetDB().First(&tunnel, id).Error; err != nil {
			return nil, fmt.Errorf("managed tunnel %d required by template is unavailable", id)
		}
		if !tunnel.Enabled {
			continue
		}
		config, err := decryptPanelSecret(managedTunnelSecretPurpose, tunnel.ProviderConfigEncrypted)
		if err != nil {
			return nil, fmt.Errorf("decrypt managed tunnel %q: %w", tunnel.Name, err)
		}
		item := AgentTunnelDelivery{ID: tunnel.ID, Name: tunnel.Name, Provider: tunnel.Provider, PublicHostname: tunnel.PublicHostname, TargetHost: tunnel.TargetHost, TargetPort: tunnel.TargetPort, Protocol: tunnel.Protocol}
		if config != "" {
			item.ProviderConfig = json.RawMessage(config)
		}
		state.Tunnels = append(state.Tunnels, item)
	}
	return state, nil
}

func deploymentEnrollmentIDs(tx *gorm.DB, templateID int, in FleetDeploymentInput) ([]int, error) {
	if in.PoolID <= 0 {
		return uniquePositiveIDs(in.EnrollmentIDs)
	}
	var pool model.FleetPool
	if err := tx.First(&pool, in.PoolID).Error; err != nil {
		return nil, err
	}
	if !pool.Enabled {
		return nil, common.NewError("fleet pool is disabled")
	}
	if pool.TemplateID != nil && *pool.TemplateID != templateID {
		return nil, common.NewError("fleet pool is bound to a different template")
	}
	var members []model.FleetPoolMember
	if err := tx.Where("pool_id = ?", pool.ID).Order("enrollment_id asc").Find(&members).Error; err != nil {
		return nil, err
	}
	ids := make([]int, 0, len(members))
	for _, member := range members {
		ids = append(ids, member.EnrollmentID)
	}
	if len(ids) == 0 {
		return nil, common.NewError("fleet pool has no enrollments")
	}
	return uniquePositiveIDs(ids)
}

func normalizeFleetPoolInput(in FleetPoolInput) (string, string, *int, []int, error) {
	name, err := normalizeFleetName(in.Name, "fleet pool")
	if err != nil {
		return "", "", nil, nil, err
	}
	members, err := uniquePositiveIDs(in.EnrollmentIDs)
	if err != nil {
		return "", "", nil, nil, err
	}
	if len(members) > 500 {
		return "", "", nil, nil, common.NewError("fleet pool can contain at most 500 enrollments")
	}
	if in.TemplateID != nil {
		if *in.TemplateID <= 0 {
			return "", "", nil, nil, common.NewError("invalid fleet pool template id")
		}
		var count int64
		if err := database.GetDB().Model(&model.FleetTemplate{}).Where("id = ?", *in.TemplateID).Count(&count).Error; err != nil {
			return "", "", nil, nil, err
		}
		if count != 1 {
			return "", "", nil, nil, common.NewError("fleet pool template does not exist")
		}
	}
	if err := validateFleetEnrollmentIDs(database.GetDB(), members); err != nil {
		return "", "", nil, nil, err
	}
	return name, trimBounded(in.Description, 512), in.TemplateID, members, nil
}

func replaceFleetPoolMembers(tx *gorm.DB, poolID int, enrollmentIDs []int) error {
	if err := validateFleetEnrollmentIDs(tx, enrollmentIDs); err != nil {
		return err
	}
	if err := tx.Where("pool_id = ?", poolID).Delete(&model.FleetPoolMember{}).Error; err != nil {
		return err
	}
	for _, enrollmentID := range enrollmentIDs {
		if err := tx.Create(&model.FleetPoolMember{PoolID: poolID, EnrollmentID: enrollmentID}).Error; err != nil {
			return err
		}
	}
	return nil
}

func validateFleetEnrollmentIDs(db *gorm.DB, ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	var count int64
	if err := db.Model(&model.FleetAgentEnrollment{}).Where("id IN ?", ids).Count(&count).Error; err != nil {
		return err
	}
	if int(count) != len(ids) {
		return common.NewError("one or more fleet enrollments do not exist")
	}
	return nil
}

func fleetPoolMembers() (map[int][]int, error) {
	var rows []model.FleetPoolMember
	if err := database.GetDB().Order("pool_id asc, enrollment_id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	out := make(map[int][]int)
	for _, row := range rows {
		out[row.PoolID] = append(out[row.PoolID], row.EnrollmentID)
	}
	return out, nil
}

func fleetPoolView(row *model.FleetPool, members []int) FleetPoolView {
	return FleetPoolView{ID: row.ID, Name: row.Name, Description: row.Description, TemplateID: row.TemplateID, Enabled: row.Enabled, EnrollmentIDs: members, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func normalizeFleetTemplateInput(in FleetTemplateInput) (string, string, FleetTemplateDocument, string, string, error) {
	name, err := normalizeFleetName(in.Name, "fleet template")
	if err != nil {
		return "", "", FleetTemplateDocument{}, "", "", err
	}
	doc, canonical, digest, err := normalizeFleetDocument(in.Document)
	if err != nil {
		return "", "", FleetTemplateDocument{}, "", "", err
	}
	if err := validateManagedTunnelReferences(doc.ManagedTunnelIDs); err != nil {
		return "", "", FleetTemplateDocument{}, "", "", err
	}
	return name, trimBounded(in.Description, 512), doc, canonical, digest, nil
}

func normalizeFleetDocument(raw json.RawMessage) (FleetTemplateDocument, string, string, error) {
	if len(raw) == 0 || len(raw) > 64*1024 {
		return FleetTemplateDocument{}, "", "", common.NewError("fleet template document must be 1-65536 bytes")
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	var doc FleetTemplateDocument
	if err := decoder.Decode(&doc); err != nil {
		return FleetTemplateDocument{}, "", "", common.NewError("invalid fleet template document")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return FleetTemplateDocument{}, "", "", common.NewError("invalid fleet template document")
	}
	if doc.SchemaVersion != fleetTemplateSchemaVersion {
		return FleetTemplateDocument{}, "", "", common.NewError("unsupported fleet template schemaVersion")
	}
	if len(doc.Labels) > 32 || len(doc.ManagedTunnelIDs) > 64 || len(doc.Node.InboundTags) > 512 {
		return FleetTemplateDocument{}, "", "", common.NewError("fleet template contains too many entries")
	}
	for key, value := range doc.Labels {
		if !fleetIdentifierRE.MatchString(key) || trimBounded(value, 128) != value || value == "" {
			return FleetTemplateDocument{}, "", "", common.NewError("invalid fleet template label")
		}
	}
	if doc.Node.InboundSyncMode != "all" && doc.Node.InboundSyncMode != "selected" {
		return FleetTemplateDocument{}, "", "", common.NewError("node.inboundSyncMode must be all or selected")
	}
	if doc.Node.InboundSyncMode == "all" && len(doc.Node.InboundTags) > 0 {
		return FleetTemplateDocument{}, "", "", common.NewError("node.inboundTags requires selected sync mode")
	}
	tags := make(map[string]struct{}, len(doc.Node.InboundTags))
	normalizedTags := make([]string, 0, len(doc.Node.InboundTags))
	for _, rawTag := range doc.Node.InboundTags {
		tag := strings.TrimSpace(rawTag)
		if tag == "" || len(tag) > 128 {
			return FleetTemplateDocument{}, "", "", common.NewError("invalid node inbound tag")
		}
		if _, duplicate := tags[tag]; duplicate {
			return FleetTemplateDocument{}, "", "", common.NewError("duplicate node inbound tag")
		}
		tags[tag] = struct{}{}
		normalizedTags = append(normalizedTags, tag)
	}
	if doc.Node.InboundSyncMode == "selected" && len(tags) == 0 {
		return FleetTemplateDocument{}, "", "", common.NewError("selected sync mode requires inbound tags")
	}
	ids, err := uniquePositiveIDs(doc.ManagedTunnelIDs)
	if err != nil {
		return FleetTemplateDocument{}, "", "", err
	}
	doc.ManagedTunnelIDs = ids
	doc.Node.InboundTags = normalizedTags
	sort.Strings(doc.Node.InboundTags)
	canonical, err := json.Marshal(doc)
	if err != nil {
		return FleetTemplateDocument{}, "", "", err
	}
	sum := sha256.Sum256(canonical)
	return doc, string(canonical), hex.EncodeToString(sum[:]), nil
}

func decodeFleetDocument(raw string) (FleetTemplateDocument, error) {
	doc, _, _, err := normalizeFleetDocument(json.RawMessage(raw))
	return doc, err
}

func validateManagedTunnelReferences(ids []int) error {
	if len(ids) == 0 {
		return nil
	}
	var count int64
	if err := database.GetDB().Model(&model.ManagedTunnel{}).Where("id IN ?", ids).Count(&count).Error; err != nil {
		return err
	}
	if int(count) != len(ids) {
		return common.NewError("one or more managed tunnel ids do not exist")
	}
	return nil
}

func normalizeManagedTunnel(in ManagedTunnelInput, current *model.ManagedTunnel) (*model.ManagedTunnel, error) {
	name, err := normalizeFleetName(in.Name, "managed tunnel")
	if err != nil {
		return nil, err
	}
	provider := strings.ToLower(strings.TrimSpace(in.Provider))
	if !fleetIdentifierRE.MatchString(provider) {
		return nil, common.NewError("invalid managed tunnel provider")
	}
	hostname := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.PublicHostname), "."))
	if !validPublicHostname(hostname) {
		return nil, common.NewError("invalid managed tunnel public hostname")
	}
	target := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(in.TargetHost), "."))
	if !validManagedTunnelTarget(target) {
		return nil, common.NewError("managed tunnel target must be localhost or a private IP")
	}
	if in.TargetPort < 1 || in.TargetPort > 65535 {
		return nil, common.NewError("managed tunnel target port must be 1-65535")
	}
	protocol := strings.ToLower(strings.TrimSpace(in.Protocol))
	if protocol != "http" && protocol != "https" && protocol != "tcp" && protocol != "udp" {
		return nil, common.NewError("managed tunnel protocol must be http, https, tcp, or udp")
	}
	row := &model.ManagedTunnel{Name: name, Provider: provider, PublicHostname: hostname, TargetHost: target, TargetPort: in.TargetPort, Protocol: protocol, Enabled: in.Enabled}
	if current != nil {
		row.ProviderConfigEncrypted = current.ProviderConfigEncrypted
	}
	if in.ProviderConfig != nil {
		config, err := normalizeProviderConfig(in.ProviderConfig)
		if err != nil {
			return nil, err
		}
		ciphertext, err := encryptPanelSecret(managedTunnelSecretPurpose, config)
		if err != nil {
			return nil, err
		}
		row.ProviderConfigEncrypted = ciphertext
	}
	return row, nil
}

func normalizeProviderConfig(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "", nil
	}
	if len(raw) > 16*1024 || !json.Valid(raw) {
		return "", common.NewError("managed tunnel providerConfig must be valid JSON up to 16384 bytes")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return "", common.NewError("managed tunnel providerConfig must be a JSON object")
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func validPublicHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 || net.ParseIP(host) != nil || !strings.Contains(host, ".") {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, r := range label {
			if !(r == '-' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9') {
				return false
			}
		}
	}
	return true
}

func validManagedTunnelTarget(host string) bool {
	if host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && (ip.IsLoopback() || ip.IsPrivate())
}

func fleetAssignmentsByEnrollment() (map[int]*FleetAssignmentView, error) {
	var rows []model.FleetAssignment
	if err := database.GetDB().Order("enrollment_id asc").Find(&rows).Error; err != nil {
		return nil, err
	}
	templateNames := make(map[int]string)
	var templates []model.FleetTemplate
	if err := database.GetDB().Find(&templates).Error; err != nil {
		return nil, err
	}
	for i := range templates {
		templateNames[templates[i].ID] = templates[i].Name
	}
	out := make(map[int]*FleetAssignmentView, len(rows))
	for i := range rows {
		out[rows[i].EnrollmentID] = &FleetAssignmentView{TemplateID: rows[i].TemplateID, TemplateName: templateNames[rows[i].TemplateID], DesiredRevision: rows[i].DesiredRevision, AppliedRevision: rows[i].AppliedRevision, Status: rows[i].Status, LastError: rows[i].LastError, LastAttemptAt: rows[i].LastAttemptAt, AppliedAt: rows[i].AppliedAt}
	}
	return out, nil
}

func updateAssignmentAcknowledgement(enrollmentID, appliedRevision int, applyError string, now time.Time) error {
	var assignment model.FleetAssignment
	if err := database.GetDB().Where("enrollment_id = ?", enrollmentID).First(&assignment).Error; errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	} else if err != nil {
		return err
	}
	updates := map[string]any{"last_attempt_at": now.UnixMilli()}
	if applyError != "" {
		updates["status"] = "failed"
		updates["last_error"] = trimBounded(applyError, 512)
	} else if appliedRevision == assignment.DesiredRevision {
		updates["status"] = "applied"
		updates["applied_revision"] = appliedRevision
		updates["applied_at"] = now.UnixMilli()
		updates["last_error"] = ""
	} else {
		updates["status"] = "pending"
	}
	return database.GetDB().Model(&assignment).Updates(updates).Error
}

func authenticateFleetAgent(credential string) (*model.FleetAgentEnrollment, error) {
	if len(credential) < 32 {
		return nil, errFleetAgentUnauthorized
	}
	hash := fleetTokenHash(credential)
	var enrollment model.FleetAgentEnrollment
	if err := database.GetDB().Where("credential_hash = ?", hash).First(&enrollment).Error; err != nil {
		return nil, errFleetAgentUnauthorized
	}
	if !enrollment.Enabled || enrollment.CredentialExpiresAt <= time.Now().UnixMilli() || !constantHashEqual(enrollment.CredentialHash, hash) {
		return nil, errFleetAgentUnauthorized
	}
	return &enrollment, nil
}

func rotateFleetAgentCredential(enrollment *model.FleetAgentEnrollment) (string, error) {
	if enrollment == nil {
		return "", errFleetAgentUnauthorized
	}
	next, err := fleetRandomToken()
	if err != nil {
		return "", err
	}
	now := time.Now()
	updates := map[string]any{"credential_hash": fleetTokenHash(next), "credential_expires_at": now.Add(fleetCredentialLifetime).UnixMilli(), "credential_rotated_at": now.UnixMilli()}
	if err := database.GetDB().Model(enrollment).Updates(updates).Error; err != nil {
		return "", err
	}
	return next, nil
}

func fleetTemplateView(row *model.FleetTemplate) FleetTemplateView {
	return FleetTemplateView{ID: row.ID, Name: row.Name, Description: row.Description, CurrentRevision: row.CurrentRevision, Enabled: row.Enabled, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func fleetEnrollmentView(row *model.FleetAgentEnrollment) FleetEnrollmentView {
	var capabilities any
	if row.Capabilities != "" {
		_ = json.Unmarshal([]byte(row.Capabilities), &capabilities)
	}
	agentID := ""
	if row.AgentID != nil {
		agentID = *row.AgentID
	}
	return FleetEnrollmentView{ID: row.ID, Name: row.Name, NodeID: row.NodeID, AgentID: agentID, BootstrapExpiresAt: row.BootstrapExpiresAt, BootstrapUsedAt: row.BootstrapUsedAt, CredentialExpiresAt: row.CredentialExpiresAt, CredentialRotatedAt: row.CredentialRotatedAt, HasCredential: row.CredentialHash != "", Enabled: row.Enabled, Capabilities: capabilities, Version: row.Version, Status: row.Status, LastCheckInAt: row.LastCheckInAt, LastError: row.LastError, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func managedTunnelView(row *model.ManagedTunnel) ManagedTunnelView {
	return ManagedTunnelView{ID: row.ID, Name: row.Name, Provider: row.Provider, PublicHostname: row.PublicHostname, TargetHost: row.TargetHost, TargetPort: row.TargetPort, Protocol: row.Protocol, HasProviderConfig: row.ProviderConfigEncrypted != "", Enabled: row.Enabled, CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt}
}

func normalizeAgentCapabilities(raw json.RawMessage) (string, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return "{}", nil
	}
	if len(raw) > 8192 || !json.Valid(raw) {
		return "", errors.New("invalid capabilities")
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil || len(object) > 64 {
		return "", errors.New("invalid capabilities")
	}
	for key, value := range object {
		if len(key) == 0 || len(key) > 64 || len(value) > 1024 {
			return "", errors.New("invalid capabilities")
		}
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return "", err
	}
	return string(canonical), nil
}

func normalizeFleetName(raw, kind string) (string, error) {
	name := strings.TrimSpace(raw)
	if len(name) == 0 || len(name) > 64 {
		return "", fmt.Errorf("%s name must be 1-64 characters", kind)
	}
	return name, nil
}

func uniquePositiveIDs(ids []int) ([]int, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	seen := make(map[int]struct{}, len(ids))
	out := make([]int, 0, len(ids))
	for _, id := range ids {
		if id <= 0 {
			return nil, common.NewError("ids must be positive")
		}
		if _, exists := seen[id]; exists {
			return nil, common.NewError("ids must be unique")
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	sort.Ints(out)
	return out, nil
}

func validAgentID(value string) bool {
	if len(value) < 16 || len(value) > 80 {
		return false
	}
	for _, r := range value {
		if !(r == '-' || r == '_' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}

func trimBounded(value string, max int) string {
	value = strings.TrimSpace(value)
	if len(value) > max {
		return ""
	}
	return value
}

func fleetRandomToken() (string, error) {
	raw := make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func fleetTokenHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func constantHashEqual(left, right string) bool {
	if len(left) != len(right) {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(left), []byte(right)) == 1
}

var errFleetAgentUnauthorized = errors.New("fleet agent authentication failed")

// ErrFleetAgentUnauthorized exposes identity comparison to the HTTP boundary
// without leaking the sentinel itself as a public mutable value.
func ErrFleetAgentUnauthorized() error { return errFleetAgentUnauthorized }
