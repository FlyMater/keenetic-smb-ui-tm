package web

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/samba"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/timemachine"
)

type localDiskEdit struct {
	UUID        string              `json:"uuid"`
	Publish     bool                `json:"publish"`
	TimeMachine *timeMachineRequest `json:"time_machine,omitempty"`
}

type localDiskRequest struct {
	Disks []localDiskEdit `json:"disks"`
}

func (s *Server) handleLocalDisks(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
		defer cancel()
		volumes, err := s.volumeInventory(ctx, s.cfg)
		response := map[string]any{
			"volumes": volumes, "disks": s.cfg.Disks(), "users": timeMachineUsers(s.cfg),
			"legacy": s.timeMachineLegacyPreview(ctx), "share": s.cfg.SMB.ShareName,
		}
		if err != nil {
			response["inventory_error"] = err.Error()
		}
		diagnostics := map[string]any{}
		for _, disk := range s.cfg.Disks() {
			diagnostics[strings.ToLower(disk.UUID)] = s.tmDiagnostics(disk.UUID)
		}
		response["diagnostics"] = diagnostics
		writeJSON(w, response)
	case http.MethodPost:
		var request localDiskRequest
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64*1024))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&request); err != nil || request.Disks == nil || decoder.Decode(new(any)) != io.EOF {
			tmError(w, errors.New("invalid local disk request"), http.StatusBadRequest)
			return
		}
		timeout := 90 * time.Second
		for _, disk := range request.Disks {
			if disk.TimeMachine != nil && disk.TimeMachine.Migration != nil {
				timeout = 15 * time.Minute
			}
		}
		ctx, cancel := context.WithTimeout(s.ctx, timeout)
		defer cancel()
		if err := s.applyLocalDisks(ctx, request.Disks); err != nil {
			tmError(w, err, http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, map[string]string{"status": "applied"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func hasDiskSelection(cfg *config.Config) bool {
	for _, disk := range cfg.Disks() {
		if disk.Publish || disk.TimeMachine != nil && disk.TimeMachine.Enabled {
			return true
		}
	}
	return false
}

func findDisk(cfg *config.Config, uuid string) config.LocalDisk {
	for _, disk := range cfg.Disks() {
		if strings.EqualFold(disk.UUID, uuid) {
			return disk
		}
	}
	return config.LocalDisk{}
}

var diskName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,79}$`)

func localDirectory(volume timemachine.Volume) string {
	name := filepath.Base(volume.MountPoint)
	if filepath.Dir(volume.MountPoint) != "/tmp/mnt" || !diskName.MatchString(name) {
		name = strings.ToLower(volume.UUID)
	}
	if !diskName.MatchString(name) {
		hash := sha256.Sum256([]byte(strings.ToLower(volume.UUID)))
		name = fmt.Sprintf("%x", hash[:8])
	}
	return "local-" + name
}

func (s *Server) validateDiskLayout(cfg *config.Config) error {
	root := filepath.Clean(cfg.SMB.SharePath)
	if timemachine.Contains("/tmp/mnt", root) || timemachine.Contains(root, "/tmp/mnt") {
		return errors.New("aggregate is on USB storage or contains USB mount paths")
	}
	if !hasDiskSelection(cfg) {
		if cfg.TimeMachine != nil && cfg.TimeMachine.Enabled {
			return samba.ValidateTimeMachine(cfg, tmPath(cfg))
		}
		return nil
	}
	if err := s.validateAggregate(cfg); err != nil {
		return err
	}
	roots := map[string]bool{}
	for _, remote := range cfg.Remotes {
		roots[strings.ToLower(s.shares.Directory(remote))] = true
	}
	desired := s.remoteDirectories(cfg)
	for _, disk := range cfg.Disks() {
		if !disk.Publish {
			continue
		}
		name, ok := strings.CutPrefix(disk.Directory, "local-")
		if !ok || !diskName.MatchString(name) || roots[strings.ToLower(disk.Directory)] {
			return fmt.Errorf("USB publication path already exists: %s", disk.Directory)
		}
		roots[strings.ToLower(disk.Directory)] = true
		desired[disk.Directory] = true
	}
	// Render the ordinary and ACL shares first to account for Samba's generated
	// names, including suffixes used when ACL share names collide.
	ordinary := strings.ToLower(samba.RenderDisks(cfg, nil, desired))
	used := map[string]bool{}
	for _, disk := range cfg.Disks() {
		p := disk.TimeMachine
		if p == nil || !p.Enabled {
			continue
		}
		name := strings.ToLower(p.Share)
		if used[name] || strings.Contains(ordinary, "\n["+name+"]\n") {
			return errors.New("Time Machine share name is already in use")
		}
		used[name] = true
		projected := *cfg
		projected.TimeMachine = p
		if err := samba.ValidateTimeMachine(&projected, backupPath(cfg, disk.UUID)); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) openDiskBindings(ctx context.Context, cfg *config.Config, volumes []timemachine.Volume,
	edits map[string]localDiskEdit, migrating string) (diskBindings, map[string]string, error) {
	bindings := diskBindings{}
	status := map[string]string{}
	if !hasDiskSelection(cfg) {
		return bindings, status, nil
	}
	if err := s.validateDiskLayout(cfg); err != nil {
		for _, disk := range cfg.Disks() {
			status[strings.ToLower(disk.UUID)] = err.Error()
		}
		return bindings, status, err
	}
	if volumes == nil {
		var err error
		volumes, err = s.volumeInventory(ctx, cfg)
		if err != nil {
			for _, disk := range cfg.Disks() {
				status[strings.ToLower(disk.UUID)] = err.Error()
			}
			return bindings, status, err
		}
	}
	for _, disk := range cfg.Disks() {
		uuid := strings.ToLower(disk.UUID)
		edit, changed := edits[uuid]
		p := disk.TimeMachine
		backup := p != nil && p.Enabled
		if !disk.Publish && !backup {
			continue
		}
		var ordinaryErr, backupErr error
		if disk.Publish {
			var volume timemachine.Volume
			volume, ordinaryErr = timemachine.FindDisk(volumes, uuid, true)
			path := filepath.Join(cfg.SMB.SharePath, disk.Directory)
			if ordinaryErr == nil {
				ordinaryErr = s.checkPublicationPath(path, true)
			}
			if ordinaryErr == nil {
				var target *timemachine.Target
				target, ordinaryErr = timemachine.OpenTarget(volume, "", nil)
				if ordinaryErr == nil {
					bindings[path] = target
				}
			}
		}
		if backup {
			path := backupPath(cfg, uuid)
			backupErr = s.checkPublicationPath(path, false)
			if backupErr == nil {
				var target *timemachine.Target
				target, backupErr = s.openBackupTarget(ctx, cfg, disk, volumes, edit.TimeMachine, uuid == migrating)
				if target != nil {
					bindings[path] = target
				}
			}
		}
		if err := errors.Join(ordinaryErr, backupErr); err != nil {
			status[uuid] = err.Error()
			if changed {
				previous := findDisk(s.cfg, uuid)
				if ordinaryErr != nil && !previous.Publish || backupErr != nil && (edit.TimeMachine != nil && edit.TimeMachine.CreateFolder || !reflect.DeepEqual(previous.TimeMachine, p)) {
					return bindings, status, fmt.Errorf("%s: %w", uuid, err)
				}
			}
		}
	}
	return bindings, status, nil
}

func (s *Server) openBackupTarget(ctx context.Context, cfg *config.Config, disk config.LocalDisk,
	volumes []timemachine.Volume, edit *timeMachineRequest, changeOwner bool) (*timemachine.Target, error) {
	p := disk.TimeMachine
	volume, err := backupVolume(volumes, p)
	if err != nil {
		return nil, err
	}
	if err := samba.CheckAvailable(cfg); err != nil {
		return nil, err
	}
	path := backupPath(cfg, disk.UUID)
	previous := s.tm.bindings[path]
	if edit == nil && previous != nil {
		target, err := timemachine.OpenTarget(volume, p.Folder, nil)
		if err != nil {
			return nil, err
		}
		if target.MountID == previous.MountID && target.Inode == previous.Inode {
			if !p.ReadOnly && volume.Free == 0 {
				return target, errors.New("USB volume has no free space")
			}
			return target, nil
		}
		_ = target.Close()
	}
	if !p.ReadOnly && volume.Free == 0 {
		return nil, errors.New("USB volume has no free space")
	}
	projected := *cfg
	projected.TimeMachine = p
	identity, err := existingTimeMachineIdentity(ctx, &projected)
	if err != nil {
		return nil, err
	}
	var create *timemachine.Identity
	if edit != nil && edit.CreateFolder && p.Folder != "" {
		create = &identity
	}
	target, err := timemachine.OpenTarget(volume, p.Folder, create)
	if err != nil {
		return nil, err
	}
	if changeOwner {
		err = target.CheckOwnershipSupport(identity)
	} else {
		err = target.Probe(ctx, identity, p.ReadOnly)
		if err != nil && edit != nil && edit.Migration != nil && !p.ReadOnly {
			err = errOwnershipRequired
		}
	}
	if err != nil {
		removeErr := target.RemoveCreatedFolder()
		_ = target.Close()
		return nil, errors.Join(err, removeErr)
	}
	return target, nil
}

func (s *Server) applyLocalDisks(ctx context.Context, changes []localDiskEdit) (result error) {
	if len(changes) == 0 {
		return nil
	}
	old := s.cfg.Snapshot()
	next := s.cfg.Snapshot()
	next.LocalDisks, next.TimeMachine = next.Disks(), nil
	volumes, inventoryErr := s.volumeInventory(ctx, &old)
	edits := map[string]localDiskEdit{}
	seen := map[string]bool{}
	var migration *timeMachineRequest
	changeOwner := ""
	for _, edit := range changes {
		uuid := strings.ToLower(edit.UUID)
		if uuid == "" || seen[uuid] {
			return errors.New("invalid or duplicate USB volume UUID")
		}
		seen[uuid] = true
		previous := findDisk(&old, uuid)
		disk := previous
		disk.UUID, disk.Publish = uuid, edit.Publish
		if disk.Publish && disk.Directory == "" {
			if inventoryErr != nil {
				return inventoryErr
			}
			volume, err := timemachine.FindDisk(volumes, uuid, true)
			if err != nil {
				return err
			}
			disk.Directory = localDirectory(volume)
		}
		if req := edit.TimeMachine; req != nil {
			req.UUID = uuid
			if !req.Enabled {
				if p := disk.TimeMachine; p != nil {
					saved := *p
					saved.Enabled = false
					disk.TimeMachine = &saved
				}
			} else {
				legacyOnly := !previous.Publish && previous.TimeMachine != nil && previous.TimeMachine.Enabled
				if !disk.Publish && !legacyOnly && req.Migration == nil {
					return errors.New("publish the USB disk before enabling Time Machine")
				}
				if _, err := timeMachineAccount(&old, &req.TimeMachine); err != nil {
					return err
				}
				disk.TimeMachine = &req.TimeMachine
			}
			if req.Migration != nil {
				if migration != nil {
					return errors.New("only one manual Time Machine resource can be imported at a time")
				}
				if _, err := s.prepareTimeMachineMigration(ctx, &old, &next, req); err != nil {
					return err
				}
				migration = req
				if req.Migration.ChangeOwner {
					changeOwner = uuid
				}
			}
		}
		if previous.Publish && !disk.Publish && disk.TimeMachine != nil && disk.TimeMachine.Enabled {
			saved := *disk.TimeMachine
			saved.Enabled = false
			disk.TimeMachine = &saved
		}
		if previous.UUID == "" && !disk.Publish && disk.TimeMachine == nil {
			continue
		}
		if reflect.DeepEqual(previous, disk) && (edit.TimeMachine == nil || !edit.TimeMachine.CreateFolder && edit.TimeMachine.Migration == nil) {
			continue
		}
		if disk.Publish {
			next.SMB.AggregateDisabled = false
		}
		edits[uuid] = edit
		setDisk(&next, disk)
	}
	if len(edits) == 0 {
		return nil
	}
	for _, disk := range next.LocalDisks {
		if disk.Publish {
			next.SMB.AggregateDisabled = false
		}
	}
	if inventoryErr != nil {
		for uuid := range edits {
			disk, previous := findDisk(&next, uuid), findDisk(&old, uuid)
			if disk.Publish && !previous.Publish || disk.TimeMachine != nil && disk.TimeMachine.Enabled && !reflect.DeepEqual(disk.TimeMachine, previous.TimeMachine) {
				return inventoryErr
			}
		}
		volumes = []timemachine.Volume{}
	}
	bindings, statuses, err := s.openDiskBindings(ctx, &next, volumes, edits, changeOwner)
	if err != nil {
		bindings.close(true)
		return err
	}
	defer func() {
		if result != nil {
			bindings.close(true)
		}
	}()
	backups, local := s.diskShares(&next, bindings)
	if err := samba.ValidateCandidate(ctx, &next, backups, local); err != nil {
		return err
	}
	tx, err := tmTransaction(&old)
	if err != nil {
		return err
	}
	if err := tx.Prepare(); err != nil {
		return err
	}
	wasRunning := s.smb.Running()
	previousBindings := s.tm.bindings
	var stopped, touched, ownerChanged bool
	defer func() {
		if result == nil {
			return
		}
		rollback, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
		defer cancel()
		var rollbackErr error
		if stopped {
			rollbackErr = s.smb.StopInstalled(&next)
		}
		if rollbackErr == nil && ownerChanged {
			rollbackErr = applyTimeMachineOwnership(rollback, &old, bindings[backupPath(&next, changeOwner)], true)
		}
		if rollbackErr == nil && touched {
			rollbackErr = errors.Join(s.switchBindings(&old, previousBindings), tx.Restore())
			s.cfg.Replace(old)
		}
		if rollbackErr == nil && stopped && wasRunning {
			rollbackErr = s.smb.StartInstalled(rollback, &old)
		}
		if rollbackErr == nil {
			rollbackErr = tx.Complete()
		}
		if rollbackErr != nil {
			s.tm.recoveryError = rollbackErr.Error()
			result = errors.Join(result, rollbackErr)
		} else {
			_ = os.Remove(tmOwnershipPath(&old))
		}
		s.updateBonjour(rollback)
	}()
	if err := s.smb.StopInstalled(&old); err != nil {
		return err
	}
	stopped = true
	s.clearBonjour()
	if err := tx.Capture(); err != nil {
		return err
	}
	if err := removeLegacyDiskLinks(next.SMB.SharePath); err != nil {
		return err
	}
	if changeOwner != "" {
		p := migration.TimeMachine
		projected := next
		projected.TimeMachine = &p
		target := bindings[backupPath(&next, changeOwner)]
		identity, err := existingTimeMachineIdentity(ctx, &projected)
		if err != nil {
			return err
		}
		if err := saveTimeMachineOwnership(ctx, &projected, target, identity, migration.Migration.Bundle); err != nil {
			return err
		}
		ownerChanged = true
		if err := applyTimeMachineOwnership(ctx, &next, target, false); err != nil {
			return err
		}
		if err := target.Probe(ctx, identity, false); err != nil {
			return err
		}
	}
	installed := bindings
	if s.tm.paused || !next.SMB.Enabled {
		installed = nil
	}
	touched = true
	if err := s.switchBindings(&next, installed); err != nil {
		return err
	}
	if err := s.installation(ctx, &next, installed, false); err != nil {
		return err
	}
	for uuid, edit := range edits {
		if edit.TimeMachine == nil || installed[backupPath(&next, uuid)] == nil {
			continue
		}
		projected := next
		profile := *findDisk(&next, uuid).TimeMachine
		projected.TimeMachine = &profile
		password, err := timeMachineAccount(&old, projected.TimeMachine)
		if err != nil {
			return err
		}
		if password != "" && samba.ValidatePassword(password) == nil {
			if err := samba.CheckSMB(ctx, &projected, password); err != nil {
				return err
			}
		}
	}
	if err := s.cfg.Update(func(cfg *config.Config) error {
		cfg.LocalDisks, cfg.TimeMachine, cfg.SMB = next.LocalDisks, next.TimeMachine, next.SMB
		return nil
	}); err != nil {
		return err
	}
	if err := syncMigrationConfig(&next); err != nil {
		return err
	}
	if err := tx.Complete(); err != nil {
		return err
	}
	_ = os.Remove(tmOwnershipPath(&old))
	previousBindings.close(false)
	for _, target := range bindings {
		target.KeepCreatedFolder()
	}
	if installed == nil {
		bindings.close(false)
	}
	s.tm.bindings, s.tm.diskErrors = installed, statuses
	s.updateBonjour(ctx)
	return nil
}

func setDisk(cfg *config.Config, disk config.LocalDisk) {
	for i := range cfg.LocalDisks {
		if strings.EqualFold(cfg.LocalDisks[i].UUID, disk.UUID) {
			cfg.LocalDisks[i] = disk
			return
		}
	}
	cfg.LocalDisks = append(cfg.LocalDisks, disk)
}
