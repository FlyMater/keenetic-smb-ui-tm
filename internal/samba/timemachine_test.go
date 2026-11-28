package samba

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/acl"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
)

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	cfg := config.Default()
	cfg.SMB.SharePath = t.TempDir()
	cfg.TimeMachine = &config.TimeMachine{Enabled: true, UUID: "123", Share: "Mac Studio", User: "tm_mac", LimitGiB: 100}
	cfg.Folders = []config.Folder{{Name: "usb-old", Path: "local-123/old", Enabled: true}}
	return cfg
}

func TestRenderTimeMachineIsolation(t *testing.T) {
	cfg := testConfig(t)
	cfg.SMB.MinProtocol = "NT1"
	target := filepath.Join(t.TempDir(), "volume")
	render := RenderDisks(cfg, map[string]string{"123": target}, nil)
	regular, tm, ok := strings.Cut(render, "\n[Mac Studio]\n")
	if !ok {
		t.Fatal("missing Time Machine resource")
	}
	for _, required := range []string{"server min protocol = SMB2", "multicast dns register = no",
		"wide links = no", "invalid users = tm_mac"} {
		if !strings.Contains(regular, required) {
			t.Errorf("missing %q from existing shares", required)
		}
	}
	for _, required := range []string{"path = " + target, "read only = no", "valid users = tm_mac",
		"vfs objects = catia fruit streams_xattr", "fruit:time machine = yes", "fruit:resource = file",
		"fruit:time machine max size = 100G", "veto files = /Backups.backupdb/", "delete veto files = no"} {
		if !strings.Contains(tm, required) {
			t.Errorf("missing %q from Time Machine share", required)
		}
	}
	if strings.Contains(tm, "force user") || strings.Contains(regular, "[usb-old]") {
		t.Fatal("unsafe Samba export")
	}
	if rendered := RenderDisks(cfg, nil, nil); !strings.Contains(rendered, "wide links = no") ||
		strings.Contains(rendered, "[Mac Studio]") {
		t.Fatal("absent USB volume must leave isolation enabled without exporting an empty path")
	}
	cfg.SMB.MinProtocol = "SMB3_11"
	cfg.TimeMachine.ReadOnly = true
	render = RenderDisks(cfg, map[string]string{"123": target}, nil)
	if !strings.Contains(render, "server min protocol = SMB3_11") ||
		!strings.Contains(render, "fruit:time machine = no") || strings.Contains(render, "time machine max size") {
		t.Fatal("read-only exposure must keep stronger SMB minimum and suppress Time Machine capability")
	}
}

func TestTimeMachineRejectsSambaConflicts(t *testing.T) {
	work := filepath.Join(t.TempDir(), "tm")
	for name, mutate := range map[string]func(*config.Config){
		"share collision":   func(cfg *config.Config) { cfg.SMB.ShareName = "mac studio" },
		"user collision":    func(cfg *config.Config) { cfg.SMB.Users[0].Name = "TM_MAC" },
		"raw include":       func(cfg *config.Config) { cfg.SMB.ExtraOptions = []string{"include = /tmp/custom.conf"} },
		"combined override": func(cfg *config.Config) { cfg.SMB.ExtraOptions = []string{"log level = 0\nforce user = root"} },
		"root account":      func(cfg *config.Config) { cfg.TimeMachine.User = "root" },
		"unmanaged port":    func(cfg *config.Config) { cfg.SMB.Ports = "445 139" },
		"nested aggregate":  func(cfg *config.Config) { cfg.SMB.SharePath = filepath.Dir(work) },
	} {
		t.Run(name, func(t *testing.T) {
			cfg := testConfig(t)
			mutate(cfg)
			if err := ValidateTimeMachine(cfg, work); err == nil {
				t.Fatal("accepted conflicting configuration")
			}
		})
	}
	if err := ValidateTimeMachine(testConfig(t), work); err != nil {
		t.Fatal(err)
	}
}

