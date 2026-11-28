package web

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/timemachine"
)

type timeMachineMigration struct {
	Bundle      string `json:"bundle"`
	ChangeOwner bool   `json:"change_owner"`
}

type legacyTimeMachine struct {
	Profile config.TimeMachine `json:"profile"`
	Host    string             `json:"host"`
	Target  string             `json:"target"`
	Bundles []string           `json:"bundles"`
}

var (
	errLegacyTimeMachine = errors.New("the manual Time Machine configuration cannot be migrated automatically")
	errOwnershipRequired = errors.New("legacy Time Machine requires write access or confirmed ownership transfer")
)

// recognizeLegacyTimeMachine only accepts the known standalone Time Capsule
// include. Unknown directives can affect security or other SMB shares.
func recognizeLegacyTimeMachine(r io.Reader, aggregate string) (path string, profile config.TimeMachine, err error) {
	scanner := bufio.NewScanner(r)
	var section string
	shares := make(map[string]map[string]string)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line[1 : len(line)-1]
			if shares[section] != nil {
				return "", profile, errLegacyTimeMachine
			}
			shares[section] = make(map[string]string)
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.ToLower(strings.Join(strings.Fields(key), " "))
		_, duplicate := shares[section][key]
		if !ok || shares[section] == nil || duplicate {
			return "", profile, errLegacyTimeMachine
		}
		shares[section][key] = strings.TrimSpace(value)
	}
	if err := scanner.Err(); err != nil {
		return "", profile, err
	}
	hidden := shares[aggregate]
	if len(shares) != 2 || len(hidden) != 2 || hidden["available"] != "no" || hidden["browseable"] != "no" {
		return "", profile, errLegacyTimeMachine
	}
	for name, options := range shares {
		if name == aggregate {
			continue
		}
		for key, value := range options {
			switch key {
			case "path", "valid users":
				continue
			case "browseable", "ea support", "fruit:time machine":
				if value == "yes" {
					continue
				}
			case "guest ok", "read only":
				if value == "no" {
					continue
				}
			case "vfs objects":
				if strings.Join(strings.Fields(value), " ") == "catia fruit streams_xattr" {
					continue
				}
			case "fruit:resource":
				if value == "file" {
					continue
				}
			case "force user":
				if value == "root" {
					continue
				}
			}
			return "", profile, errLegacyTimeMachine
		}
		path = options["path"]
		user := options["valid users"]
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.ContainsAny(path, "%\\") ||
			options["fruit:time machine"] != "yes" || options["vfs objects"] == "" ||
			options["read only"] != "no" || options["guest ok"] != "no" || len(strings.Fields(user)) != 1 {
			return "", profile, errLegacyTimeMachine
		}
		return path, config.TimeMachine{Enabled: true, Share: name, User: user, Bonjour: true}, nil
	}
	return "", profile, errLegacyTimeMachine
}

