package service

import (
	"fmt"

	"github.com/Dark-Sky07/phoenix/internal/openvpn"
)

// rejectUnverifiedOpenVPNReset blocks quota resets and capacity release while
// a client's durable session evidence is incomplete. A completed hook record
// is the only proof of final usage for a session missed by status polls; an
// incident or an unclosed admission record is not a byte measurement, so the
// conservative action is to refuse to clear counters rather than guess.
func rejectUnverifiedOpenVPNReset(emails []string) error {
	if err := openvpn.CheckClientSessionEvidence(emails); err != nil {
		return fmt.Errorf("OpenVPN reset requires reconciliation: %w", err)
	}
	return nil
}
