package service

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/common"
	"gorm.io/gorm"
)

// TicketService manages SupportTickets and SupportTicketMessages between
// Resellers and Panel Admins.
type TicketService struct{}

func normalizeTicketPriority(p string) string {
	switch strings.ToLower(strings.TrimSpace(p)) {
	case "low":
		return "low"
	case "high":
		return "high"
	default:
		return "normal"
	}
}

func (s *TicketService) ListForAdmin(status string) ([]model.SupportTicket, error) {
	db := database.GetDB()
	q := db.Model(&model.SupportTicket{})
	status = strings.ToLower(strings.TrimSpace(status))
	if status != "" && status != "all" {
		q = q.Where("status = ?", status)
	}
	var rows []model.SupportTicket
	if err := q.Order("updated_at DESC, id DESC").Limit(500).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *TicketService) ListForReseller(resellerId int) ([]model.SupportTicket, error) {
	db := database.GetDB()
	var rows []model.SupportTicket
	if err := db.Where("reseller_id = ?", resellerId).
		Order("updated_at DESC, id DESC").
		Limit(200).
		Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *TicketService) GetTicketWithMessages(ticketId int, resellerId int) (*model.SupportTicket, []model.SupportTicketMessage, error) {
	if ticketId <= 0 {
		return nil, nil, common.NewError("invalid ticket id")
	}
	db := database.GetDB()
	var t model.SupportTicket
	if err := db.First(&t, ticketId).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil, nil, common.NewError("ticket not found")
		}
		return nil, nil, err
	}
	if resellerId > 0 && t.ResellerId != resellerId {
		return nil, nil, common.NewError("ticket not found")
	}
	var msgs []model.SupportTicketMessage
	if err := db.Where("ticket_id = ?", t.Id).Order("id ASC").Find(&msgs).Error; err != nil {
		return nil, nil, err
	}
	return &t, msgs, nil
}

func (s *TicketService) CreateTicket(reseller *model.Reseller, subject, priority, message string) (*model.SupportTicket, error) {
	if reseller == nil || reseller.Id <= 0 {
		return nil, common.NewError("reseller is required")
	}
	subject = strings.TrimSpace(subject)
	message = strings.TrimSpace(message)
	if subject == "" {
		return nil, common.NewError("subject is required")
	}
	if message == "" {
		return nil, common.NewError("message is required")
	}
	db := database.GetDB()
	now := time.Now().UnixMilli()
	var created model.SupportTicket
	err := db.Transaction(func(tx *gorm.DB) error {
		created = model.SupportTicket{
			ResellerId:       reseller.Id,
			ResellerUsername: reseller.Username,
			Subject:          subject,
			Status:           "open",
			Priority:         normalizeTicketPriority(priority),
			CreatedAt:        now,
			UpdatedAt:        now,
		}
		if err := tx.Create(&created).Error; err != nil {
			return err
		}
		msg := model.SupportTicketMessage{
			TicketId:   created.Id,
			SenderRole: "reseller",
			SenderName: reseller.Username,
			Message:    message,
			CreatedAt:  now,
		}
		return tx.Create(&msg).Error
	})
	if err != nil {
		return nil, err
	}
	return &created, nil
}

func (s *TicketService) ReplyTicket(ticketId int, senderRole, senderName, message string, resellerId int) (*model.SupportTicketMessage, error) {
	if ticketId <= 0 {
		return nil, common.NewError("invalid ticket id")
	}
	message = strings.TrimSpace(message)
	if message == "" {
		return nil, common.NewError("message is required")
	}
	db := database.GetDB()
	var t model.SupportTicket
	if err := db.First(&t, ticketId).Error; err != nil {
		return nil, common.NewError("ticket not found")
	}
	if resellerId > 0 && t.ResellerId != resellerId {
		return nil, common.NewError("ticket not found")
	}
	now := time.Now().UnixMilli()
	nextStatus := "open"
	if senderRole == "admin" {
		nextStatus = "answered"
	}
	var msg model.SupportTicketMessage
	err := db.Transaction(func(tx *gorm.DB) error {
		msg = model.SupportTicketMessage{
			TicketId:   t.Id,
			SenderRole: senderRole,
			SenderName: senderName,
			Message:    message,
			CreatedAt:  now,
		}
		if err := tx.Create(&msg).Error; err != nil {
			return err
		}
		return tx.Model(&model.SupportTicket{}).
			Where("id = ?", t.Id).
			Updates(map[string]any{
				"status":     nextStatus,
				"updated_at": now,
			}).Error
	})
	if err != nil {
		return nil, err
	}
	return &msg, nil
}

