package controller

import (
	"testing"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
)

// seedSecurityTestAdmin makes authentication-dependent tests explicit about
// their principal. Fresh production databases no longer receive admin/admin.
func seedSecurityTestAdmin(t *testing.T) model.User {
	t.Helper()
	user := model.User{Username: "test-superadmin", Password: "test-hash", Role: "superadmin"}
	if err := database.GetDB().Create(&user).Error; err != nil {
		t.Fatalf("seed test admin: %v", err)
	}
	return user
}
