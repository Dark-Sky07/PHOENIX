package service

import (
	"strings"

	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/util/common"
)

func validatePPPVPNClientCredentials(protocol model.Protocol, clients []model.Client) error {
	for _, client := range clients {
		if strings.TrimSpace(client.Email) == "" || client.Password == "" ||
			strings.ContainsAny(client.Email, "\r\n*\t") || strings.ContainsAny(client.Password, "\r\n") {
			return common.NewErrorf("%s clients require a valid email and password", protocol)
		}
	}
	return nil
}
