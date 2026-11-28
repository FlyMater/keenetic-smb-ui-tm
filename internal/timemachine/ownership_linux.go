package timemachine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// O_PATH is part of the Linux ABI on the project's target architectures.
// The frozen syscall package omits it on amd64, 386 and arm.
const openPath = 0x200000

type ownershipEntry struct {
	Path           string
	Inode          uint64
	UID, GID, Mode uint32
}

// openOwnershipEntry walks from the bound directory without following links,
// including when re-opening entries after a restart.
func (t *Target) openOwnershipEntry(path string) (*os.File, syscall.Stat_t, error) {
	var stat syscall.Stat_t
	parts, err := FolderParts(path)
	if err != nil {
		return nil, stat, err
	}
	parts = append([]string{"."}, parts...)
	parent := int(t.file.Fd())
	var file *os.File
	for i, part := range parts {
		flags := openPath | syscall.O_CLOEXEC | syscall.O_NOFOLLOW
		if i < len(parts)-1 {
			flags |= syscall.O_DIRECTORY
		}
		fd, err := syscall.Openat(parent, part, flags, 0)
		if file != nil {
			_ = file.Close()
		}
		if err != nil {
			return nil, stat, err
		}
		file = os.NewFile(uintptr(fd), part)
		parent = fd
		id, err := fdMountID(fd)
		if err != nil || id != t.MountID {
			_ = file.Close()
			return nil, stat, errors.New("backup ownership cannot cross mounts")
		}
	}
	if err := syscall.Fstat(parent, &stat); err != nil {
		_ = file.Close()
		return nil, stat, err
	}
	kind := stat.Mode & syscall.S_IFMT
	if (kind != syscall.S_IFDIR && (kind != syscall.S_IFREG || stat.Nlink != 1)) ||
		stat.Mode&(syscall.S_ISUID|syscall.S_ISGID) != 0 {
		_ = file.Close()
		return nil, stat, errors.New("backup contains an entry whose ownership cannot safely change")
	}
	// An O_PATH descriptor pins the inode without opening special files.
	// Re-open only the regular file or directory that was just verified.
	opened, err := os.Open(fmt.Sprintf("/proc/self/fd/%d", file.Fd()))
	_ = file.Close()
	return opened, stat, err
}

func (t *Target) CheckOwnershipSupport(identity Identity) error {
	probe, err := os.CreateTemp(fmt.Sprintf("/proc/self/fd/%d", t.file.Fd()), ".timemachine-owner-*")
	if err != nil {
		return err
	}
	changeErr := changeOwner(probe, identity.UID, identity.GID)
	return errors.Join(changeErr, probe.Close(), os.Remove(probe.Name()))
}

func changeOwner(file *os.File, uid, gid uint32) error {
	if err := file.Chown(int(uid), int(gid)); err != nil {
		return err
	}
	var stat syscall.Stat_t
	if err := syscall.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if stat.Uid != uid || stat.Gid != gid {
		return errors.New("USB filesystem did not retain the requested ownership")
	}
	return nil
}

// SaveOwnership records the target itself and, optionally, a single backup
// bundle. No other descendants of the target are visited.
func (t *Target) SaveOwnership(ctx context.Context, bundle string, w io.Writer) error {
	enc := json.NewEncoder(w)
	var walk func(string, bool) error
	walk = func(path string, descend bool) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		file, stat, err := t.openOwnershipEntry(path)
		if err != nil {
			return err
		}
		defer file.Close()
		if err := enc.Encode(ownershipEntry{path, stat.Ino, stat.Uid, stat.Gid, stat.Mode}); err != nil {
			return err
		}
		if !descend || stat.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			if path == bundle && bundle != "" {
				return errors.New("selected backup bundle is not a directory")
			}
			return nil
		}
		for {
			names, err := file.Readdirnames(128)
			for _, name := range names {
				if err := walk(path+"/"+name, true); err != nil {
					return err
				}
			}
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
		}
	}
	if bundle != "" && (filepath.Base(bundle) != bundle || !strings.HasSuffix(strings.ToLower(bundle), ".sparsebundle")) {
		return errors.New("invalid backup bundle")
	}
	if err := walk("", false); err != nil {
		return err
	}
	if bundle != "" {
		return walk(bundle, true)
	}
	return nil
}

func (t *Target) ApplyOwnership(ctx context.Context, r io.Reader, owner Identity, restore bool) error {
	dec := json.NewDecoder(r)
	changed := false
	defer func() {
		if changed {
			syscall.Sync()
		}
	}()
	var failure error
	for {
		if err := ctx.Err(); err != nil {
			return errors.Join(failure, err)
		}
		var entry ownershipEntry
		if err := dec.Decode(&entry); err != nil {
			if errors.Is(err, io.EOF) {
				return failure
			}
			return errors.Join(failure, err)
		}
		didChange, err := t.applyOwner(entry, owner, restore)
		changed = changed || didChange
		if err != nil {
			if !restore {
				return err
			}
			// Restore the remaining entries even when one disappeared.
			if failure == nil {
				failure = err
			}
		}
	}
}

func (t *Target) applyOwner(entry ownershipEntry, owner Identity, restore bool) (bool, error) {
	file, stat, err := t.openOwnershipEntry(entry.Path)
	if err != nil {
		return false, err
	}
	defer file.Close()
	uid, gid := owner.UID, owner.GID
	fromUID, fromGID := entry.UID, entry.GID
	if restore {
		uid, gid, fromUID, fromGID = fromUID, fromGID, uid, gid
	}
	switch {
	case stat.Ino != entry.Inode || stat.Mode != entry.Mode:
		return false, errors.New("backup entry changed since its ownership was recorded")
	case stat.Uid == uid && stat.Gid == gid:
		return false, nil
	case stat.Uid != fromUID || stat.Gid != fromGID:
		return false, errors.New("backup entry ownership changed concurrently")
	default:
		return true, changeOwner(file, uid, gid)
	}
}
