package controller

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResellerCannotResetOrRewriteTrafficCounters(t *testing.T) {
	r := setupResellerInboundRouter(t)
	NewClientController(r.Group("/panel/api/clients"))
	for _, path := range []string{
		"/resetAllTraffics", "/bulkResetTraffic", "/resetTraffic/example", "/updateTraffic/example",
	} {
		w := httptest.NewRecorder()
		r.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/panel/api/clients"+path, nil))
		var body resellerWriteContract
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatalf("%s: invalid response: %v", path, err)
		}
		if w.Code != http.StatusForbidden || body.Success || body.Msg != errResellerTrafficResetUnavailable {
			t.Errorf("%s: unexpected response %d %+v", path, w.Code, body)
		}
	}
}
