package service

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	moneycurrency "github.com/Dark-Sky07/phoenix/internal/util/currency"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// Interrupted renewals must not be obscured by later changes. Callers hold
// the allocation lock; processing remains ambiguous until an operator checks
// the payment and its SQL/runtime effects.
func checkProcessingClientRenewal(tx *gorm.DB, email string) error {
	var count int64
	if err := tx.Model(&model.PaymentOrder{}).Where("target_type = ? AND status = ? AND LOWER(TRIM(target_ref)) = ?", "client_renew", "processing", transferEmailKey(email)).Count(&count).Error; err != nil {
		return err
	}
	if count > 0 {
		return errors.New("client renewal requires manual reconciliation")
	}
	return nil
}

func processingClientRenewalsQuery(tx *gorm.DB, resellerID int) *gorm.DB {
	// EXISTS avoids duplicate counts if legacy ownership mappings differ only
	// by case or whitespace.
	return tx.Model(&model.PaymentOrder{}).
		Where("payment_orders.target_type = ? AND payment_orders.status = ?", "client_renew", "processing").
		Where("EXISTS (SELECT 1 FROM reseller_clients rc WHERE rc.reseller_id = ? AND LOWER(TRIM(rc.email)) = LOWER(TRIM(payment_orders.target_ref)))", resellerID)
}

func countResellerProcessingClientRenewals(tx *gorm.DB, resellerID int) (int64, error) {
	var count int64
	err := processingClientRenewalsQuery(tx, resellerID).Count(&count).Error
	return count, err
}

func checkResellerProcessingClientRenewals(tx *gorm.DB, resellerID int) error {
	count, err := countResellerProcessingClientRenewals(tx, resellerID)
	if err != nil {
		return err
	}
	if count > 0 {
		return errors.New("reseller client renewal requires manual reconciliation; contact the administrator")
	}
	return nil
}

// ProcessingRenewalIssue is deliberately restricted to administrators. A
// processing order may still be running; its presence is not proof of failure.
type ProcessingRenewalIssue struct {
	OrderId   string `json:"orderId"`
	TargetRef string `json:"targetRef"`
	CreatedAt int64  `json:"createdAt"`
}

func (s *ResellerService) ProcessingRenewalIssues(resellerID int) ([]ProcessingRenewalIssue, error) {
	if resellerID <= 0 {
		return nil, errors.New("invalid reseller id")
	}
	rows := make([]ProcessingRenewalIssue, 0)
	err := processingClientRenewalsQuery(database.GetDB(), resellerID).Select("payment_orders.order_id", "payment_orders.target_ref", "payment_orders.created_at").Order("payment_orders.id DESC").Limit(100).Scan(&rows).Error
	return rows, err
}

// CheckResellerProcessingClientRenewals is used before reseller mutations and
// allocation checks. Admin corrections require the documented manual process.
func CheckResellerProcessingClientRenewals(resellerID int) error {
	return checkResellerProcessingClientRenewals(database.GetDB(), resellerID)
}

