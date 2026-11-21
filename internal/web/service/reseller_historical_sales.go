package service

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

// RecordHistoricalSale appends an administrator-entered historical record.
// The unique reference prevents accidentally importing the same source twice;
// it does not establish that the claimed sale or amount actually occurred.
func (s *ResellerService) RecordHistoricalSale(row *model.ResellerHistoricalSale) error {
	if row == nil || row.ResellerId <= 0 {
		return errors.New("reseller is required")
	}
	row.Reference = strings.TrimSpace(row.Reference)
	row.Source = strings.TrimSpace(row.Source)
	row.Email = strings.TrimSpace(row.Email)
	row.ActorName = strings.TrimSpace(row.ActorName)
	for _, field := range []struct {
		value string
		max   int
	}{
		{row.Reference, 128}, {row.Source, 512}, {row.Email, 254}, {row.ActorName, 128},
	} {
		if field.value == "" || utf8.RuneCountInString(field.value) > field.max || strings.IndexFunc(field.value, unicode.IsControl) >= 0 {
			return errors.New("reference, source, email, and actor are required and must be valid")
		}
	}
	if row.ActorType != "admin" && row.ActorType != "api_token" {
		return errors.New("administrator or API token required")
	}
	if row.SoldAt <= 0 || row.SoldAt > time.Now().UnixMilli() || row.TrafficBytes < 0 {
		return errors.New("invalid historical sale date or traffic")
	}
	if row.AmountMinor != nil {
		price := &ResellerSalePrice{AmountMinor: *row.AmountMinor, Currency: row.Currency}
		if err := ValidateResellerSalePrice(price); err != nil {
			return err
		}
		row.Currency = price.Currency
	} else if strings.TrimSpace(row.Currency) != "" {
		return errors.New("currency requires a declared amount")
	}
	if _, err := s.Get(row.ResellerId); err != nil {
		return err
	}
	row.Id = 0
	row.RecordedAt = time.Now().UnixMilli()
	return database.GetDB().Create(row).Error
}

func (s *ResellerService) HistoricalSales(resellerId int) ([]model.ResellerHistoricalSale, error) {
	if _, err := s.Get(resellerId); err != nil {
		return nil, err
	}
	var rows []model.ResellerHistoricalSale
	err := database.GetDB().Where("reseller_id = ?", resellerId).Order("recorded_at DESC, id DESC").Limit(500).Find(&rows).Error
	return rows, err
}
