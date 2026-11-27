package timemachine

import (
	"strings"
	"testing"
)

func TestUSBInventory(t *testing.T) {
	const listing = `10 1 8:1 / /tmp/mnt/entware rw - ext4 /dev/sda1 rw
11 1 8:2 / /tmp/mnt/sibling rw - ext4 /dev/sda2 rw
12 1 8:17 / /tmp/mnt/Back\040ups rw - ext4 /dev/sdb1 rw
13 1 8:33 / /tmp/mnt/clone rw - ext4 /dev/sdc1 rw
14 1 8:49 / /tmp/mnt/readonly ro - btrfs /dev/sdd1 rw
15 1 8:65 / /tmp/mnt/unknown rw - ext4 /dev/sde1 rw
16 1 8:17 / /opt/var/timemachine/volume rw - ext4 /dev/sdb1 rw
17 1 0:65 / /opt/share/remote rw - cifs //nas/share rw
18 1 8:81 / /tmp/mnt/target rw - ext4 /dev/sdf1 rw
`
	mounts, err := readMounts(strings.NewReader(listing))
	if err != nil {
		t.Fatal(err)
	}
	block := func(m mount) blockDevice {
		if m.source == "/dev/sda1" || m.source == "/dev/sda2" {
			return blockDevice{name: "entware", usb: true}
		}
		return blockDevice{name: m.device, usb: strings.HasPrefix(m.source, "/dev/")}
	}
	got := inventory(mounts, block(mounts[0]), "/opt/var/timemachine/volume", block, func(m mount) Volume {
		uuids := map[string]string{"12": "abcd", "13": "ABCD", "14": "read-only", "18": "unique"}
		return Volume{UUID: uuids[m.id], Total: 2 << 30, Free: 1 << 30}
	})
	if len(got) != 5 {
		t.Fatalf("expected five candidate volumes, got %+v", got)
	}
	if got[0].MountPoint != "/tmp/mnt/Back ups" {
		t.Fatalf("mount point was not decoded: %q", got[0].MountPoint)
	}
	for _, uuid := range []string{"abcd", "read-only", "missing", ""} {
		if _, err := Find(got, uuid); err == nil {
			t.Errorf("unexpected volume match for UUID %q", uuid)
		}
	}
	target, err := Find(got, "UNIQUE")
	if err != nil || target.Filesystem != "ext4" || target.Free != 1<<30 {
		t.Fatalf("target = %+v, error = %v", target, err)
	}
}

func TestFolderProtection(t *testing.T) {
	for _, path := range []string{"/media", "../folder", "folder/..", "a//b", "backups.backupdb", "nested/Backups.backupdb/host", "a\\b"} {
		if _, err := FolderParts(path); err == nil {
			t.Errorf("accepted unsafe folder %q", path)
		}
	}
	for _, path := range []string{"", "Mac Backups", "macs/design team"} {
		if _, err := FolderParts(path); err != nil {
			t.Errorf("rejected valid folder %q: %v", path, err)
		}
	}
}
