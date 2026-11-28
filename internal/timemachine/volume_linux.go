package timemachine

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func currentMounts() ([]mount, error) {
	f, err := os.Open("/proc/self/mountinfo")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return readMounts(f)
}

func physicalDevice(device string) blockDevice {
	resolve := func(root string) blockDevice {
		if _, err := os.Stat(filepath.Join(root, "partition")); err == nil {
			root = filepath.Dir(root)
		}
		if slaves, err := os.ReadDir(filepath.Join(root, "slaves")); err == nil && len(slaves) != 0 {
			if len(slaves) == 1 {
				if device, err := os.ReadFile(filepath.Join(root, "slaves", slaves[0].Name(), "dev")); err == nil {
					if backing := physicalDevice(strings.TrimSpace(string(device))); backing.name != "" {
						return backing
					}
				}
			}
			return blockDevice{name: root, composite: true}
		}
		for dir := root; dir != "/sys" && dir != "/"; dir = filepath.Dir(dir) {
			subsystem, err := os.Readlink(filepath.Join(dir, "subsystem"))
			if err == nil && filepath.Base(subsystem) == "usb" {
				return blockDevice{name: root, usb: true}
			}
		}
		return blockDevice{name: root}
	}
	root, err := filepath.EvalSymlinks(filepath.Join("/sys/dev/block", device))
	if err != nil {
		return blockDevice{}
	}
	return resolve(root)
}

func mountDevice(m mount) blockDevice {
	disk := physicalDevice(m.device)
	if disk.name == "" && strings.HasPrefix(m.source, "/dev/") {
		// A FUSE filesystem has a synthetic mount device, but its source
		// may still identify the backing USB partition.
		source, err := filepath.EvalSymlinks(m.source)
		if err == nil {
			dev, err := os.ReadFile(filepath.Join("/sys/class/block", filepath.Base(source), "dev"))
			if err == nil {
				disk = physicalDevice(strings.TrimSpace(string(dev)))
			}
		}
	}
	return disk
}

func volumes(ctx context.Context, mounts []mount, entwarePath string, serviceMounts ...string) ([]Volume, error) {
	opt, err := filepath.EvalSymlinks(entwarePath)
	if err != nil {
		return nil, fmt.Errorf("locate Entware storage: %w", err)
	}
	entwareMount := coveringMount(mounts, opt)
	entware := mountDevice(entwareMount)
	if entware.name == "" && (entwareMount.device == "" || Contains("/tmp/mnt", opt) ||
		strings.HasPrefix(entwareMount.source, "/dev/")) {
		return nil, errors.New("cannot identify Entware storage")
	}
	if strings.HasPrefix(filepath.Base(entware.name), "loop") {
		backing, err := os.ReadFile(filepath.Join(entware.name, "loop", "backing_file"))
		if err != nil {
			return nil, errors.New("cannot identify backing storage for Entware")
		}
		host := coveringMount(mounts, strings.TrimSpace(string(backing)))
		entware = mountDevice(host)
		if entware.name == "" {
			return nil, errors.New("cannot identify backing storage for Entware")
		}
	}
	if entware.composite {
		return nil, errors.New("cannot isolate all physical disks backing Entware storage")
	}
	// mountinfo reports resolved paths, including when /opt is a symlink.
	excluded := make(map[string]bool, len(serviceMounts))
	for _, path := range serviceMounts {
		if real, err := resolvePath(path); err == nil {
			excluded[real] = true
		}
	}
	sources := make([]mount, 0, len(mounts))
	for _, m := range mounts {
		if !excluded[m.path] {
			sources = append(sources, m)
		}
	}
	return inventory(sources, entware, "", mountDevice, func(m mount) Volume {
		probe, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		out, _ := exec.CommandContext(probe, "blkid", "-c", "/dev/null", "-s", "UUID", "-o", "value", m.source).Output()
		v := Volume{UUID: strings.TrimSpace(string(out))}
		var st syscall.Statfs_t
		if err := syscall.Statfs(m.path, &st); err == nil {
			v.Total, v.Free = uint64(st.Blocks)*uint64(st.Bsize), uint64(st.Bavail)*uint64(st.Bsize)
		} else {
			v.Reason = "USB volume capacity is unavailable"
		}
		return v
	}), ctx.Err()
}

func Volumes(ctx context.Context, serviceMounts ...string) ([]Volume, error) {
	mounts, err := currentMounts()
	if err != nil {
		return nil, err
	}
	return volumes(ctx, mounts, "/opt", serviceMounts...)
}

// ValidateAggregate rejects an aggregate that is itself on a USB mount or
// contains USB bind mounts. The normal Entware /opt aggregate is permitted.
func ValidateAggregate(path string, protected ...string) error {
	return ValidateAggregateMounts(path, nil, protected...)
}

