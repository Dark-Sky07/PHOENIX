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

// processResellerPayment commits the idempotency record and all reseller
// accounting effects together. PostgreSQL locks the order and reseller rows;
// SQLite conflicting writers fail rather than duplicating the credit.
func (s *PaymentWebhookService) processResellerPayment(p PaymentWebhookPayload) (*model.PaymentOrder, error) {
	if math.IsNaN(p.Amount) || math.IsInf(p.Amount, 0) || p.Amount < 0 {
		return nil, errors.New("invalid payment amount")
	}
	var settings SettingService
	currency, currencyErr := settings.GetCheckoutCurrency()
	code, validCurrency := moneycurrency.Normalize(currency)
	validCurrency = validCurrency && currencyErr == nil
	if validCurrency {
		currency = code
	} else {
		currency = "USD"
	}
	tolerance := 0.5 / math.Pow10(moneycurrency.FractionDigits(currency))
	var result model.PaymentOrder
	err := database.GetDB().Transaction(func(tx *gorm.DB) error {
		candidate := model.PaymentOrder{OrderId: p.OrderId, Gateway: p.Gateway, TargetType: p.TargetType, TargetRef: p.TargetRef, Amount: p.Amount, AddGB: p.AddGB, AddDays: p.AddDays, ResetTraffic: p.ResetTraffic, Status: "pending"}
		if candidate.Gateway == "" {
			candidate.Gateway = "webhook"
		}
		// Create only when the webhook supplies a complete new order. A
		// callback for an unknown checkout ID cannot invent its target.
		if strings.TrimSpace(candidate.TargetRef) != "" {
			if err := tx.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "order_id"}}, DoNothing: true}).Create(&candidate).Error; err != nil {
				return err
			}
		}
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("order_id = ?", p.OrderId).First(&result).Error; err != nil {
			return err
		}
		if result.TargetType != "reseller_deposit" && result.TargetType != "reseller_quota" {
			return errors.New("order target changed; retry callback")
		}
		if result.Status == "completed" {
			return nil
		}
		if result.Status != "pending" {
			return errors.New("order is not pending")
		}
		if result.Currency != "" {
			orderCurrency, ok := moneycurrency.Normalize(result.Currency)
			if !ok {
				return errors.New("invalid order currency; administrator reconciliation required")
			}
			if result.TargetType == "reseller_deposit" && (!validCurrency || orderCurrency != currency) {
				return errors.New("deposit currency changed; administrator reconciliation required")
			}
			tolerance = 0.5 / math.Pow10(moneycurrency.FractionDigits(orderCurrency))
		}
		if result.Amount > 0 && (p.Amount <= 0 || math.Abs(p.Amount-result.Amount) > tolerance) {
			return errors.New("webhook amount does not match pending order")
		}
		if math.IsNaN(result.Amount) || math.IsInf(result.Amount, 0) || result.Amount < 0 || result.AddGB < 0 || result.AddGB > math.MaxInt64/bytesPerGB || result.AddDays < 0 {
			return errors.New("invalid payment accounting values")
		}
		var reseller model.Reseller
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("username = ?", result.TargetRef).First(&reseller).Error; err != nil {
			return err
		}
		now := time.Now().UnixMilli()
		updates := map[string]any{"updated_at": now}
		if result.AddGB > 0 {
			// Zero denotes unlimited, not an empty bucket. Never silently
			// turn unlimited access into a smaller finite quota or consume a
			// paid grant that cannot be represented.
			if reseller.TrafficLimit == 0 {
				return errors.New("cannot apply traffic grant to an unlimited reseller; administrator reconciliation required")
			}
			addition := result.AddGB * bytesPerGB
			if reseller.TrafficLimit < 0 || reseller.TrafficLimit > math.MaxInt64-addition {
				return errors.New("traffic quota exceeds supported range")
			}
			updates["traffic_limit"] = reseller.TrafficLimit + addition
		}
		if result.TargetType == "reseller_quota" && result.AddDays > 0 {
			base := max(now, reseller.ExpiryTime)
			if int64(result.AddDays) > (math.MaxInt64-base)/86400000 {
				return errors.New("expiry exceeds supported range")
			}
			updates["expiry_time"] = base + int64(result.AddDays)*86400000
		}
		if result.TargetType == "reseller_deposit" {
			if result.Amount <= 0 || math.IsNaN(reseller.Deposit) || math.IsInf(reseller.Deposit, 0) {
				return errors.New("invalid reseller deposit")
			}
			amount := RoundMoney(result.Amount, currency)
			balance := RoundMoney(reseller.Deposit+amount, currency)
			if amount <= 0 || math.IsInf(balance, 0) || math.IsNaN(balance) {
				return errors.New("deposit exceeds supported range")
			}
			updates["deposit"] = balance
			if err := tx.Create(&model.ResellerTransaction{ResellerId: reseller.Id, Type: "deposit", Amount: amount, Balance: balance, Comment: fmt.Sprintf("Webhook (%s) #%s", result.Gateway, result.OrderId), CreatedAt: now}).Error; err != nil {
				return err
			}
		}
		update := tx.Model(&model.Reseller{}).Where("id = ? AND deposit = ? AND traffic_limit = ? AND expiry_time = ?", reseller.Id, reseller.Deposit, reseller.TrafficLimit, reseller.ExpiryTime).Updates(updates)
		if update.Error != nil {
			return update.Error
		}
		if update.RowsAffected != 1 {
			return errors.New("reseller account changed concurrently; retry payment")
		}
		if err := tx.Model(&model.PaymentOrder{}).Where("id = ?", result.Id).Update("status", "completed").Error; err != nil {
			return err
		}
		result.Status = "completed"
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}
