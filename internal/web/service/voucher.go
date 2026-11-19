package service

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/random"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// VoucherService manages Reseller Wallet Gift/Voucher codes.
type VoucherService struct {
	resellerService ResellerService
}

func (s *VoucherService) List() ([]model.ResellerVoucher, error) {
	db := database.GetDB()
	var rows []model.ResellerVoucher
	err := db.Order("id DESC").Find(&rows).Error
	for i := range rows {
		if rows[i].UsedByIds == nil {
			rows[i].UsedByIds = []int{}
		}
	}
	return rows, err
}

func (s *VoucherService) Create(code string, amount float64, trafficGB int64, maxUses int, expiresAt int64, note string) (*model.ResellerVoucher, error) {
	if math.IsNaN(amount) || math.IsInf(amount, 0) || trafficGB > math.MaxInt64/bytesPerGB {
		return nil, errors.New("voucher amount or traffic exceeds supported range")
	}
	if amount <= 0 && trafficGB <= 0 {
		return nil, errors.New("voucher must grant a positive wallet amount or traffic quota (GB)")
	}
	code = strings.ToUpper(strings.TrimSpace(code))
	if code == "" {
		code = fmt.Sprintf("PHOENIX-%s-%s", strings.ToUpper(random.NumLower(4)), strings.ToUpper(random.NumLower(4)))
	}
	if maxUses <= 0 {
		maxUses = 1
	}
	v := &model.ResellerVoucher{
		Code:      code,
		Amount:    Round2(amount),
		TrafficGB: max(trafficGB, 0),
		MaxUses:   maxUses,
		UsedCount: 0,
		UsedByIds: []int{},
		ExpiresAt: max(expiresAt, 0),
		Note:      strings.TrimSpace(note),
		CreatedAt: time.Now().UnixMilli(),
	}
	db := database.GetDB()
	if err := db.Create(v).Error; err != nil {
		return nil, err
	}
	return v, nil
}

func (s *VoucherService) Delete(id int) error {
	if id <= 0 {
		return errors.New("invalid voucher id")
	}
	return database.GetDB().Delete(&model.ResellerVoucher{}, id).Error
}

func (s *VoucherService) Redeem(resellerId int, rawCode string) (*model.Reseller, *model.ResellerVoucher, error) {
	code := strings.ToUpper(strings.TrimSpace(rawCode))
	if code == "" {
		return nil, nil, errors.New("voucher code is required")
	}
	db := database.GetDB()
	var redeemedVoucher model.ResellerVoucher
	now := time.Now().UnixMilli()

	err := db.Transaction(func(tx *gorm.DB) error {
		var v model.ResellerVoucher
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).Where("UPPER(code) = ?", code).First(&v).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return errors.New("invalid voucher code")
			}
			return err
		}
		if v.ExpiresAt > 0 && now > v.ExpiresAt {
			return errors.New("this voucher code has expired")
		}
		if v.MaxUses > 0 && v.UsedCount >= v.MaxUses {
			return errors.New("this voucher code has already been fully redeemed")
		}
		if slices.Contains(v.UsedByIds, resellerId) {
			return errors.New("you have already redeemed this voucher code")
		}

		var r model.Reseller
		if err := tx.Clauses(clause.Locking{Strength: "UPDATE"}).First(&r, resellerId).Error; err != nil {
			return err
		}

		if math.IsNaN(v.Amount) || math.IsInf(v.Amount, 0) || math.IsNaN(r.Deposit) || math.IsInf(r.Deposit, 0) || v.TrafficGB < 0 || v.TrafficGB > math.MaxInt64/bytesPerGB {
			return errors.New("voucher or reseller has invalid accounting values")
		}
		updates := map[string]any{
			"updated_at": now,
		}
		newBalance := r.Deposit
		if v.Amount > 0 {
			newBalance = Round2(r.Deposit + v.Amount)
			if math.IsInf(newBalance, 0) || math.IsNaN(newBalance) {
				return errors.New("voucher balance exceeds supported range")
			}
			updates["deposit"] = newBalance
		}
		if v.TrafficGB > 0 && r.TrafficLimit <= 0 {
			return errors.New("cannot redeem traffic grant for an unlimited or invalid reseller quota")
		}
		if v.TrafficGB > 0 {
			addition := v.TrafficGB * bytesPerGB
			if r.TrafficLimit > math.MaxInt64-addition {
				return errors.New("voucher traffic quota exceeds supported range")
			}
			updates["traffic_limit"] = r.TrafficLimit + addition
		}
		result := tx.Model(&model.Reseller{}).Where("id = ? AND deposit = ? AND traffic_limit = ?", resellerId, r.Deposit, r.TrafficLimit).Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return errors.New("reseller account changed concurrently; retry voucher redemption")
		}

		if v.Amount > 0 {
			if err := tx.Create(&model.ResellerTransaction{
				ResellerId: resellerId,
				Type:       "deposit",
				Amount:     Round2(v.Amount),
				Balance:    newBalance,
				Comment:    "Voucher: " + v.Code,
				CreatedAt:  now,
			}).Error; err != nil {
				return err
			}
		}

		v.UsedCount++
		v.UsedByIds = append(v.UsedByIds, resellerId)
		if err := tx.Save(&v).Error; err != nil {
			return err
		}
		redeemedVoucher = v
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	reseller, err := s.resellerService.Get(resellerId)
	return reseller, &redeemedVoucher, err
}
