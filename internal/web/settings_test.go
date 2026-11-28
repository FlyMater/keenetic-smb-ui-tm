package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/tommilevs/keenetic-smb-ui/internal/acl"
	"github.com/tommilevs/keenetic-smb-ui/internal/config"
)

func TestSaveSettingsPreservesSMB(t *testing.T) {
	cfg := config.Default()
	dir := t.TempDir()
	cfg.Paths.ConfigFile = filepath.Join(dir, "config.json")
	cfg.Paths.DataDir = dir
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.cancel)

	request := func(method, path, password, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		if password != "" {
			req.SetBasicAuth(cfg.Web.User, password)
		}
		rec := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(rec, req)
		return rec
	}
	password := cfg.Web.Password
	if rec := request(http.MethodGet, "/api/config", password, ""); rec.Code != http.StatusOK {
		t.Fatalf("loading settings returned %d", rec.Code)
	}
	if err := cfg.Update(func(c *config.Config) error {
		c.SMB.ListenIP = "192.168.160.22"
		c.SMB.ShareName = "new-share"
		c.SMB.ServerString = "new-server-name"
		c.SMB.Users = []config.User{{Name: "media", Password: "smb-password", Enabled: true}}
		c.Remotes = []config.Remote{{ID: "nas", Host: "nas.local", Share: "music", Password: "remote-password", Enabled: true}}
		c.Folders = []config.Folder{{ID: "media", Path: "media", Enabled: true, Rules: []acl.Rule{
			{User: "media", Access: string(acl.ReadOnly), Inherit: true},
		}}}
		c.TimeMachine = &config.TimeMachine{Enabled: true, UUID: "test-volume", Folder: "backups", Share: "TM", User: "tm_acct"}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	assertConfig := func(want config.Config) {
		t.Helper()
		if got := cfg.Snapshot(); !reflect.DeepEqual(got, want) {
			t.Fatal("unexpected in-memory configuration after saving settings")
		}
		saved, err := config.Load(want.Paths.ConfigFile)
		if err != nil {
			t.Fatal(err)
		}
		want.Paths.DataDir = "" // Runtime-only field.
		if !reflect.DeepEqual(*saved, want) {
			t.Fatal("unexpected configuration on disk after saving settings")
		}
	}

	want := cfg.Snapshot()
	settings := `{"web":{"listen":"127.0.0.1:8098","user":"admin","password":""},"stats":{"interval_sec":8,"history_len":60}}`
	invalid := `{"web":{},"stats":{},"smb":{"enabled":false}}`
	if rec := request(http.MethodPost, "/api/settings", password, invalid); rec.Code != http.StatusBadRequest {
		t.Fatalf("saving SMB via settings returned %d", rec.Code)
	}
	if rec := request(http.MethodPost, "/api/settings", "", settings); rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated settings save returned %d", rec.Code)
	}
	assertConfig(want)
	if rec := request(http.MethodPost, "/api/settings", password, settings); rec.Code != http.StatusOK {
		t.Fatalf("saving settings returned %d: %s", rec.Code, rec.Body)
	}
	want.Web.Listen = "127.0.0.1:8098"
	want.Stats = config.Stats{IntervalSec: 8, HistoryLen: 60}
	assertConfig(want)

	settings = `{"web":{"listen":"127.0.0.1:8098","user":"admin","password":"new-web-password","readonly":true},"stats":{"interval_sec":8,"history_len":60}}`
	if rec := request(http.MethodPost, "/api/settings", password, settings); rec.Code != http.StatusOK {
		t.Fatalf("updating panel credentials returned %d: %s", rec.Code, rec.Body)
	}
	want.Web.Password = "new-web-password"
	want.Web.ReadOnly = true
	assertConfig(want)
	if rec := request(http.MethodPost, "/api/settings", want.Web.Password, settings); rec.Code != http.StatusForbidden {
		t.Fatalf("saving settings in read-only mode returned %d", rec.Code)
	}
	assertConfig(want)
}
