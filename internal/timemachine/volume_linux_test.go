package timemachine

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOpenTargetRejectsLinksAndNestedMounts(t *testing.T) {
	mounts, err := currentMounts()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"ok", "Backups.backupdb"} {
		if err := os.Mkdir(filepath.Join(root, name), 0755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("ok", filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	volume := Volume{MountPoint: root, mount: coveringMount(mounts, root)}
	target, err := OpenTarget(volume, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer target.Close()
	folders, err := target.Folders()
	if err != nil || len(folders) != 1 || folders[0] != "ok" {
		t.Fatalf("folders = %v, error = %v", folders, err)
	}
	for _, name := range []string{"link", "Backups.backupdb"} {
		if bad, err := OpenTarget(volume, name, nil); err == nil {
			bad.Close()
			t.Fatalf("opened protected path %q", name)
		}
	}
	volume = Volume{MountPoint: "/", mount: coveringMount(mounts, "/")}
	if coveringMount(mounts, "/proc").id == volume.mount.id {
		t.Skip("fixture has no separate proc mount")
	}
	if bad, err := OpenTarget(volume, "proc", nil); err == nil {
		bad.Close()
		t.Fatal("opened a directory on another mount")
	}
}

func TestFolderReplacementAndCleanup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires the disposable Linux root fixture")
	}
	mounts, err := currentMounts()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	volume := Volume{MountPoint: root, mount: coveringMount(mounts, root)}
	account := Identity{UID: 65534, GID: 65534}
	old, err := OpenTarget(volume, "new", &account)
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	if err := old.RemoveCreatedFolder(); err != nil {
		t.Fatal(err)
	}
	replacement, err := OpenTarget(volume, "new", &account)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if old.MountID == replacement.MountID && old.Inode == replacement.Inode {
		t.Fatal("replacement target has the same identity as the removed open directory")
	}
}

// TestBindAfterReplacement uses real bind mounts in a disposable Linux mount
// namespace when explicitly enabled by the integration harness.
func TestBindAfterReplacement(t *testing.T) {
	if os.Getenv("KEENETIC_TEST_BIND") != "1" {
		t.Skip("requires a disposable Linux mount namespace with CAP_SYS_ADMIN")
	}
	mounts, err := currentMounts()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	volume := Volume{MountPoint: root, mount: coveringMount(mounts, root)}
	source := filepath.Join(root, "backup")
	bind := filepath.Join(t.TempDir(), "volume")
	create := func(text string) {
		t.Helper()
		if err := os.Mkdir(source, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(source, "marker"), []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
	}
	read := func(want string) {
		t.Helper()
		got, err := os.ReadFile(filepath.Join(bind, "marker"))
		if err != nil || string(got) != want {
			t.Fatalf("bind content = %q, %v, want %q", got, err, want)
		}
	}
	create("original")
	original, err := OpenTarget(volume, "backup", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer original.Close()
	if err := original.Bind(bind); err != nil {
		t.Fatal(err)
	}
	defer Detach(bind)
	other, err := OpenTarget(volume, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	otherBind := filepath.Join(t.TempDir(), "other")
	if err := other.Bind(otherBind); err != nil {
		t.Fatal(err)
	}
	defer Detach(otherBind)
	read("original")
	if err := os.Rename(source, source+"-detached"); err != nil {
		t.Fatal(err)
	}
	create("replacement")
	read("original")
	if err := Detach(bind); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bind, "marker")); !os.IsNotExist(err) {
		t.Fatalf("stale backup target remained accessible: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(otherBind, "backup-detached", "marker"))
	if err != nil || string(got) != "original" {
		t.Fatalf("detaching one destination affected another: %q, %v", got, err)
	}
	replacement, err := OpenTarget(volume, "backup", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer replacement.Close()
	if err := replacement.Bind(bind); err != nil {
		t.Fatal(err)
	}
	read("replacement")
}