func ValidateAggregateMounts(path string, managed []string, protected ...string) error {
	real, err := resolvePath(path)
	if err != nil {
		return err
	}
	for _, path := range protected {
		private, err := resolvePath(path)
		if err != nil {
			return err
		}
		if Contains(real, private) || Contains(private, real) {
			return errors.New("aggregated share overlaps private Samba state")
		}
	}
	opt, err := filepath.EvalSymlinks("/opt")
	if err != nil {
		return err
	}
	mounts, err := currentMounts()
	if err != nil {
		return err
	}
	owner, entware := coveringMount(mounts, real), coveringMount(mounts, opt)
	entwareAggregate := Contains(opt, real) && owner.id == entware.id
	ownerDisk := mountDevice(owner)
	if !entwareAggregate && (Contains("/tmp/mnt", real) || Contains(real, "/tmp/mnt") ||
		ownerDisk.usb || ownerDisk.composite) {
		return errors.New("aggregate is on USB storage or contains USB mount paths")
	}
	allowed := make(map[string]bool, len(managed))
	for _, path := range managed {
		if resolved, err := resolvePath(path); err == nil {
			allowed[resolved] = true
		}
	}
	for _, m := range mounts {
		if allowed[m.path] {
			continue
		}
		if m.id != owner.id && Contains(real, m.path) {
			disk := mountDevice(m)
			if !disk.usb && !disk.composite {
				continue
			}
			return errors.New("aggregate contains a USB mount")
		}
	}
	return nil
}

func resolvePath(path string) (string, error) {
	resolved, err := filepath.EvalSymlinks(path)
	if !errors.Is(err, os.ErrNotExist) {
		return resolved, err
	}
	if _, statErr := os.Lstat(path); statErr == nil {
		return "", err // a dangling link is not an empty destination
	}
	parent := filepath.Dir(path)
	if parent == path {
		return "", err
	}
	resolved, err = resolvePath(parent)
	if err != nil {
		return "", err
	}
	return filepath.Join(resolved, filepath.Base(path)), nil
}

type Target struct {
	file          *os.File
	createdParent *os.File
	createdName   string
	MountID       string
	Inode         uint64
}

func (t *Target) Close() error {
	t.KeepCreatedFolder()
	return t.file.Close()
}

func (t *Target) KeepCreatedFolder() {
	if t.createdParent != nil {
		_ = t.createdParent.Close()
		t.createdParent = nil
	}
}

func (t *Target) RemoveCreatedFolder() error {
	if t.createdParent == nil {
		return nil
	}
	defer t.KeepCreatedFolder()
	path := fmt.Sprintf("/proc/self/fd/%d/%s", t.createdParent.Fd(), t.createdName)
	current, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	created, err := t.file.Stat()
	if err != nil {
		return err
	}
	if !current.IsDir() || !os.SameFile(current, created) {
		return errors.New("new backup directory changed before cleanup")
	}
	return os.Remove(path)
}

func fdMountID(fd int) (string, error) {
	data, err := os.ReadFile("/proc/self/fdinfo/" + strconv.Itoa(fd))
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if value, ok := strings.CutPrefix(line, "mnt_id:"); ok {
			return strings.TrimSpace(value), nil
		}
	}
	return "", errors.New("kernel did not report mount identity")
}

type Identity struct {
	UID, GID uint32
	Groups   []uint32
}

