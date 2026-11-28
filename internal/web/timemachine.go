package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/samba"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/timemachine"
)

type timeMachineRuntime struct {
	bindings      diskBindings
	mounts        []string
	diskErrors    map[string]string
	bonjour       timemachine.Bonjour
	bonjourError  string
	advertised    bool
	paused        bool
	applyError    string
	recoveryError string
}

type timeMachineRequest struct {
	config.TimeMachine
	CreateFolder bool                  `json:"create_folder"`
	Migration    *timeMachineMigration `json:"migration,omitempty"`
}

func tmEnabled(cfg *config.Config) bool {
	if cfg.TimeMachine != nil && cfg.TimeMachine.Enabled {
		return true
	}
	for _, disk := range cfg.LocalDisks {
		if disk.TimeMachine != nil && disk.TimeMachine.Enabled {
			return true
		}
	}
	return false
}

func tmDir(cfg *config.Config) string {
	dir := cfg.Paths.DataDir
	if dir == "" {
		dir = "/opt/var/keenetic-smb-ui"
	}
	return filepath.Join(dir, "timemachine")
}

func tmPath(cfg *config.Config) string {
	return filepath.Join(tmDir(cfg), "volume")
}

func tmTransaction(cfg *config.Config) (*samba.Transaction, error) {
	if err := os.MkdirAll(tmDir(cfg), 0755); err != nil {
		return nil, err
	}
	return samba.NewTransaction(filepath.Join(tmDir(cfg), "transaction"),
		cfg.Paths.ConfigFile, cfg.Paths.SambaConf, samba.PassDB)
}

func (s *Server) handleTimeMachine(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 25*time.Second)
	defer cancel()
	path := strings.TrimPrefix(r.URL.Path, "/api/time-machine")
	switch path {
	case "", "/volumes", "/folders", "/diagnostics":
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	case "/check", "/apply":
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
	default:
		http.NotFound(w, r)
		return
	}

	switch path {
	case "":
		writeJSON(w, map[string]any{"profile": s.cfg.TimeMachine, "diagnostics": s.tmDiagnostics(r.URL.Query().Get("uuid")),
			"users": timeMachineUsers(s.cfg), "legacy": s.timeMachineLegacyPreview(ctx)})
	case "/volumes", "/folders":
		volumes, err := s.volumeInventory(ctx, s.cfg)
		if err != nil {
			tmError(w, err, http.StatusServiceUnavailable)
			return
		}
		if path == "/volumes" {
			writeJSON(w, volumes)
			return
		}
		volume, err := timemachine.FindDisk(volumes, r.URL.Query().Get("uuid"), true)
		if err != nil {
			tmError(w, err, http.StatusUnprocessableEntity)
			return
		}
		target, err := timemachine.OpenTarget(volume, r.URL.Query().Get("path"), nil)
		if err != nil {
			tmError(w, err, http.StatusUnprocessableEntity)
			return
		}
		defer target.Close()
		folders, err := target.Folders()
		if err != nil {
			tmError(w, err, http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, folders)
	case "/diagnostics":
		writeJSON(w, s.tmDiagnostics(r.URL.Query().Get("uuid")))
	case "/check", "/apply":
		var request timeMachineRequest
		r.Body = http.MaxBytesReader(w, r.Body, 16*1024)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		err := decoder.Decode(&request)
		if err == nil && decoder.Decode(new(any)) != io.EOF {
			err = errors.New("trailing input")
		}
		if err != nil {
			tmError(w, errors.New("invalid Time Machine request"), http.StatusBadRequest)
			return
		}
		var result string
		if path == "/check" {
			result, err = s.checkTimeMachine(ctx, request)
		} else {
			// Complete a started application even if the browser closes.
			timeout := 45 * time.Second
			if request.Migration != nil {
				timeout = 15 * time.Minute
			}
			applyCtx, stop := context.WithTimeout(s.ctx, timeout)
			defer stop()
			disk := findDisk(s.cfg, request.UUID)
			if request.UUID == "" && s.cfg.TimeMachine != nil {
				request.UUID = s.cfg.TimeMachine.UUID
				disk = findDisk(s.cfg, request.UUID)
			}
			err = s.applyLocalDisks(applyCtx, []localDiskEdit{{UUID: request.UUID, Publish: disk.Publish, TimeMachine: &request}})
			s.tm.applyError = ""
			if err != nil {
				s.tm.applyError = err.Error()
			}
			result = "applied"
		}
		if err != nil {
			tmError(w, err, http.StatusUnprocessableEntity)
			return
		}
		writeJSON(w, map[string]any{"status": result, "diagnostics": s.tmDiagnostics(r.URL.Query().Get("uuid"))})
	}
}

