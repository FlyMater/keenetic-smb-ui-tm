package samba

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tommilevs/keenetic-smb-ui/internal/config"
)

// TestEntwareTimeMachine exercises the package's Samba tools in a disposable
// Entware environment. The supplied address must belong to that environment.
func TestEntwareTimeMachine(t *testing.T) {
	ip, user := os.Getenv("KEENETIC_TEST_SMB_IP"), os.Getenv("KEENETIC_TEST_SMB_USER")
	if ip == "" || user == "" {
		t.Skip("requires an isolated Entware test environment")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if err := EnsurePortFree(ip); err != nil {
		t.Fatal(err)
	}
	id, err := UnixIdentity(ctx, user)
	if err != nil {
		t.Fatal(err)
	}
	dir, err := os.MkdirTemp("", "keenetic-smb-integration-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	if err := os.Chmod(dir, 0755); err != nil {
		t.Fatal(err)
	}
	cfg := config.Default()
	cfg.SMB.ListenIP, cfg.SMB.SharePath = ip, filepath.Join(dir, "all")
	cfg.Paths.SambaPidDir, cfg.Paths.SambaConf = filepath.Join(dir, "run"), filepath.Join(dir, "smb.conf")
	cfg.TimeMachine = &config.TimeMachine{Enabled: true, Share: "Mac Backup", User: user, LimitGiB: 5}
	target := filepath.Join(dir, "volume")
	for _, name := range []string{"all", "volume", "run", "private", "state", "cache", "lock", "rpc", "log"} {
		if err := os.Mkdir(filepath.Join(dir, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chown(target, int(id.UID), int(id.GID)); err != nil {
		t.Fatal(err)
	}
	legacy := filepath.Join(target, "Backups.backupdb")
	if err := os.Mkdir(legacy, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "original.txt"), []byte("legacy backup"), 0644); err != nil {
		t.Fatal(err)
	}
	content := RenderWithTimeMachine(cfg, target)
	content = strings.NewReplacer(
		"/opt/etc/samba/private", filepath.Join(dir, "private"),
		"/opt/var/keenetic-smb-ui/state", filepath.Join(dir, "state"),
		"/opt/var/keenetic-smb-ui/cache", filepath.Join(dir, "cache"),
		"/opt/var/keenetic-smb-ui/lock", filepath.Join(dir, "lock"),
		"/opt/var/keenetic-smb-ui/log", filepath.Join(dir, "log"),
	).Replace(content)
	content = strings.Replace(content, "[global]\n", "[global]\n   ncalrpc dir = "+filepath.Join(dir, "rpc")+"\n", 1)
	if err := os.WriteFile(cfg.Paths.SambaConf, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	if out, err := exec.CommandContext(ctx, "testparm", "-s", cfg.Paths.SambaConf).CombinedOutput(); err != nil {
		t.Fatalf("testparm: %v: %s", err, out)
	}
	const password = "test-fixture%password!"
	if err := SetPassword(ctx, cfg.Paths.SambaConf, user, password); err != nil {
		t.Fatal(err)
	}
	if !HasAccount(ctx, cfg.Paths.SambaConf, user) {
		t.Fatal("smbpasswd did not create the Samba account")
	}
	manager := New(cfg)
	defer func() {
		if err := manager.Stop(); err != nil {
			t.Error(err)
		}
	}()
	if err := manager.StartInstalled(ctx, cfg); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "log", "samba.log"))
		t.Fatalf("%v\n%s", err, log)
	}
	if err := CheckListener(ctx, ip); err != nil {
		t.Fatal(err)
	}
	if err := CheckSMB(ctx, cfg, password); err != nil {
		log, _ := os.ReadFile(filepath.Join(dir, "log", "samba.log"))
		t.Fatalf("%v\n%s", err, log)
	}
	if err := CheckSMB(ctx, cfg, "incorrect"); err == nil {
		t.Fatal("accepted invalid SMB credentials")
	}
	cmd := exec.CommandContext(ctx, "smbclient", "//"+ip+"/"+cfg.TimeMachine.Share,
		"-I", ip, "-U", user, "--use-kerberos=off", "-c", "ls")
	cmd.Env = append(os.Environ(), "PASSWD="+password)
	out, err := cmd.CombinedOutput()
	if err != nil || strings.Contains(strings.ToLower(string(out)), "backups.backupdb") {
		t.Fatalf("legacy directory is visible or listing failed: %v: %s", err, out)
	}
	cfg.TimeMachine.Share = cfg.SMB.ShareName
	if err := CheckSMB(ctx, cfg, password); err == nil {
		t.Fatal("Time Machine user can log into the aggregate share")
	}
	data, err := os.ReadFile(filepath.Join(legacy, "original.txt"))
	if err != nil || string(data) != "legacy backup" {
		t.Fatalf("legacy backup changed: %v", err)
	}
}