func (s *Server) legacyTimeMachine(ctx context.Context, cfg *config.Config) (*legacyTimeMachine, int, error) {
	if cfg.TimeMachine != nil {
		return nil, -1, nil
	}
	include := filepath.Join(filepath.Dir(cfg.Paths.ConfigFile), "timemachine.conf")
	index := -1
	for i, option := range cfg.SMB.ExtraOptions {
		key, value, ok := strings.Cut(option, "=")
		if !ok || !strings.EqualFold(strings.TrimSpace(key), "include") {
			continue
		}
		if index != -1 || strings.TrimSpace(value) != include {
			return nil, -1, errLegacyTimeMachine
		}
		index = i
	}
	if index == -1 {
		return nil, -1, nil
	}
	info, err := os.Lstat(include)
	if err != nil || !info.Mode().IsRegular() || info.Size() > 16*1024 {
		return nil, -1, errLegacyTimeMachine
	}
	file, err := os.Open(include)
	if err != nil {
		return nil, -1, err
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil || !os.SameFile(info, actual) {
		return nil, -1, errLegacyTimeMachine
	}
	path, profile, err := recognizeLegacyTimeMachine(io.LimitReader(file, 16*1024+1), cfg.SMB.ShareName)
	if err != nil {
		return nil, -1, err
	}
	volumes, err := s.volumeInventory(ctx, cfg)
	if err != nil {
		return nil, -1, err
	}
	var chosen timemachine.Volume
	for _, volume := range volumes {
		if volume.Reason == "" && timemachine.Contains(volume.MountPoint, path) &&
			len(volume.MountPoint) > len(chosen.MountPoint) {
			chosen = volume
		}
	}
	if chosen.UUID == "" {
		return nil, -1, errors.New("USB volume unavailable")
	}
	if findDisk(cfg, chosen.UUID).TimeMachine != nil {
		return nil, -1, nil
	}
	profile.UUID = chosen.UUID
	if path != chosen.MountPoint {
		profile.Folder, err = filepath.Rel(chosen.MountPoint, path)
		if err != nil {
			return nil, -1, err
		}
	}
	target, err := timemachine.OpenTarget(chosen, profile.Folder, nil)
	if err != nil {
		return nil, -1, err
	}
	defer target.Close()
	folders, err := target.Folders()
	if err != nil {
		return nil, -1, err
	}
	bundles := make([]string, 0, len(folders))
	for _, folder := range folders {
		if strings.HasSuffix(strings.ToLower(folder), ".sparsebundle") {
			bundles = append(bundles, folder)
		}
	}
	return &legacyTimeMachine{Profile: profile, Host: cfg.SMB.ListenIP, Target: path, Bundles: bundles}, index, nil
}

func (s *Server) timeMachineLegacyPreview(ctx context.Context) *legacyTimeMachine {
	migration, _, _ := s.legacyTimeMachine(ctx, s.cfg)
	return migration
}

func (s *Server) prepareTimeMachineMigration(ctx context.Context, old, next *config.Config, request *timeMachineRequest) (*legacyTimeMachine, error) {
	if request.Migration == nil {
		return nil, nil
	}
	preview, index, err := s.legacyTimeMachine(ctx, old)
	if err != nil {
		return nil, err
	}
	// Re-read the source on check/apply and bind consent to the same target.
	if preview == nil || !request.Enabled || !strings.EqualFold(request.UUID, preview.Profile.UUID) ||
		request.Folder != preview.Profile.Folder || request.Share != preview.Profile.Share ||
		request.User != preview.Profile.User || request.ReadOnly || request.CreateFolder {
		return nil, errors.New("manual Time Machine settings changed; reload the migration preview")
	}
	if request.Migration.ChangeOwner {
		found := len(preview.Bundles) == 0 && request.Migration.Bundle == ""
		for _, bundle := range preview.Bundles {
			found = found || bundle == request.Migration.Bundle
		}
		if !found {
			return nil, errors.New("select the backup bundle whose ownership may change")
		}
	}
	next.SMB.ExtraOptions = append(next.SMB.ExtraOptions[:index], next.SMB.ExtraOptions[index+1:]...)
	next.SMB.AggregateDisabled = true
	return preview, nil
}

// The ownership log lives beside the Samba transaction. Its first record
// identifies the USB directory across reboots, followed by the original owners.
type timeMachineOwnership struct {
	UUID   string
	Folder string
	Inode  uint64
	Owner  timemachine.Identity
}

func tmOwnershipPath(cfg *config.Config) string {
	return filepath.Join(tmDir(cfg), "transaction", "ownership")
}

func saveTimeMachineOwnership(ctx context.Context, cfg *config.Config, target *timemachine.Target, owner timemachine.Identity, bundle string) error {
	path := tmOwnershipPath(cfg)
	file, err := os.CreateTemp(filepath.Dir(path), ".ownership-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	header := timeMachineOwnership{cfg.TimeMachine.UUID, cfg.TimeMachine.Folder, target.Inode, owner}
	encErr := json.NewEncoder(file).Encode(header)
	if encErr == nil {
		encErr = target.SaveOwnership(ctx, bundle, file)
	}
	if err := errors.Join(encErr, file.Sync(), file.Close()); err != nil {
		return err
	}
	if err := os.Rename(file.Name(), path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func applyTimeMachineOwnership(ctx context.Context, cfg *config.Config, target *timemachine.Target, restore bool) error {
	file, err := os.Open(tmOwnershipPath(cfg))
	if restore && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	var header timeMachineOwnership
	if err := decoder.Decode(&header); err != nil {
		return err
	}
	if target == nil {
		volumes, err := timemachine.Volumes(ctx, tmPath(cfg))
		if err != nil {
			return err
		}
		volume, err := timemachine.Find(volumes, header.UUID)
		if err != nil {
			return err
		}
		target, err = timemachine.OpenTarget(volume, header.Folder, nil)
		if err != nil {
			return err
		}
		defer target.Close()
	}
	if target.Inode != header.Inode || header.Owner.UID == 0 || header.Owner.GID == 0 {
		return errors.New("backup directory no longer matches the ownership journal")
	}
	return target.ApplyOwnership(ctx, io.MultiReader(decoder.Buffered(), file), header.Owner, restore)
}

func syncMigrationConfig(cfg *config.Config) error {
	for _, path := range []string{cfg.Paths.SambaConf, cfg.Paths.ConfigFile} {
		for _, name := range []string{path, filepath.Dir(path)} {
			file, err := os.Open(name)
			if err != nil {
				return err
			}
			if err := errors.Join(file.Sync(), file.Close()); err != nil {
				return err
			}
		}
	}
	return nil
}
