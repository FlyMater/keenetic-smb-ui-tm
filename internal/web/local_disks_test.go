package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/timemachine"
)

func TestLocalDisksPreserveLegacyBackupWithoutPublishingUSB(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.ConfigFile = filepath.Join(t.TempDir(), "config.json")
	cfg.TimeMachine = &config.TimeMachine{
		Enabled: true, UUID: "BACKUP-1", Folder: "mac", Share: "Mac Backup", User: "tm_user",
		LimitGiB: 200, Bonjour: true,
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/local-disks", nil)
	req.SetBasicAuth(cfg.Web.User, cfg.Web.Password)
	res := httptest.NewRecorder()
	server.http.Handler.ServeHTTP(res, req)
	var result struct {
		Disks []config.LocalDisk `json:"disks"`
	}
	if res.Code != http.StatusOK || json.Unmarshal(res.Body.Bytes(), &result) != nil {
		t.Fatalf("unexpected disk response %d: %s", res.Code, res.Body)
	}
	if len(result.Disks) != 1 || result.Disks[0].Publish || result.Disks[0].UUID != "BACKUP-1" ||
		result.Disks[0].TimeMachine == nil || *result.Disks[0].TimeMachine != *cfg.TimeMachine {
		t.Fatalf("legacy backup settings were changed: %+v", result.Disks)
	}
	if cfg.LocalDisks != nil {
		t.Fatal("listing the available disks changed the configuration")
	}
}

func TestLocalDiskEditsRequireExplicitPublication(t *testing.T) {
	cfg := config.Default()
	cfg.Paths.ConfigFile = filepath.Join(t.TempDir(), "config.json")
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.cancel()
	send := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/api/local-disks", strings.NewReader(body))
		req.SetBasicAuth(cfg.Web.User, cfg.Web.Password)
		res := httptest.NewRecorder()
		server.http.Handler.ServeHTTP(res, req)
		return res
	}
	res := send(`{"disks":[{"uuid":"A","publish":false,"time_machine":{"enabled":true}}]}`)
	if res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.Body.String(), "publish the USB disk") {
		t.Fatalf("backup without an SMB selection returned %d: %s", res.Code, res.Body)
	}
	res = send(`{"disks":[{"uuid":"A","publish":false},{"uuid":"a","publish":false}]}`)
	if res.Code != http.StatusUnprocessableEntity || !strings.Contains(res.Body.String(), "duplicate USB volume UUID") {
		t.Fatalf("duplicate UUID returned %d: %s", res.Code, res.Body)
	}
	res = send(`{"disks":[{"uuid":"unused","publish":false}]}`)
	if res.Code != http.StatusOK || len(cfg.LocalDisks) != 0 {
		t.Fatalf("unselected disk was persisted: %d, %+v", res.Code, cfg.LocalDisks)
	}
}

func TestDiskSharesWithdrawOnlyMissingDisk(t *testing.T) {
	cfg := config.Default()
	cfg.LocalDisks = []config.LocalDisk{
		{UUID: "one", Directory: "local-one", Publish: true, TimeMachine: &config.TimeMachine{Enabled: true}},
		{UUID: "two", Directory: "local-two", Publish: true, TimeMachine: &config.TimeMachine{Enabled: true}},
	}
	server, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer server.cancel()
	path := filepath.Join(cfg.SMB.SharePath, "local-two")
	tm := backupPath(cfg, "two")
	backups, ordinary := server.diskShares(cfg, diskBindings{
		path: &timemachine.Target{}, tm: &timemachine.Target{},
	})
	if len(backups) != 1 || backups["two"] != tm || len(ordinary) != 1 || !ordinary["local-two"] {
		t.Fatalf("unexpected active exports: %v, %v", backups, ordinary)
	}
	if len(cfg.LocalDisks) != 2 || !cfg.LocalDisks[0].Publish || !cfg.LocalDisks[0].TimeMachine.Enabled {
		t.Fatal("temporary disconnect removed the disk selection")
	}
}
