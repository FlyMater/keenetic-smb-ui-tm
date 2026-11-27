package web

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/tommilevs/keenetic-smb-ui/internal/config"
	"github.com/tommilevs/keenetic-smb-ui/internal/samba"
	"github.com/tommilevs/keenetic-smb-ui/internal/timemachine"
)

func (s *Server) switchTarget(cfg *config.Config, target *timemachine.Target) error {
	path := tmPath(cfg)
	if err := timemachine.Detach(path); err != nil {
		return err
	}
	if target != nil {
		return target.Bind(path)
	}
	return nil
}

func (s *Server) installSamba(ctx context.Context, cfg *config.Config, target *timemachine.Target, start, syncUsers bool) error {
	path := ""
	if target != nil {
		path = tmPath(cfg)
	}
	if err := s.smb.WriteCandidate(cfg, path); err != nil {
		return err
	}
	if syncUsers {
		if err := s.smb.SyncCandidateUsers(cfg); err != nil {
			return err
		}
	}
	if !start || !cfg.SMB.Enabled {
		return nil
	}
	if tmEnabled(cfg) {
		if err := samba.EnsurePortFree(cfg.SMB.ListenIP); err != nil {
			return err
		}
	}
	if err := s.smb.StartInstalled(ctx, cfg); err != nil {
		return err
	}
	if tmEnabled(cfg) {
		return samba.CheckListener(ctx, cfg.SMB.ListenIP)
	}
	return nil
}

func (s *Server) applyTimeMachine(ctx context.Context, request timeMachineRequest) (result error) {
	old := s.cfg.Snapshot()
	next := s.cfg.Snapshot()
	p := &request.TimeMachine
	if !p.Enabled {
		if old.TimeMachine == nil || !old.TimeMachine.Enabled {
			return nil
		}
		saved := *old.TimeMachine
		saved.Enabled = false
		p = &saved
	}
	next.TimeMachine = p
	var volume timemachine.Volume
	if p.Enabled {
		var err error
		volume, err = s.validateTimeMachine(ctx, &next, request)
		if err != nil {
			return err
		}
	}
	oldUser := ""
	if old.TimeMachine != nil {
		oldUser = old.TimeMachine.User
	}
	newAccount := p.Enabled && p.User != oldUser
	if newAccount && (request.Password == "" || samba.UnixExists(ctx, p.User)) {
		return errors.New("a new Time Machine account requires an unused Unix name and a password")
	}
	if p.Enabled && p.User == oldUser && request.Password == "" &&
		!samba.HasAccount(ctx, old.Paths.SambaConf, p.User) {
		return errors.New("enter a password to restore the Time Machine SMB account")
	}
	if !p.Enabled && oldUser != "" {
		// Keep the inactive profile so re-enabling it can reuse its account.
		p.User = oldUser
	}
	path := ""
	if p.Enabled {
		path = tmPath(&next)
	}
	if err := samba.ValidateCandidate(ctx, &next, path); err != nil {
		return err
	}
	tx, err := tmTransaction(&old)
	if err != nil {
		return err
	}
	if err := tx.Prepare(); err != nil {
		return err
	}
	previousTarget := s.tm.target
	wasRunning := s.smb.Running()
	var target *timemachine.Target
	var stopAttempted, bindingChanged, startAttempted bool
	defer func() {
		if result == nil {
			if target != nil {
				target.KeepCreatedFolder()
			}
			if previousTarget != nil {
				_ = previousTarget.Close()
			}
			return
		}
		rollback, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if target != nil {
			defer target.Close()
		}
		if startAttempted {
			if err := s.smb.StopInstalled(&next); err != nil {
				result = errors.Join(result, fmt.Errorf("rollback stop: %w", err))
				s.tm.recoveryError = result.Error()
				return
			}
		}
		var bindErr, restoreErr, startErr error
		if bindingChanged {
			bindErr = s.switchTarget(&old, previousTarget)
			restoreErr = tx.Restore()
			s.cfg.Replace(old)
		}
		var folderErr error
		if target != nil {
			folderErr = target.RemoveCreatedFolder()
		}
		if stopAttempted && wasRunning && restoreErr == nil && (bindErr == nil || previousTarget == nil) {
			startErr = s.smb.StartInstalled(rollback, &old)
		}
		accountCtx, cancelAccount := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancelAccount()
		userErr := rollbackAccount(accountCtx, tx)
		var completeErr error
		if bindErr == nil && restoreErr == nil && startErr == nil && userErr == nil {
			completeErr = tx.Complete()
		}
		result = errors.Join(result, bindErr, restoreErr, startErr, folderErr, userErr, completeErr)
		if bindErr != nil || restoreErr != nil || startErr != nil || userErr != nil || completeErr != nil {
			s.tm.recoveryError = result.Error()
		}
		if stopAttempted {
			s.updateBonjour(rollback)
		}
	}()
	if p.Enabled {
		if newAccount {
			if err := tx.RecordNewAccount(p.User); err != nil {
				return err
			}
			if err := samba.CreateUnix(ctx, p.User); err != nil {
				return fmt.Errorf("cannot create Time Machine Unix account: %w", err)
			}
		}
		identity, err := samba.UnixIdentity(ctx, p.User)
		if err != nil {
			return err
		}
		var create *timemachine.Identity
		if request.CreateFolder && p.Folder != "" {
			create = &identity
		}
		target, err = timemachine.OpenTarget(volume, p.Folder, create)
		if err != nil {
			return err
		}
		if err := target.Probe(ctx, identity, p.ReadOnly); err != nil {
			return err
		}
	}
	stopAttempted = true
	if err := s.smb.StopInstalled(&old); err != nil {
		return err
	}
	s.clearBonjour()
	if err := tx.Capture(); err != nil {
		return err
	}
	bindingChanged = true
	if err := s.switchTarget(&next, target); err != nil {
		return err
	}
	if err := s.installSamba(ctx, &next, target, false, false); err != nil {
		return err
	}
	if p.Enabled && request.Password != "" {
		if err := samba.SetPassword(ctx, next.Paths.SambaConf, p.User, request.Password); err != nil {
			return err
		}
	}
	if newAccount && oldUser != "" {
		if err := samba.DeletePassword(ctx, next.Paths.SambaConf, oldUser); err != nil {
			return err
		}
	}
	if next.SMB.Enabled && !s.tm.paused {
		if p.Enabled {
			if err := samba.EnsurePortFree(next.SMB.ListenIP); err != nil {
				return err
			}
		}
		startAttempted = true
		if err := s.smb.StartInstalled(ctx, &next); err != nil {
			return err
		}
		if p.Enabled {
			if err := samba.CheckListener(ctx, next.SMB.ListenIP); err != nil {
				return err
			}
			if request.Password != "" {
				if err := samba.CheckSMB(ctx, &next, request.Password); err != nil {
					return err
				}
			}
		}
	}
	if err := s.cfg.Update(func(cfg *config.Config) error {
		cfg.TimeMachine = p
		return nil
	}); err != nil {
		return err
	}
	if err := tx.Complete(); err != nil {
		return err
	}
	s.tm.target, s.tm.diskError = target, ""
	s.updateBonjour(ctx)
	return nil
}