func tmError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": err.Error()})
}

func (s *Server) tmDiagnostics(uuid string) map[string]any {
	out := map[string]any{"smb": s.smb.Status(), "disk": "disabled", "bonjour": "disabled"}
	if s.tm.applyError != "" {
		out["apply_error"] = s.tm.applyError
	}
	if s.tm.recoveryError != "" {
		out["recovery_error"] = s.tm.recoveryError
	}
	if uuid == "" && s.cfg.TimeMachine != nil {
		uuid = s.cfg.TimeMachine.UUID
	}
	disk := findDisk(s.cfg, uuid)
	out["published"] = disk.Publish && s.tm.bindings[filepath.Join(s.cfg.SMB.SharePath, disk.Directory)] != nil
	if err := s.tm.diskErrors[strings.ToLower(uuid)]; err != "" {
		out["disk_error"] = err
	}
	p := disk.TimeMachine
	if p == nil || !p.Enabled {
		return out
	}
	out["limit_gib"] = p.LimitGiB
	out["smb_url"] = (&url.URL{Scheme: "smb", Host: s.cfg.SMB.ListenIP, Path: "/" + p.Share}).String()
	out["disk"] = "waiting"
	out["permissions"] = "pending"
	out["xattr"] = "pending"
	if s.tm.bindings[backupPath(s.cfg, uuid)] != nil {
		out["disk"], out["permissions"] = "ready", "verified"
		if p.ReadOnly {
			out["xattr"] = "skipped"
		} else {
			out["xattr"] = "verified"
		}
	}
	if p.Bonjour && !p.ReadOnly {
		out["bonjour"] = "waiting"
		if s.tm.advertised {
			out["bonjour"] = "published"
		}
		if s.tm.bonjourError != "" {
			out["bonjour_error"] = s.tm.bonjourError
		}
	}
	return out
}

func backupVolume(volumes []timemachine.Volume, p *config.TimeMachine) (timemachine.Volume, error) {
	volume, err := timemachine.FindDisk(volumes, p.UUID, p.ReadOnly)
	if err != nil {
		return timemachine.Volume{}, err
	}
	if !p.ReadOnly && p.LimitGiB > volume.Total/(1<<30) {
		return timemachine.Volume{}, errors.New("backup limit exceeds USB volume capacity")
	}
	return volume, nil
}

type timeMachineUser struct {
	Name     string `json:"name"`
	ReadOnly bool   `json:"readonly"`
}

func timeMachineUsers(cfg *config.Config) []timeMachineUser {
	users := make([]timeMachineUser, 0, len(cfg.SMB.Users))
	for _, user := range cfg.SMB.Users {
		if _, err := samba.TimeMachineSMBUser(cfg.SMB.Users, user.Name, true); err == nil {
			users = append(users, timeMachineUser{user.Name, user.ReadOnly})
		}
	}
	return users
}

var errTimeMachineUserInUse = errors.New("change the Time Machine user or disable Time Machine before removing this SMB access")

func validateTimeMachineUserChange(cfg *config.Config) error {
	profiles := cfg.Disks()
	if cfg.TimeMachine != nil && cfg.TimeMachine.UUID == "" {
		profiles = append(profiles, config.LocalDisk{TimeMachine: cfg.TimeMachine})
	}
	for _, disk := range profiles {
		p := disk.TimeMachine
		if p != nil && p.Enabled && p.SMBUser {
			if _, err := samba.TimeMachineSMBUser(cfg.SMB.Users, p.User, p.ReadOnly); err != nil {
				return errTimeMachineUserInUse
			}
		}
	}
	return nil
}

func reservedTimeMachineUser(cfg *config.Config, name string) bool {
	if p := cfg.TimeMachine; p != nil && !p.SMBUser && strings.EqualFold(p.User, name) {
		return true
	}
	for _, disk := range cfg.LocalDisks {
		if p := disk.TimeMachine; p != nil && !p.SMBUser && strings.EqualFold(p.User, name) {
			return true
		}
	}
	return false
}

