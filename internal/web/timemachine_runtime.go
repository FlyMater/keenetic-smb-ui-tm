package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/samba"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/timemachine"
)

type diskBindings map[string]*timemachine.Target

func (bindings diskBindings) close(discard bool) {
	for _, target := range bindings {
		if discard {
			_ = target.RemoveCreatedFolder()
		}
		_ = target.Close()
	}
}

func (bindings diskBindings) same(other diskBindings) bool {
	if len(bindings) != len(other) {
		return false
	}
	for path, target := range bindings {
		if previous := other[path]; previous == nil || previous.MountID != target.MountID || previous.Inode != target.Inode {
			return false
		}
	}
	return true
}

func backupPath(cfg *config.Config, uuid string) string {
	key := sha256.Sum256([]byte(strings.ToLower(uuid)))
	return filepath.Join(tmDir(cfg), "volumes", fmt.Sprintf("%x", key))
}

func (s *Server) remoteDirectories(cfg *config.Config) map[string]bool {
	local := map[string]bool{}
	for _, remote := range cfg.Remotes {
		if remote.Enabled {
			local[s.shares.Directory(remote)] = true
		}
	}
	return local
}

func (s *Server) diskShares(cfg *config.Config, bindings diskBindings) (map[string]string, map[string]bool) {
	backups, local := map[string]string{}, s.remoteDirectories(cfg)
	for _, disk := range cfg.Disks() {
		path := backupPath(cfg, disk.UUID)
		if bindings[path] != nil {
			backups[strings.ToLower(disk.UUID)] = path
		}
		if disk.Publish && bindings[filepath.Join(cfg.SMB.SharePath, disk.Directory)] != nil {
			local[disk.Directory] = true
		}
	}
	return backups, local
}

func (s *Server) volumeInventory(ctx context.Context, cfg *config.Config) ([]timemachine.Volume, error) {
	return timemachine.Volumes(ctx, append(slices.Clone(s.tm.mounts), tmPath(cfg))...)
}

func (s *Server) validateAggregate(cfg *config.Config) error {
	return timemachine.ValidateAggregateMounts(cfg.SMB.SharePath, s.tm.mounts, tmDir(cfg),
		filepath.Dir(cfg.Paths.SambaConf), filepath.Dir(samba.PassDB))
}

func (s *Server) installation(ctx context.Context, cfg *config.Config, bindings diskBindings, syncUsers bool) error {
	if err := s.validateAggregate(cfg); err != nil {
		return err
	}
	backups, local := s.diskShares(cfg, bindings)
	if err := s.smb.WriteCandidate(cfg, backups, local); err != nil {
		return err
	}
	if syncUsers {
		if err := s.smb.SyncCandidateUsers(cfg); err != nil {
			return err
		}
	}
	if s.tm.paused || !cfg.SMB.Enabled {
		return nil
	}
	if len(backups) != 0 {
		if err := samba.EnsurePortFree(cfg.SMB.ListenIP); err != nil {
			return err
		}
	}
	if err := s.smb.StartInstalled(ctx, cfg); err != nil {
		return err
	}
	if len(backups) != 0 {
		return samba.CheckListener(ctx, cfg.SMB.ListenIP)
	}
	return nil
}

func (s *Server) recordMounts(cfg *config.Config, paths []string) error {
	if len(paths) == 0 && len(s.tm.mounts) == 0 {
		return nil
	}
	dir := tmDir(cfg)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	file, err := os.CreateTemp(dir, ".mounts-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	encodeErr := json.NewEncoder(file).Encode(paths)
	if err := errors.Join(encodeErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), filepath.Join(dir, "mounts.json")); err != nil {
		return err
	}
	s.tm.mounts = paths
	parent, err := os.Open(dir)
	if err != nil {
		return err
	}
	return errors.Join(parent.Sync(), parent.Close())
}

func detachDisk(path string) error {
	if err := timemachine.Detach(path); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode().Perm() != 0 {
		return fmt.Errorf("unexpected USB mount backing directory: %s", path)
	}
	return os.Remove(path)
}

// switchBindings journals the exact destinations before mounting anything.
// Recovery can then withdraw mounts even if the new configuration was never saved.
func (s *Server) switchBindings(cfg *config.Config, next diskBindings) error {
	oldPaths := slices.Clone(s.tm.mounts)
	union := slices.Clone(oldPaths)
	paths := make([]string, 0, len(next))
	for path := range next {
		paths = append(paths, path)
		if !slices.Contains(oldPaths, path) {
			if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
				if err != nil {
					return err
				}
				return fmt.Errorf("USB publication path already exists: %s", path)
			}
			union = append(union, path)
		}
	}
	sort.Strings(paths)
	if err := s.recordMounts(cfg, union); err != nil {
		return err
	}
	for _, path := range oldPaths {
		if err := detachDisk(path); err != nil {
			return err
		}
	}
	for _, path := range paths {
		if err := next[path].Bind(path); err != nil {
			return err
		}
	}
	return s.recordMounts(cfg, paths)
}

func (s *Server) stopDiskBindings(cfg *config.Config) error {
	if err := s.switchBindings(cfg, nil); err != nil {
		return err
	}
	s.tm.bindings.close(false)
	s.tm.bindings = nil
	return nil
}

func legacyDiskLink(path string) bool {
	name, ok := strings.CutPrefix(filepath.Base(path), "local-")
	if !ok || name == "" {
		return false
	}
	target, err := os.Readlink(path)
	return err == nil && target == filepath.Join("/tmp/mnt", name)
}

