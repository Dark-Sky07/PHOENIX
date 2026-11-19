package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Dark-Sky07/phoenix/internal/database"
	"github.com/Dark-Sky07/phoenix/internal/database/model"
	"github.com/Dark-Sky07/phoenix/internal/web/service"
	"github.com/gin-contrib/sessions"
	"github.com/gin-contrib/sessions/cookie"
	"github.com/gin-gonic/gin"
)

func waitForQuotaTestFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
	return fmt.Errorf("timed out waiting for %s", path)
}

func TestResellerMultiprocessQuotaHelper(t *testing.T) {
	dir := os.Getenv("PHOENIX_QUOTA_CHILD_DIR")
	if dir == "" {
		t.Skip("subprocess helper")
	}
	index := os.Getenv("PHOENIX_QUOTA_CHILD_INDEX")
	if err := database.InitDB(filepath.Join(dir, "x-ui.db")); err != nil {
		t.Fatal(err)
	}
	defer database.CloseDB()
	var reseller model.Reseller
	if err := database.GetDB().Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	inboundID, err := strconv.Atoi(os.Getenv("PHOENIX_QUOTA_INBOUND_ID"))
	if err != nil {
		t.Fatal(err)
	}
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(sessions.Sessions("phoenix", cookie.NewStore([]byte("multi-process-quota-test"))))
	router.Use(func(c *gin.Context) {
		session := sessions.Default(c)
		session.Set("LOGIN_RESELLER", reseller.Id)
		session.Set("LOGIN_RESELLER_EPOCH", reseller.LoginEpoch)
		c.Next()
	})
	NewClientController(router.Group("/panel/api/clients"))
	if err := os.WriteFile(filepath.Join(dir, "ready-"+index), []byte("ready"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := waitForQuotaTestFile(filepath.Join(dir, "start"), 30*time.Second); err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"client": map[string]any{"email": "process-" + index + "@example.test", "totalGB": 60, "enable": true}, "inboundIds": []int{inboundID}})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/panel/api/clients/add", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if err := os.WriteFile(filepath.Join(dir, "result-"+index), w.Body.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}

// Two real HTTP create paths in distinct processes share one SQLite database.
// Each requested 60 bytes fits alone, but the 100-byte limit permits only one.
func TestConcurrentResellerCreationAcrossSQLiteProcesses(t *testing.T) {
	setupResellerInboundRouter(t)
	db := database.GetDB()
	var reseller model.Reseller
	if err := db.Where("username = ?", "sara").First(&reseller).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&reseller).Update("traffic_limit", 100).Error; err != nil {
		t.Fatal(err)
	}
	reseller.TrafficLimit = 100
	inbound := model.Inbound{UserId: 1, Port: 19371, Protocol: model.VLESS, Settings: `{"clients":[]}`, Tag: "multiprocess-quota"}
	if err := db.Create(&inbound).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&model.ResellerInbound{ResellerId: reseller.Id, InboundId: inbound.Id}).Error; err != nil {
		t.Fatal(err)
	}
	dir := os.Getenv("XUI_DB_FOLDER")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmds := make([]*exec.Cmd, 0, 2)
	outputs := make([]*bytes.Buffer, 0, 2)
	for i := 0; i < 2; i++ {
		index := strconv.Itoa(i)
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestResellerMultiprocessQuotaHelper$", "-test.v")
		cmd.Env = append(os.Environ(), "PHOENIX_QUOTA_CHILD_DIR="+dir, "PHOENIX_QUOTA_CHILD_INDEX="+index, "PHOENIX_QUOTA_INBOUND_ID="+strconv.Itoa(inbound.Id))
		output := &bytes.Buffer{}
		cmd.Stdout, cmd.Stderr = output, output
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		cmds, outputs = append(cmds, cmd), append(outputs, output)
		t.Cleanup(func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
		})
		// Initialize databases sequentially; only the actual writes race.
		if err := waitForQuotaTestFile(filepath.Join(dir, "ready-"+index), 30*time.Second); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "start"), []byte("start"), 0o600); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range cmds {
		if err := cmd.Wait(); err != nil {
			t.Fatalf("child %d: %v\n%s", i, err, outputs[i].String())
		}
	}
	var successes int
	for i := 0; i < 2; i++ {
		data, err := os.ReadFile(filepath.Join(dir, "result-"+strconv.Itoa(i)))
		if err != nil {
			t.Fatal(err)
		}
		var response resellerWriteContract
		if err := json.Unmarshal(data, &response); err != nil {
			t.Fatalf("response %s: %v", data, err)
		}
		if response.Success {
			successes++
		} else if !strings.Contains(response.Msg, "traffic quota") {
			t.Fatalf("unexpected failure: %s", data)
		}
	}
	if successes != 1 {
		t.Fatalf("expected one successful request, got %d", successes)
	}
	var mappings, sales, clients int64
	if err := db.Model(&model.ResellerClient{}).Where("reseller_id = ?", reseller.Id).Count(&mappings).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ResellerSale{}).Where("reseller_id = ?", reseller.Id).Count(&sales).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Model(&model.ClientRecord{}).Where("email LIKE ?", "process-%@example.test").Count(&clients).Error; err != nil {
		t.Fatal(err)
	}
	stat, err := (&service.ResellerService{}).Stat(&reseller)
	if err != nil || clients != 1 || mappings != 1 || sales != 1 || stat.AllocatedTraffic != 60 || stat.RemainingTraffic != 40 {
		t.Fatalf("inconsistent result clients=%d mappings=%d sales=%d stat=%+v err=%v", clients, mappings, sales, stat, err)
	}
}