func (s *Server) clearBonjour() {
	s.tm.bonjour.Close()
	s.tm.advertised, s.tm.bonjourError = false, ""
}

func (s *Server) updateBonjour(ctx context.Context) {
	p := s.cfg.TimeMachine
	if p == nil || !p.Enabled || !p.Bonjour || p.ReadOnly || s.tm.target == nil || !s.smb.Running() {
		s.clearBonjour()
		return
	}
	err := s.tm.bonjour.Publish(ctx, timemachine.Announcement{
		Interface: s.cfg.SMB.BindDevice, IP: s.cfg.SMB.ListenIP, Share: p.Share,
	})
	s.tm.advertised, s.tm.bonjourError = err == nil, ""
	if err != nil {
		s.tm.bonjourError = err.Error()
	}
}

// reconfigureSamba is used by the existing service and settings operations as
// well as hotplug reconciliation. The caller holds the server operation lock.
func (s *Server) reconfigureSamba(ctx context.Context, syncUsers bool) error {
	cfg := s.cfg.Snapshot()
	if !tmEnabled(&cfg) {
		if err := s.smb.StopInstalled(&cfg); err != nil {
			return err
		}
		return s.installSamba(ctx, &cfg, nil, !s.tm.paused, syncUsers)
	}
	return s.reconcileTimeMachine(ctx, true, syncUsers)
}

