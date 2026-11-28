package timemachine

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestTimeMachineOwnershipTransfer(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("changing file ownership requires root")
	}

	parent := os.Getenv("KEENETIC_TEST_OWNERSHIP_DIR")
	if parent == "" {
		parent = t.TempDir()
	}
	root, err := os.MkdirTemp(parent, ".keenetic-ownership-test-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	bundle := "existing.sparsebundle"
	if err := os.MkdirAll(filepath.Join(root, bundle, "bands"), 0777); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{bundle + "/bands/0", "other"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte("test fixture"), 0666); err != nil {
			t.Fatal(err)
		}
	}
	dir, err := os.Open(root)
	if err != nil {
		t.Fatal(err)
	}
	mountID, err := fdMountID(int(dir.Fd()))
	if err != nil {
		dir.Close()
		t.Fatal(err)
	}
	target := &Target{file: dir, MountID: mountID}
	defer target.Close()

	owner := Identity{UID: 1001, GID: 1001}
	if err := target.CheckOwnershipSupport(owner); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var journal bytes.Buffer
	if err := target.SaveOwnership(ctx, bundle, &journal); err != nil {
		t.Fatal(err)
	}
	// Simulate interruption after the first entry has been changed.
	first := journal.Bytes()[:bytes.IndexByte(journal.Bytes(), '\n')+1]
	if err := target.ApplyOwnership(ctx, bytes.NewReader(first), owner, false); err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyOwnership(ctx, bytes.NewReader(journal.Bytes()), owner, true); err != nil {
		t.Fatal(err)
	}
	if err := target.ApplyOwnership(ctx, bytes.NewReader(journal.Bytes()), owner, false); err != nil {
		t.Fatal(err)
	}
	for name, uid := range map[string]uint32{"": owner.UID, bundle + "/bands/0": owner.UID, "other": 0} {
		var stat syscall.Stat_t
		if err := syscall.Stat(filepath.Join(root, name), &stat); err != nil || stat.Uid != uid {
			t.Errorf("%q has owner %d, want %d (%v)", name, stat.Uid, uid, err)
		}
	}
	if err := target.ApplyOwnership(ctx, bytes.NewReader(journal.Bytes()), owner, true); err != nil {
		t.Fatal(err)
	}
	var band syscall.Stat_t
	if err := syscall.Stat(filepath.Join(root, bundle, "bands", "0"), &band); err != nil || band.Uid != 0 {
		t.Fatal("interrupted migration could not restore the original owner", err)
	}
}