// A client renewal touches both SQL and runtime services. Until those effects
// can be committed atomically, claim the order durably before touching either.
// A crash or ambiguous failure leaves "processing" and requires an operator
// to reconcile it; never automatically repeat a potentially applied renewal.
func (s *PaymentWebhookService) processClientRenewalPayment(p PaymentWebhookPayload) (*model.PaymentOrder, error) {
	if math.IsNaN(p.Amount) || math.IsInf(p.Amount, 0) {
		return nil, errors.New("invalid payment amount")
	}
	db := database.GetDB()
	if p.TargetRef != "" {
		gateway := p.Gateway
		if gateway == "" {
			gateway = "webhook"
		}
		candidate := model.PaymentOrder{OrderId: p.OrderId, Gateway: gateway, TargetType: p.TargetType, TargetRef: p.TargetRef, Amount: p.Amount, AddGB: p.AddGB, AddDays: p.AddDays, ResetTraffic: p.ResetTraffic, Status: "pending"}
		if err := db.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "order_id"}}, DoNothing: true}).Create(&candidate).Error; err != nil {
			return nil, err
		}
	}
	var order model.PaymentOrder
	if err := db.Where("order_id = ?", p.OrderId).First(&order).Error; err != nil {
		return nil, err
	}
	if order.TargetType != "client_renew" {
		return nil, errors.New("order target changed; retry callback")
	}
	if order.Status == "completed" {
		return &order, nil
	}
	if order.Status != "pending" {
		return nil, errors.New("client renewal is processing or requires manual reconciliation")
	}
	var settingSvc SettingService
	currency, _ := settingSvc.GetCheckoutCurrency()
	tolerance := 0.5 / math.Pow10(moneycurrency.FractionDigits(currency))
	if math.IsNaN(order.Amount) || math.IsInf(order.Amount, 0) || (p.Amount > 0 && order.Amount > 0 && math.Abs(p.Amount-order.Amount) > tolerance) {
		return nil, errors.New("webhook amount does not match pending order")
	}
	if err := checkProcessingClientRenewal(db, order.TargetRef); err != nil {
		return nil, err
	}
	updated, err := s.preparePaidClientRenewal(&order)
	if err != nil {
		return nil, err
	}
	claim := db.Model(&model.PaymentOrder{}).Where("id = ? AND status = ?", order.Id, "pending").Update("status", "processing")
	if claim.Error != nil {
		return nil, claim.Error
	}
	if claim.RowsAffected != 1 {
		return nil, errors.New("client renewal already claimed; manual reconciliation may be required")
	}
	if order.ResetTraffic {
		if err := s.inboundService.ResetClientTrafficByEmail(order.TargetRef); err != nil {
			return nil, fmt.Errorf("client renewal requires manual reconciliation: %w", err)
		}
	}
	needRestart, err := s.clientService.UpdateByEmail(&s.inboundService, order.TargetRef, *updated)
	if needRestart {
		(&XrayService{}).SetToNeedRestart()
	}
	if err != nil {
		return nil, fmt.Errorf("client renewal requires manual reconciliation: %w", err)
	}
	done := db.Model(&model.PaymentOrder{}).Where("id = ? AND status = ?", order.Id, "processing").Update("status", "completed")
	if done.Error != nil {
		return nil, fmt.Errorf("client renewal requires manual reconciliation: %w", done.Error)
	}
	if done.RowsAffected != 1 {
		return nil, errors.New("client renewal requires manual reconciliation: order status changed")
	}
	order.Status = "completed"
	return &order, nil
}

func (s *PaymentWebhookService) preparePaidClientRenewal(order *model.PaymentOrder) (*model.Client, error) {
	if strings.TrimSpace(order.TargetRef) == "" {
		return nil, errors.New("client target is required")
	}
	rec, err := s.clientService.GetRecordByEmail(nil, order.TargetRef)
	if err != nil {
		return nil, fmt.Errorf("client %q not found: %w", order.TargetRef, err)
	}
	updated := *rec.ToClient()
	// Zero denotes unlimited traffic; adding a finite number to zero would
	// silently replace unlimited access with a finite quota after payment.
	if updated.TotalGB == 0 && order.AddGB > 0 {
		return nil, errors.New("cannot add traffic to an unlimited client; administrator reconciliation required")
	}
	if order.AddGB < 0 || order.AddGB > math.MaxInt64/bytesPerGB || updated.TotalGB < 0 || (order.AddGB > 0 && updated.TotalGB > math.MaxInt64-order.AddGB*bytesPerGB) {
		return nil, errors.New("client renewal quota exceeds supported range")
	}
	updated.TotalGB += order.AddGB * bytesPerGB
	if err := validatePaidClientRenewal(rec, updated.TotalGB, order.ResetTraffic); err != nil {
		return nil, err
	}
	if order.AddDays < 0 || int64(order.AddDays) > math.MaxInt64/86400000 {
		return nil, errors.New("client renewal expiry exceeds supported range")
	}
	if order.AddDays > 0 {
		period := int64(order.AddDays) * 86400000
		if updated.ExpiryTime < 0 {
			if updated.ExpiryTime < math.MinInt64+period {
				return nil, errors.New("client renewal expiry exceeds supported range")
			}
			updated.ExpiryTime -= period
		} else {
			base := max(time.Now().UnixMilli(), updated.ExpiryTime)
			if base > math.MaxInt64-period {
				return nil, errors.New("client renewal expiry exceeds supported range")
			}
			updated.ExpiryTime = base + period
		}
	}
	updated.Enable = true
	return &updated, nil
}
