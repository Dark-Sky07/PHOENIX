package service

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/web/trafficprotocol"
)

// PrepareVerifiedNodeGlobalTraffic is an opt-in sender building block, not yet
// called by the node sync job. Sender state must already be registered under
// node:<nodeID>, with a stable sender GUID and generation. Snapshot capture and
// sequence reservation share the participating allocation lock, so a later
// participating capture cannot reserve a newer sequence for an older view.
// The lock does not fence a lost DB session or non-participating writers.
// A nil envelope means the node has no rows to send.
func (s *InboundService) PrepareVerifiedNodeGlobalTraffic(nodeID int, masterGUID, apiToken string) (*trafficprotocol.Envelope, error) {
	if nodeID <= 0 || masterGUID == "" || masterGUID != strings.TrimSpace(masterGUID) {
		return nil, errors.New("invalid verified global traffic sender")
	}
	key, err := trafficprotocol.KeyFromAPIToken(apiToken)
	if err != nil {
		return nil, err
	}
	unlock, err := LockResellerAllocation()
	if err != nil {
		return nil, err
	}
	defer unlock()
	rows, err := s.GetNodeClientTraffics(nodeID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, nil
	}
	snapshot, err := json.Marshal(rows)
	if err != nil {
		return nil, err
	}
	if len(snapshot) > trafficprotocol.MaxSnapshotBytes {
		return nil, errors.New("global traffic snapshot exceeds size limit")
	}
	envelope, err := trafficprotocol.ReserveSequence(database.GetDB(), fmt.Sprintf("node:%d", nodeID))
	if err != nil {
		return nil, fmt.Errorf("reserve global traffic sequence: %w", err)
	}
	if envelope.Sender != masterGUID {
		return nil, errors.New("registered global traffic sender does not match panel GUID")
	}
	envelope.Snapshot = snapshot
	envelope, err = trafficprotocol.Sign(envelope, key)
	if err != nil {
		return nil, err
	}
	return &envelope, nil
}