func OpenTarget(volume Volume, folder string, create *Identity) (*Target, error) {
	parts, err := FolderParts(folder)
	if err != nil {
		return nil, err
	}
	const flags = syscall.O_DIRECTORY | syscall.O_NOFOLLOW | syscall.O_CLOEXEC | syscall.O_RDONLY
	// Walk from / using directory descriptors so mounting or replacing a
	// pathname while it is being checked cannot redirect the bind mount.
	fd, err := syscall.Open("/", flags, 0)
	if err != nil {
		return nil, err
	}
	defer func() {
		if fd >= 0 {
			_ = syscall.Close(fd)
		}
	}()
	next := func(part string) error {
		child, err := syscall.Openat(fd, part, flags, 0)
		if err == nil {
			_ = syscall.Close(fd)
			fd = child
		}
		return err
	}
	for _, part := range strings.Split(strings.TrimPrefix(volume.MountPoint, "/"), "/") {
		if part != "" {
			if err := next(part); err != nil {
				return nil, err
			}
		}
	}
	checkMount := func() error {
		id, err := fdMountID(fd)
		if err != nil {
			return err
		}
		if id != volume.mount.id {
			return errors.New("USB volume was detached or folder crosses a mount boundary")
		}
		return nil
	}
	if err := checkMount(); err != nil {
		return nil, err
	}
	var createdParent *os.File
	var createdName string
	defer func() {
		if createdParent != nil {
			_ = createdParent.Close()
		}
	}()
	for i, part := range parts {
		err := next(part)
		if errors.Is(err, syscall.ENOENT) && create != nil && i == len(parts)-1 {
			parent := fd
			if err := syscall.Mkdirat(parent, part, 0700); err != nil {
				return nil, err
			}
			child, err := syscall.Openat(parent, part, flags, 0)
			if err != nil {
				return nil, err
			}
			var info syscall.Stat_t
			if err := syscall.Fstat(child, &info); err != nil || info.Uid != uint32(os.Geteuid()) || info.Mode&0777 != 0700 {
				_ = syscall.Close(child)
				return nil, errors.New("new directory ownership changed during creation")
			}
			if err := syscall.Fchown(child, int(create.UID), int(create.GID)); err != nil {
				_ = syscall.Close(child)
				return nil, err
			}
			createdParent = os.NewFile(uintptr(parent), "new backup folder parent")
			createdName = part
			fd = child
		} else if err != nil {
			return nil, err
		}
		if err := checkMount(); err != nil {
			return nil, err
		}
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(fd, &stat); err != nil {
		return nil, err
	}
	t := &Target{file: os.NewFile(uintptr(fd), "USB backup folder"), createdParent: createdParent,
		createdName: createdName, MountID: volume.mount.id, Inode: uint64(stat.Ino)}
	fd = -1
	createdParent = nil
	return t, nil
}

func (t *Target) Folders() ([]string, error) {
	names := make([]string, 0)
	for {
		entries, err := t.file.ReadDir(128)
		if err != nil && !errors.Is(err, io.EOF) {
			return nil, err
		}
		for _, entry := range entries {
			if _, err := FolderParts(entry.Name()); err != nil || !entry.IsDir() {
				continue
			}
			fd, err := syscall.Openat(int(t.file.Fd()), entry.Name(), syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				continue
			}
			id, idErr := fdMountID(fd)
			_ = syscall.Close(fd)
			if idErr == nil && id == t.MountID {
				names = append(names, entry.Name())
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
	}
	sort.Strings(names)
	return names, nil
}

// IsMount reports whether path is a mount point rather than a backing directory.
func IsMount(path string) (bool, error) {
	real, err := resolvePath(path)
	if err != nil {
		return false, err
	}
	mounts, err := currentMounts()
	if err != nil {
		return false, err
	}
	return coveringMount(mounts, real).path == real, nil
}

func (t *Target) Bind(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	if err := os.Mkdir(path, 0000); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0 {
		return errors.New("unsafe Time Machine bind mount backing directory")
	}
	return syscall.Mount(fmt.Sprintf("/proc/self/fd/%d", t.file.Fd()), path, "", syscall.MS_BIND, "")
}

func Detach(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return errors.New("unsafe Time Machine bind mount path")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	mounts, err := currentMounts()
	if err != nil {
		return err
	}
	if coveringMount(mounts, real).path != real {
		return nil
	}
	err = syscall.Unmount(real, syscall.MNT_DETACH)
	if errors.Is(err, syscall.EINVAL) || errors.Is(err, syscall.ENOENT) {
		return nil
	}
	return err
}

func (t *Target) Probe(ctx context.Context, identity Identity, readOnly bool) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	mode := "write"
	if readOnly {
		mode = "read"
	}
	cmd := exec.CommandContext(ctx, exe, "--timemachine-probe", mode)
	cmd.ExtraFiles = []*os.File{t.file}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{
		Uid: identity.UID, Gid: identity.GID, Groups: identity.Groups,
	}}
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("USB permissions or xattr probe failed: %s (%w)", strings.TrimSpace(string(out)), err)
	}
	return nil
}

// ProbeFile runs only in the privilege-dropped helper with the validated
// directory inherited as descriptor 3.
func ProbeFile(write bool) error {
	if os.Geteuid() == 0 {
		return errors.New("probe requires a non-root account")
	}
	if err := syscall.Fchdir(3); err != nil {
		return err
	}
	dir, err := os.Open(".")
	if err != nil {
		return err
	}
	defer dir.Close()
	if !write {
		_, err := dir.Readdirnames(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return err
		}
		return nil
	}
	f, err := os.CreateTemp(".", ".time-machine-check-*")
	if err != nil {
		return err
	}
	defer f.Close()
	defer os.Remove(f.Name())
	if _, err := f.WriteString("Time Machine"); err != nil {
		return err
	}
	const attr = "user.keenetic-time-machine-check"
	path := fmt.Sprintf("/proc/self/fd/%d", f.Fd())
	if err := syscall.Setxattr(path, attr, []byte{1}, 0); err != nil {
		return err
	}
	value := make([]byte, 1)
	if n, err := syscall.Getxattr(path, attr, value); err != nil || n != 1 || value[0] != 1 {
		return errors.New("USB volume did not retain the test xattr")
	}
	return nil
}
