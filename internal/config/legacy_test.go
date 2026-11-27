package config

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestPackagePreservesLegacyConfig(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "opt/etc/keenetic-smb-ui")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	legacy, err := os.ReadFile("../../packaging/config.default.json")
	if err != nil {
		t.Fatal(err)
	}
	working := filepath.Join(dir, "config.json")
	seed := filepath.Join(dir, "config.default.json")
	if err := os.WriteFile(seed, legacy, 0600); err != nil {
		t.Fatal(err)
	}
	run := func(name string) {
		t.Helper()
		cmd := exec.Command("sh", "../../packaging/"+name)
		cmd.Env = append(os.Environ(), "IPKG_INSTROOT="+root)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v, %s", name, err, output)
		}
	}
	run("postinst")
	assert := func(want string) {
		t.Helper()
		got, err := os.ReadFile(working)
		if err != nil || string(got) != want {
			t.Fatalf("working config %q, error %v", got, err)
		}
		info, err := os.Stat(working)
		if err != nil || info.Mode().Perm() != 0600 {
			t.Errorf("working configuration mode is not 0600")
		}
	}
	assert(string(legacy))
	const oldConfig = `{"smb":{"listen_ip":"192.168.1.77","users":[{"name":"legacy","password":"legacy","enabled":true}]}}`
	if err := os.WriteFile(working, []byte(oldConfig), 0600); err != nil {
		t.Fatal(err)
	}
	// Simulate the 1.7.0 prerm backup when opkg leaves the working file
	// or removes the former conffile while upgrading.
	if err := os.WriteFile(working+".bak-auto", []byte(oldConfig+"\n  "), 0600); err != nil {
		t.Fatal(err)
	}
	run("postinst")
	assert(oldConfig)
	if err := os.Remove(working); err != nil {
		t.Fatal(err)
	}
	run("postinst")
	assert(oldConfig + "\n  ")
	cfg, err := Load(working)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.TimeMachine != nil || cfg.SMB.ListenIP != "192.168.1.77" || cfg.SMB.Users[0].Password != "legacy" {
		t.Fatal("legacy settings did not survive the upgrade")
	}
	run("prerm")
	if err := os.Remove(working); err != nil {
		t.Fatal(err)
	}
	run("postinst")
	assert(oldConfig + "\n  ")
}
