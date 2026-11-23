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
