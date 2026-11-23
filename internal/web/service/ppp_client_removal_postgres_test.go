package service

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/google/uuid"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

func TestPPPRemovalGuardPostgres(t *testing.T) {
	dsn := os.Getenv("PHOENIX_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("PHOENIX_TEST_POSTGRES_DSN not set")
	}
	t.Setenv("XUI_BIN_FOLDER", t.TempDir())
	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.AutoMigrate(&model.Inbound{}, &model.ClientRecord{}, &model.ClientInbound{}); err != nil {
		t.Fatal(err)
	}
	tx := db.Begin()
	if tx.Error != nil {
		t.Fatal(tx.Error)
	}
	defer tx.Rollback()
	id := uuid.NewString()
	email := id + "@ppp-guard.test"
	payload, err := json.Marshal(map[string]any{"clients": []any{map[string]any{"email": email}}})
	if err != nil {
		t.Fatal(err)
	}
	ib := model.Inbound{Tag: id, Protocol: model.PPTP, Settings: string(payload)}
	client := model.ClientRecord{Email: email, UUID: id, Enable: true}
	for _, row := range []any{&ib, &client} {
		if err := tx.Create(row).Error; err != nil {
			t.Fatal(err)
		}
	}
	link := model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}
	if err := tx.Create(&link).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(tx, []string{email}, nil); err == nil {
		t.Fatal("linked PPP removal accepted")
	}
	if err := tx.Model(&ib).Update("protocol", model.L2TP).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(tx, []string{email}, nil); err == nil {
		t.Fatal("linked L2TP removal accepted")
	}
	// The only evidence is now the link, whose email has whitespace SQL
	// TRIM does not remove. The guard must still match the Go-normalized key.
	if err := tx.Model(&ib).Update("settings", `{"clients":[]}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Model(&client).Update("email", "\t"+email+"\n").Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(tx, []string{email}, nil); err == nil {
		t.Fatal("Go-normalized link accepted by PostgreSQL guard")
	}
	if err := tx.Model(&ib).Update("settings", string(payload)).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Where("client_id = ?", client.Id).Delete(&model.ClientInbound{}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(tx, []string{email}, nil); err == nil {
		t.Fatal("settings drift accepted")
	}
	if err := rejectUnprovenPPPRemoval(tx, []string{id + "-unrelated@test"}, nil); err != nil {
		t.Fatalf("unrelated identity blocked: %v", err)
	}
	if err := tx.Model(&ib).Update("settings", `{"clients":[]}`).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Delete(&client).Error; err != nil {
		t.Fatal(err)
	}
	if err := tx.Create(&model.ClientInbound{ClientId: client.Id, InboundId: ib.Id}).Error; err != nil {
		t.Fatal(err)
	}
	if err := rejectUnprovenPPPRemoval(tx, []string{id + "-unrelated@test"}, []int{ib.Id}); err == nil {
		t.Fatal("orphan link allowed removal with unknown identity")
	}
}