func (s *Server) checkPublicationPath(path string, allowLegacyLink bool) error {
	if slices.Contains(s.tm.mounts, path) {
		return nil
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if allowLegacyLink && info.Mode()&os.ModeSymlink != 0 && legacyDiskLink(path) {
		return nil
	}
	return fmt.Errorf("USB publication path already exists: %s", path)
}

func removeLegacyDiskLinks(root string) error {
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		path := filepath.Join(root, entry.Name())
		if entry.Type()&os.ModeSymlink != 0 && legacyDiskLink(path) {
			if err := os.Remove(path); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *Server) clearBonjour() {
	s.tm.bonjour.Close()
	s.tm.advertised, s.tm.bonjourError = false, ""
}

func (s *Server) updateBonjour(ctx context.Context) {
	backups, _ := s.diskShares(s.cfg, s.tm.bindings)
	var advertised []string
	for _, disk := range s.cfg.Disks() {
		if p := disk.TimeMachine; p != nil && p.Bonjour && !p.ReadOnly && backups[strings.ToLower(disk.UUID)] != "" {
			advertised = append(advertised, p.Share)
		}
	}
	if len(advertised) == 0 || !s.smb.Running() {
		s.clearBonjour()
		return
	}
	sort.Strings(advertised)
	err := s.tm.bonjour.Publish(ctx, timemachine.Announcement{
		Interface: s.cfg.SMB.BindDevice, IP: s.cfg.SMB.ListenIP, Shares: advertised,
	})
	s.tm.advertised, s.tm.bonjourError = err == nil, ""
	if err != nil {
		s.tm.bonjourError = err.Error()
	}
}

func (s *Server) reconfigureSamba(ctx context.Context, syncUsers bool) error {
	return s.reconcileTimeMachine(ctx, true, syncUsers)
}

func (s *Server) reconcileTimeMachine(ctx context.Context, force, syncUsers bool) error {
	if s.tm.recoveryError != "" || (!force && (s.tm.paused || !s.cfg.SMB.Enabled || !hasDiskSelection(s.cfg))) {
		return nil
	}
	cfg := s.cfg.Snapshot()
	var next diskBindings
	var statuses map[string]string
	if cfg.SMB.Enabled && !s.tm.paused && hasDiskSelection(&cfg) {
		next, statuses, _ = s.openDiskBindings(ctx, &cfg, nil, nil, "")
	}
	s.tm.diskErrors = statuses
	if !force && next.same(s.tm.bindings) && s.smb.Running() {
		next.close(false)
		s.updateBonjour(ctx)
		return nil
	}
	previous := s.tm.bindings
	if err := s.smb.StopInstalled(&cfg); err != nil {
		next.close(false)
		return err
	}
	s.clearBonjour()
	if err := removeLegacyDiskLinks(cfg.SMB.SharePath); err != nil {
		next.close(false)
		return err
	}
	if err := s.switchBindings(&cfg, next); err != nil {
		next.close(false)
		// A disconnected disk must never be reattached from an old descriptor.
		if detachErr := s.stopDiskBindings(&cfg); detachErr != nil {
			return errors.Join(err, detachErr)
		}
		return errors.Join(err, s.installation(ctx, &cfg, nil, syncUsers))
	}
	previous.close(false)
	s.tm.bindings = next
	if err := s.installation(ctx, &cfg, next, syncUsers); err != nil {
		if stopErr := s.smb.StopInstalled(&cfg); stopErr != nil {
			return errors.Join(err, stopErr)
		}
		if detachErr := s.stopDiskBindings(&cfg); detachErr != nil {
			return errors.Join(err, detachErr)
		}
		return errors.Join(err, s.installation(ctx, &cfg, nil, false))
	}
	s.updateBonjour(ctx)
	return nil
}

func (s *Server) recoverTimeMachine() error {
	cfg := s.cfg.Snapshot()
	dir := tmDir(&cfg)
	data, err := os.ReadFile(filepath.Join(dir, "mounts.json"))
	if err == nil {
		if err := json.Unmarshal(data, &s.tm.mounts); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	_, err = os.Stat(filepath.Join(dir, "transaction", "pending"))
	pending := err == nil
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	legacyMount := false
	if cfg.TimeMachine != nil {
		legacyMount, err = timemachine.IsMount(tmPath(&cfg))
		if err != nil {
			return err
		}
	}
	if len(s.tm.mounts) != 0 || pending || legacyMount {
		if err := s.smb.StopInstalled(&cfg); err != nil {
			return err
		}
		if err := s.stopDiskBindings(&cfg); err != nil {
			return err
		}
	}
	if legacyMount || pending {
		if err := timemachine.Detach(tmPath(&cfg)); err != nil {
			return err
		}
	}
	if !pending {
		return nil
	}
	tx, err := tmTransaction(&cfg)
	if err != nil {
		return err
	}
	if err := tx.Restore(); err != nil {
		return err
	}
	ownerCtx, cancel := context.WithTimeout(s.ctx, 15*time.Minute)
	ownerErr := applyTimeMachineOwnership(ownerCtx, &cfg, nil, true)
	cancel()
	if ownerErr != nil {
		return ownerErr
	}
	ctx, stop := context.WithTimeout(s.ctx, 5*time.Second)
	defer stop()
	if err := rollbackAccount(ctx, tx); err != nil {
		return err
	}
	restored, err := config.Load(cfg.Paths.ConfigFile)
	if err != nil {
		return err
	}
	restored.Paths.DataDir = cfg.Paths.DataDir
	s.cfg.Replace(*restored)
	if err := tx.Complete(); err != nil {
		return err
	}
	_ = os.Remove(tmOwnershipPath(&cfg))
	return nil
}

func rollbackAccount(ctx context.Context, tx *samba.Transaction) error {
	name, err := tx.NewAccount()
	if err != nil || name == "" {
		return err
	}
	if samba.UnixExists(ctx, name) {
		return samba.DeleteUnix(ctx, name)
	}
	return ctx.Err()
}
