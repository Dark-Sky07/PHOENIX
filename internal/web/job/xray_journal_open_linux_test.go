//go:build linux

package job

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

func TestTuicJournalRejectsFIFO(t *testing.T) {
	setupIntegrationDB(t)
	if err := os.MkdirAll(tuicJournalDir(), 0700); err != nil {
		t.Fatal(err)
	}
	batch := tuicTrafficBatch{ID: "fifo"}
	path := filepath.Join(tuicJournalDir(), batch.ID+".json")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := storeTuicBatch(batch); err == nil {
		t.Fatal("FIFO TUIC journal accepted on retry")
	}
	if err := NewTuicJob().replayTuicJournal(); err == nil {
		t.Fatal("FIFO TUIC journal accepted on recovery")
	}
}

func TestXrayJournalRejectsFIFO(t *testing.T) {
	setupIntegrationDB(t)
	if err := os.MkdirAll(xrayJournalDir(), 0700); err != nil {
		t.Fatal(err)
	}
	batch := &xrayTrafficBatch{id: uuid.NewString(), identities: []service.XrayTrafficIdentity{}}
	path := filepath.Join(xrayJournalDir(), batch.id+".json")
	if err := unix.Mkfifo(path, 0600); err != nil {
		t.Fatal(err)
	}
	if err := storeXrayBatch(batch); err == nil {
		t.Fatal("FIFO journal accepted on retry")
	}
	if err := NewXrayTrafficJob().replayXrayJournal(); err == nil {
		t.Fatal("FIFO journal accepted on recovery")
	}
}
