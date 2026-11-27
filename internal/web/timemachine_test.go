package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/tommilevs/keenetic-smb-ui/internal/config"
)

func TestTimeMachineAPI(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.ConfigFile = filepath.Join(t.TempDir(), "config.json")
	cfg.TimeMachine = &config.TimeMachine{Share: "existing", UUID: "123abc", User: "tm_mac"}
	cfg.Remotes = []config.Remote{{ID: "nas", Password: "legacy CIFS password"}}
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer s.cancel()
	request := func(method, path string, auth bool, body []byte) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		if auth {
			req.SetBasicAuth(cfg.Web.User, cfg.Web.Password)
		}
		s.http.Handler.ServeHTTP(rec, req)
		return rec
	}
	if got := request("GET", "/api/time-machine/diagnostics", false, nil).Code; got != http.StatusUnauthorized {
		t.Fatalf("unauthorized diagnostics returned %d", got)
	}
	if got := request("GET", "/api/time-machine/apply", true, nil).Code; got != http.StatusMethodNotAllowed {
		t.Fatalf("applying with GET returned %d", got)
	}
	cfg.Web.ReadOnly = true
	if got := request("POST", "/api/time-machine/apply", true, nil).Code; got != http.StatusForbidden {
		t.Fatalf("applying in read-only mode returned %d", got)
	}
	cfg.Web.ReadOnly = false
	rec := request("GET", "/api/config", true, nil)
	if rec.Code != http.StatusOK || bytes.Contains(rec.Body.Bytes(), []byte("legacy CIFS password")) {
		t.Fatalf("config response was not redacted: %d", rec.Code)
	}
	var incoming config.Config
	if err := json.Unmarshal(rec.Body.Bytes(), &incoming); err != nil {
		t.Fatal(err)
	}
	incoming.TimeMachine = nil
	incoming.SMB.ServerString = "saved by existing panel"
	data, err := json.Marshal(incoming)
	if err != nil {
		t.Fatal(err)
	}
	if got := request("POST", "/api/config", true, data).Code; got != http.StatusOK {
		t.Fatalf("saving ordinary configuration returned %d", got)
	}
	saved, err := config.Load(cfg.Paths.ConfigFile)
	if err != nil {
		t.Fatal(err)
	}
	if saved.TimeMachine == nil || saved.TimeMachine.Share != "existing" ||
		saved.SMB.Users[0].Password != "1235467890" || saved.Remotes[0].Password != "legacy CIFS password" {
		t.Fatal("saving other settings lost existing Time Machine or SMB/CIFS credentials")
	}
	rec = request("POST", "/api/time-machine/check", true, []byte(`{"password":"do-not-echo","typo":1}`))
	if rec.Code != http.StatusBadRequest || bytes.Contains(rec.Body.Bytes(), []byte("do-not-echo")) {
		t.Fatal("invalid input handling exposed credential material")
	}
}