func timeMachineAccount(old *config.Config, profile *config.TimeMachine) (string, error) {
	// Only a profile already on disk can retain its dedicated account.
	profile.SMBUser = true
	user, err := samba.TimeMachineSMBUser(old.SMB.Users, profile.User, profile.ReadOnly)
	if err == nil {
		profile.User = user.Name
		return user.Password, nil
	}
	saved := findDisk(old, profile.UUID).TimeMachine
	if profile.UUID == "" {
		saved = old.TimeMachine
	}
	if saved != nil && !saved.SMBUser && saved.User != "" && strings.EqualFold(saved.User, profile.User) {
		profile.User, profile.SMBUser = saved.User, false
		return "", nil
	}
	return "", err
}

func (s *Server) checkTimeMachine(ctx context.Context, request timeMachineRequest) (string, error) {
	if !request.Enabled {
		return "", errors.New("enable Time Machine to check the destination")
	}
	old := s.cfg.Snapshot()
	next := s.cfg.Snapshot()
	if _, err := s.prepareTimeMachineMigration(ctx, &old, &next, &request); err != nil {
		return "", err
	}
	next.LocalDisks, next.TimeMachine = next.Disks(), nil
	password, err := timeMachineAccount(&old, &request.TimeMachine)
	if err != nil {
		return "", err
	}
	disk := findDisk(&next, request.UUID)
	disk.UUID, disk.TimeMachine = request.UUID, &request.TimeMachine
	setDisk(&next, disk)
	if err := s.validateDiskLayout(&next); err != nil {
		return "", err
	}
	if err := samba.CheckAvailable(&next); err != nil {
		return "", err
	}
	volumes, err := s.volumeInventory(ctx, &next)
	if err != nil {
		return "", err
	}
	volume, err := backupVolume(volumes, &request.TimeMachine)
	if err != nil {
		return "", err
	}
	projected := next
	projected.TimeMachine = &request.TimeMachine
	identity, err := existingTimeMachineIdentity(ctx, &projected)
	if err != nil {
		return "", err
	}
	if !request.ReadOnly && volume.Free == 0 {
		return "", errors.New("USB volume has no free space")
	}
	target, err := timemachine.OpenTarget(volume, request.Folder, nil)
	if errors.Is(err, os.ErrNotExist) && request.CreateFolder && request.Folder != "" {
		parent := filepath.Dir(request.Folder)
		if parent == "." {
			parent = ""
		}
		target, err = timemachine.OpenTarget(volume, parent, nil)
	}
	if err != nil {
		return "", err
	}
	defer target.Close()
	backups, local := s.diskShares(&next, s.tm.bindings)
	backups[strings.ToLower(request.UUID)] = backupPath(&next, request.UUID)
	if err := samba.ValidateCandidate(ctx, &next, backups, local); err != nil {
		return "", err
	}
	if err := target.Probe(ctx, identity, request.ReadOnly); err != nil {
		if request.Migration != nil && request.Migration.ChangeOwner {
			return "ownership_pending", nil
		}
		if request.Migration != nil && !request.ReadOnly {
			return "", errOwnershipRequired
		}
		return "", err
	}
	if request.Migration != nil && request.Migration.ChangeOwner {
		return "ownership_pending", nil
	}
	if saved := findDisk(&old, request.UUID).TimeMachine; saved != nil && *saved == request.TimeMachine && s.tm.bindings[backupPath(&next, request.UUID)] != nil && s.smb.Running() {
		if password == "" || samba.ValidatePassword(password) != nil {
			return "check_in_finder", nil
		}
		return "connected", samba.CheckSMB(ctx, &projected, password)
	}
	return "preflight", nil
}

func existingTimeMachineIdentity(ctx context.Context, cfg *config.Config) (timemachine.Identity, error) {
	name := cfg.TimeMachine.User
	if !samba.HasAccount(ctx, cfg.Paths.SambaConf, name) {
		return timemachine.Identity{}, errors.New("Time Machine SMB account is unavailable")
	}
	return samba.UnixIdentity(ctx, name)
}