func (s *TicketService) CloseTicket(ticketId int, resellerId int) error {
	if ticketId <= 0 {
		return common.NewError("invalid ticket id")
	}
	db := database.GetDB()
	var t model.SupportTicket
	if err := db.First(&t, ticketId).Error; err != nil {
		return common.NewError("ticket not found")
	}
	if resellerId > 0 && t.ResellerId != resellerId {
		return common.NewError("ticket not found")
	}
	return db.Model(&model.SupportTicket{}).
		Where("id = ?", t.Id).
		Updates(map[string]any{
			"status":     "closed",
			"updated_at": time.Now().UnixMilli(),
		}).Error
}

// ---------------------------------------------------------------------------
// Payment / Order Webhook Service
// ---------------------------------------------------------------------------

type PaymentWebhookPayload struct {
	OrderId      string  `json:"orderId"`
	Gateway      string  `json:"gateway"`
	TargetType   string  `json:"targetType"` // reseller_deposit | reseller_quota | client_renew
	TargetRef    string  `json:"targetRef"`  // reseller username or client email
	Amount       float64 `json:"amount"`
	AddGB        int64   `json:"addGb"`
	AddDays      int     `json:"addDays"`
	ResetTraffic bool    `json:"resetTraffic"`
}

type PaymentWebhookService struct {
	resellerService ResellerService
	clientService   ClientService
	inboundService  InboundService
}

func VerifyPaymentWebhookHMAC(rawBody []byte, signatureHex string, secret string) bool {
	secret = strings.TrimSpace(secret)
	signatureHex = strings.TrimSpace(strings.TrimPrefix(signatureHex, "sha256="))
	if secret == "" || signatureHex == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(rawBody)
	expected := mac.Sum(nil)
	got, err := hex.DecodeString(signatureHex)
	if err != nil {
		return false
	}
	return hmac.Equal(expected, got)
}

func (s *PaymentWebhookService) ListOrders(limit int) ([]model.PaymentOrder, error) {
	if limit <= 0 || limit > 500 {
		limit = 200
	}
	db := database.GetDB()
	var rows []model.PaymentOrder
	if err := db.Order("id DESC").Limit(limit).Find(&rows).Error; err != nil {
		return nil, err
	}
	return rows, nil
}

func (s *PaymentWebhookService) ProcessWebhook(p PaymentWebhookPayload) (*model.PaymentOrder, error) {
	p.OrderId = strings.TrimSpace(p.OrderId)
	p.Gateway = strings.TrimSpace(p.Gateway)
	p.TargetType = strings.ToLower(strings.TrimSpace(p.TargetType))
	p.TargetRef = strings.TrimSpace(p.TargetRef)

	if p.OrderId == "" {
		return nil, common.NewError("orderId is required")
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		return nil, err
	}
	defer unlock()

	db := database.GetDB()
	// Reseller payments commit the order, balance/ledger, and quota together.
	// The persisted order is authoritative even if a callback supplies another type.
	var probe model.PaymentOrder
	probeErr := db.Where("order_id = ?", p.OrderId).First(&probe).Error
	if probeErr != nil && !errors.Is(probeErr, gorm.ErrRecordNotFound) {
		return nil, probeErr
	}
	if (probeErr == nil && (probe.TargetType == "reseller_deposit" || probe.TargetType == "reseller_quota")) ||
		(errors.Is(probeErr, gorm.ErrRecordNotFound) && (p.TargetType == "reseller_deposit" || p.TargetType == "reseller_quota")) {
		return s.processResellerPayment(p)
	}
	if (probeErr == nil && probe.TargetType == "client_renew") ||
		(errors.Is(probeErr, gorm.ErrRecordNotFound) && p.TargetType == "client_renew") {
		return s.processClientRenewalPayment(p)
	}
	return nil, common.NewError("unsupported or missing payment order target")
}