func (s *Server) reconcileTimeMachine(ctx context.Context, force, syncUsers bool) error {
	cfg := s.cfg.Snapshot()
	if !tmEnabled(&cfg) || s.tm.paused || s.tm.recoveryError != "" {
		return nil
	}
	if err := samba.ValidateTimeMachine(&cfg, tmPath(&cfg)); err != nil {
		return s.withdrawTimeMachine(err)
	}
	if err := samba.CheckAvailable(&cfg); err != nil {
		return s.withdrawTimeMachine(err)
	}
	if err := validateTimeMachineAggregate(&cfg); err != nil {
		return s.withdrawTimeMachine(err)
	}
	volume, err := selectVolume(ctx, &cfg)
	var target *timemachine.Target
	if err == nil {
		target, err = timemachine.OpenTarget(volume, cfg.TimeMachine.Folder, nil)
	}
	previous := s.tm.target
	if target != nil && previous != nil && previous.MountID == target.MountID &&
		previous.Inode == target.Inode && !force && s.smb.Running() {
		_ = target.Close()
		s.tm.diskError = ""
		if volume.Free == 0 && !cfg.TimeMachine.ReadOnly {
			s.tm.diskError = "USB volume has no free space"
		}
		s.updateBonjour(ctx)
		return nil
	}
	if target != nil {
		if !samba.HasAccount(ctx, cfg.Paths.SambaConf, cfg.TimeMachine.User) {
			err = errors.New("Time Machine SMB account is unavailable")
		} else if identity, accountErr := samba.UnixIdentity(ctx, cfg.TimeMachine.User); accountErr != nil {
			err = accountErr
		} else {
			err = target.Probe(ctx, identity, cfg.TimeMachine.ReadOnly)
		}
		if err != nil {
			_ = target.Close()
			target = nil
		}
	}
	s.tm.diskError = ""
	if err != nil {
		s.tm.diskError = err.Error()
	}
	if target == nil && previous == nil && !force && s.smb.Running() {
		return nil
	}
	path := ""
	if target != nil {
		path = tmPath(&cfg)
	}
	if err := samba.ValidateCandidate(ctx, &cfg, path); err != nil {
		if target != nil {
			_ = target.Close()
		}
		return s.withdrawTimeMachine(err)
	}
	if err := s.smb.StopInstalled(&cfg); err != nil {
		if target != nil {
			_ = target.Close()
		}
		return err
	}
	s.clearBonjour()
	if previous != nil {
		_ = previous.Close()
	}
	s.tm.target = nil
	if err := s.switchTarget(&cfg, target); err != nil {
		if target != nil {
			_ = target.Close()
		}
		s.tm.diskError = err.Error()
		return errors.Join(err, s.installSamba(ctx, &cfg, nil, true, syncUsers))
	}
	if err := s.installSamba(ctx, &cfg, target, true, syncUsers); err != nil {
		if target != nil {
			_ = target.Close()
		}
		s.tm.diskError = err.Error()
		if stopErr := s.smb.StopInstalled(&cfg); stopErr != nil {
			return errors.Join(err, stopErr)
		}
		if detachErr := s.switchTarget(&cfg, nil); detachErr != nil {
			return errors.Join(err, detachErr)
		}
		return errors.Join(err, s.installSamba(ctx, &cfg, nil, true, false))
	}
	s.tm.target = target
	s.updateBonjour(ctx)
	return nil
}

func (s *Server) withdrawTimeMachine(cause error) error {
	s.clearBonjour()
	s.tm.diskError = cause.Error()
	if err := s.smb.Stop(); err != nil {
		return errors.Join(cause, err)
	}
	if s.tm.target != nil {
		_ = s.tm.target.Close()
		s.tm.target = nil
	}
	return errors.Join(cause, timemachine.Detach(tmPath(s.cfg)))
}

func (s *Server) recoverTimeMachine() error {
	cfg := s.cfg.Snapshot()
	journal := filepath.Join(tmDir(&cfg), "transaction", "pending")
	if _, err := os.Stat(journal); errors.Is(err, os.ErrNotExist) {
		return nil
	} else if err != nil {
		return err
	}
	tx, err := tmTransaction(&cfg)
	if err != nil {
		return err
	}
	if err := s.smb.StopInstalled(&cfg); err != nil {
		return err
	}
	if err := timemachine.Detach(tmPath(&cfg)); err != nil {
		return err
	}
	if err := tx.Restore(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 5*time.Second)
	defer cancel()
	if err := rollbackAccount(ctx, tx); err != nil {
		return err
	}
	restored, err := config.Load(cfg.Paths.ConfigFile)
	if err != nil {
		return err
	}
	restored.Paths.DataDir = cfg.Paths.DataDir
	s.cfg.Replace(*restored)
	return tx.Complete()
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
