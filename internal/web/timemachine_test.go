package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
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

func TestTimeMachineUsesExistingSMBAccount(t *testing.T) {
	cfg := config.Default()
	cfg.SMB.Users = []config.User{
		{Name: "backup", Enabled: true, Password: "unchanged"},
		{Name: "reader", Enabled: true, ReadOnly: true},
		{Name: "disabled"},
	}
	cfg.TimeMachine = &config.TimeMachine{User: "legacy"}
	for _, tc := range []struct {
		name                      string
		readOnly, shared, wantErr bool
	}{
		{"backup", false, true, false},
		{"reader", true, true, false},
		{"reader", false, false, true},
		{"disabled", true, false, true},
		{"unconfigured", true, false, true},
		{"legacy", false, false, false},
	} {
		profile := config.TimeMachine{User: tc.name, ReadOnly: tc.readOnly}
		password, err := timeMachineAccount(cfg, &profile)
		if (err != nil) != tc.wantErr || err == nil && profile.SMBUser != tc.shared {
			t.Errorf("%s: profile=%+v err=%v", tc.name, profile, err)
		}
		if tc.name == "backup" && password != "unchanged" {
			t.Error("existing credentials are unavailable for the connection check")
		}
	}
	cfg.TimeMachine = &config.TimeMachine{User: "unconfigured", SMBUser: true}
	if _, err := timeMachineAccount(cfg, &config.TimeMachine{User: "unconfigured"}); err == nil {
		t.Error("removed shared account became a legacy account")
	}
}

func TestRecognizeLegacyTimeMachine(t *testing.T) {
	const manual = `[TimeMachine]
path = /tmp/mnt/usb
browseable = yes
read only = no
guest ok = no
valid users = timemachine
force user = root
ea support = yes
vfs objects = catia fruit streams_xattr
fruit:time machine = yes
fruit:resource = file

[all]
available = no
browseable = no
`
	path, profile, err := recognizeLegacyTimeMachine(strings.NewReader(manual), "all")
	if err != nil || path != "/tmp/mnt/usb" || profile.User != "timemachine" || profile.Share != "TimeMachine" {
		t.Fatalf("manual configuration was not recognized: %q %+v %v", path, profile, err)
	}
	for _, changed := range []string{
		strings.Replace(manual, "available = no", "available = yes", 1),
		strings.Replace(manual, "valid users = timemachine", "valid users = timemachine admin", 1),
		manual + "\n[global]\ninclude = /tmp/unknown\n",
	} {
		if _, _, err := recognizeLegacyTimeMachine(strings.NewReader(changed), "all"); err == nil {
			t.Error("unrecognized manual configuration could be replaced")
		}
	}
}

func TestUsersCannotRevokeActiveTimeMachineAccount(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.ConfigFile = filepath.Join(t.TempDir(), "config.json")
	cfg.SMB.Users = []config.User{{Name: "backup", Enabled: true, Password: "existing"}}
	cfg.TimeMachine = &config.TimeMachine{Enabled: true, User: "backup", SMBUser: true}
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.cancel()
	for _, body := range []string{`[]`, `[{"name":"backup","enabled":false}]`,
		`[{"name":"backup","enabled":true,"readonly":true}]`} {
		req := httptest.NewRequest(http.MethodPost, "/api/users", strings.NewReader(body))
		req.SetBasicAuth(cfg.Web.User, cfg.Web.Password)
		res := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(res, req)
		if res.Code != http.StatusConflict || !bytes.Contains(res.Body.Bytes(), []byte(errTimeMachineUserInUse.Error())) {
			t.Errorf("user update %s returned %d: %s", body, res.Code, res.Body.String())
		}
	}
	if users := cfg.Snapshot().SMB.Users; len(users) != 1 || users[0].Password != "existing" || !users[0].Enabled {
		t.Fatal("failed user update changed the account")
	}
}
