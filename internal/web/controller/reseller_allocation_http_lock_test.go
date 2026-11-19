package controller

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/gin-gonic/gin"
)

func TestAllocationHTTPMutationRevalidatesRevokedReseller(t *testing.T) {
	router := setupResellerInboundRouter(t)
	called := make(chan struct{}, 1)
	router.POST("/locked-mutation", withResellerAllocationLock(func(c *gin.Context) {
		called <- struct{}{}
		c.Status(http.StatusNoContent)
	}))
	unlock, err := service.LockResellerAllocation()
	if err != nil {
		t.Fatal(err)
	}
	locked := true
	defer func() {
		if locked {
			unlock()
		}
	}()
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/locked-mutation", nil))
		finished <- w
	}()
	select {
	case <-finished:
		t.Fatal("mutation ran while allocation lock was held")
	case <-time.After(150 * time.Millisecond):
	}
	if err := database.GetDB().Model(&model.Reseller{}).Where("username = ?", "sara").Update("login_epoch", 2).Error; err != nil {
		t.Fatal(err)
	}
	unlock()
	locked = false
	select {
	case w := <-finished:
		if w.Code != http.StatusForbidden {
			t.Fatalf("revoked request got status %d: %s", w.Code, w.Body.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("request did not finish after unlocking")
	}
	select {
	case <-called:
		t.Fatal("revoked request reached mutation handler")
	default:
	}
}