func TestTransactionRestoresConfigAndAccountDatabase(t *testing.T) {
	dir := t.TempDir()
	configPath, sambaPath, passdb := filepath.Join(dir, "config"), filepath.Join(dir, "smb.conf"), filepath.Join(dir, "passdb")
	if err := os.WriteFile(configPath, []byte("original config"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passdb, []byte("original hashes"), 0600); err != nil {
		t.Fatal(err)
	}
	tx, err := NewTransaction(filepath.Join(dir, "journal"), configPath, sambaPath, passdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := tx.RecordNewAccount("tm_mac"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Capture(); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{configPath, sambaPath, passdb} {
		if err := os.WriteFile(path, []byte("candidate"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate an interrupted application and a reboot before rollback.
	recovered, err := NewTransaction(filepath.Join(dir, "journal"), configPath, sambaPath, passdb)
	if err != nil || !recovered.Pending() {
		t.Fatalf("expected persistent recovery point: %v", err)
	}
	if name, err := recovered.NewAccount(); name != "tm_mac" || err != nil {
		t.Fatalf("recovery account = %q, %v", name, err)
	}
	if err := recovered.Restore(); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{configPath: "original config", passdb: "original hashes"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("restored %s = %q (%v)", path, got, err)
		}
	}
	if _, err := os.Stat(sambaPath); !os.IsNotExist(err) {
		t.Fatalf("new Samba config survived rollback: %v", err)
	}
	if err := recovered.Complete(); err != nil || recovered.Pending() {
		t.Fatalf("journal remained after rollback: %v", err)
	}
}

func TestTransactionKeepsJournalAfterRestoreWriteFailure(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "smb.conf")
	if err := os.WriteFile(path, []byte("prior config"), 0600); err != nil {
		t.Fatal(err)
	}
	tx, err := NewTransaction(filepath.Join(dir, "journal"), path)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Capture(); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	if err := tx.Restore(); err == nil || !tx.Pending() {
		t.Fatal("failed restore discarded the recovery journal")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := tx.Restore(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "prior config" {
		t.Fatalf("retry did not restore prior config: %q, %v", got, err)
	}
}

func TestTransactionRecoveryBeforeSambaStops(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.json")
	passdb := filepath.Join(dir, "passdb")
	journal := filepath.Join(dir, "journal")
	tx, err := NewTransaction(journal, configPath, passdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := tx.RecordNewAccount("tm_mac"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(passdb, []byte("live Samba database"), 0600); err != nil {
		t.Fatal(err)
	}
	// With no file snapshot yet, recovery can find the new Unix account
	// without rolling back a write made by the live smbd.
	recovered, err := NewTransaction(journal, configPath, passdb)
	if err != nil {
		t.Fatal(err)
	}
	if err := recovered.Prepare(); err == nil {
		t.Fatal("a second apply replaced the preparation marker")
	}
	if err := recovered.Restore(); err != nil {
		t.Fatal(err)
	}
	if name, err := recovered.NewAccount(); err != nil || name != "tm_mac" {
		t.Fatalf("missing recovery account: %q, %v", name, err)
	}
	got, err := os.ReadFile(passdb)
	if err != nil || string(got) != "live Samba database" {
		t.Fatalf("preparation recovery touched passdb: %q, %v", got, err)
	}
}

func TestManagedSambaProcess(t *testing.T) {
	const conf = "/opt/etc/keenetic-smb-ui/smb.conf"
	for _, test := range []struct {
		exe, args string
		want      bool
	}{
		{"/opt/sbin/smbd", "smbd\x00-D\x00--configfile\x00" + conf + "\x00", true},
		{"/usr/bin/qemu-aarch64", "qemu-aarch64\x00/opt/sbin/smbd\x00smbd\x00-D\x00--configfile=" + conf + "\x00", true},
		{"/usr/sbin/smbd", "smbd\x00-D\x00--configfile\x00/etc/samba/smb.conf\x00", false},
		{"/bin/sh", "sh\x00script\x00--configfile=" + conf + "\x00", false},
		{"/usr/bin/qemu-aarch64", "qemu-aarch64\x00/opt/bin/other\x00other\x00--configfile=" + conf + "\x00", false},
	} {
		if got := managedProcess(test.exe, []byte(test.args), conf); got != test.want {
			t.Errorf("managedProcess(%q, %q) = %t, want %t", test.exe, test.args, got, test.want)
		}
	}
}

func TestRenderSeveralBackupSharesAndOrdinaryUSBAccess(t *testing.T) {
	cfg := config.Default()
	cfg.SMB.Users = []config.User{{Name: "backup", Enabled: true}}
	cfg.Folders = []config.Folder{
		{Name: "Files one", Path: "local-one/files", Enabled: true, Rules: []acl.Rule{{User: "backup", Access: string(acl.ReadWrite)}}},
		{Name: "Files two", Path: "local-two/files", Enabled: true, Rules: []acl.Rule{{User: "backup", Access: string(acl.ReadWrite)}}},
		{Name: "Unselected", Path: "local-other/files", Enabled: true, Rules: []acl.Rule{{User: "backup", Access: string(acl.ReadWrite)}}},
	}
	for _, uuid := range []string{"one", "two"} {
		cfg.LocalDisks = append(cfg.LocalDisks, config.LocalDisk{
			UUID: uuid, Publish: true, Directory: "local-" + uuid,
			TimeMachine: &config.TimeMachine{Enabled: true, User: "backup", SMBUser: true, Share: "Backup " + uuid},
		})
	}
	out := RenderDisks(cfg, map[string]string{"one": "/test/1", "two": "/test/2"}, map[string]bool{"local-one": true, "local-two": true})
	for _, section := range []string{"[all]", "[Files one]", "[Files two]", "[Backup one]", "[Backup two]"} {
		if !strings.Contains(out, section) {
			t.Errorf("missing %s", section)
		}
	}
	if strings.Contains(out, "[Unselected]") || strings.Contains(out, "invalid users = backup") {
		t.Fatal("unselected disk was exposed or shared user was blocked")
	}
	global, _, _ := strings.Cut(out, "\n[all]\n")
	if !strings.Contains(global, "vfs objects = catia fruit streams_xattr") {
		t.Fatal("ordinary shares must support AAPL when backup targets are active")
	}
	if strings.Count(out, "fruit:time machine = yes") != 2 {
		t.Fatal("each active backup share must advertise Time Machine capability")
	}
}
