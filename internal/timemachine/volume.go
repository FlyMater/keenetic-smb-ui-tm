package timemachine

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"
)

type Volume struct {
	UUID       string `json:"uuid"`
	MountPoint string `json:"mount_point"`
	Filesystem string `json:"filesystem"`
	Total      uint64 `json:"total"`
	Free       uint64 `json:"free"`
	ReadOnly   bool   `json:"readonly,omitempty"`
	Reason     string `json:"reason,omitempty"`
	mount      mount
}

type mount struct {
	id, device, root, path, filesystem, source string
	readOnly                                   bool
}

func readMounts(r io.Reader) ([]mount, error) {
	var mounts []mount
	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 4096), 256*1024)
	for scanner.Scan() {
		left, right, ok := strings.Cut(scanner.Text(), " - ")
		a, b := strings.Fields(left), strings.Fields(right)
		if !ok || len(a) < 6 || len(b) < 3 {
			return nil, errors.New("invalid mountinfo entry")
		}
		mounts = append(mounts, mount{
			id: a[0], device: a[2], root: unescape(a[3]), path: unescape(a[4]),
			filesystem: b[0], source: unescape(b[1]),
			readOnly: hasOption(a[5], "ro") || hasOption(b[2], "ro"),
		})
	}
	return mounts, scanner.Err()
}

func hasOption(options, name string) bool {
	for _, option := range strings.Split(options, ",") {
		if option == name {
			return true
		}
	}
	return false
}

func unescape(s string) string {
	return strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`).Replace(s)
}

func Contains(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func coveringMount(mounts []mount, path string) mount {
	var best mount
	for _, m := range mounts {
		if Contains(m.path, path) && len(m.path) >= len(best.path) {
			best = m
		}
	}
	return best
}

func FolderParts(path string) ([]string, error) {
	if path == "" {
		return nil, nil
	}
	parts := strings.Split(path, "/")
	for _, part := range parts {
		if part == "" || part == "." || part == ".." || strings.EqualFold(part, "Backups.backupdb") ||
			strings.ContainsAny(part, "\\\n\r\x00") {
			return nil, fmt.Errorf("invalid backup folder component %q", part)
		}
	}
	return parts, nil
}

func Find(volumes []Volume, uuid string) (Volume, error) {
	return FindDisk(volumes, uuid, false)
}

const readOnlyVolume = "USB volume is mounted read-only"

func FindDisk(volumes []Volume, uuid string, allowReadOnly bool) (Volume, error) {
	for _, volume := range volumes {
		if strings.EqualFold(volume.UUID, uuid) && uuid != "" {
			if volume.Reason != "" && !(allowReadOnly && volume.Reason == readOnlyVolume) {
				return Volume{}, errors.New(volume.Reason)
			}
			return volume, nil
		}
	}
	return Volume{}, errors.New("USB volume unavailable")
}

type blockDevice struct {
	name      string
	usb       bool
	composite bool
}

func inventory(mounts []mount, entware blockDevice, serviceMount string, block func(mount) blockDevice,
	metadata func(mount) Volume) []Volume {
	count := make(map[string]int)
	volumes := make([]Volume, 0)
	for _, m := range mounts {
		if m.path == serviceMount {
			continue
		}
		device := block(m)
		routerSubvolume := m.filesystem == "btrfs" && Contains("/tmp/mnt", m.path)
		if !device.usb || device.name == entware.name || (m.root != "/" && !routerSubvolume) ||
			!strings.HasPrefix(m.source, "/dev/") {
			continue
		}
		v := metadata(m)
		v.MountPoint, v.Filesystem, v.mount, v.ReadOnly = m.path, m.filesystem, m, m.readOnly
		switch {
		case v.UUID == "":
			v.Reason = "USB volume has no readable UUID"
		case m.readOnly:
			v.Reason = readOnlyVolume
		}
		if v.UUID != "" {
			count[strings.ToLower(v.UUID)]++
		}
		volumes = append(volumes, v)
	}
	for i := range volumes {
		if count[strings.ToLower(volumes[i].UUID)] > 1 {
			volumes[i].Reason = "USB volume UUID is ambiguous"
		}
	}
	sort.Slice(volumes, func(i, j int) bool { return volumes[i].MountPoint < volumes[j].MountPoint })
	return volumes
}
