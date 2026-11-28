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
	target            *timemachine.Target
	bonjour           timemachine.Bonjour
	diskError         string
	bonjourError      string
	advertised        bool
	paused            bool
	applyError        string
	recoveryError     string
	ownershipRecovery bool
}

type timeMachineRequest struct {
	config.TimeMachine
	CreateFolder bool                  `json:"create_folder"`
	Migration    *timeMachineMigration `json:"migration,omitempty"`
}

func tmEnabled(cfg *config.Config) bool {
	return cfg.TimeMachine != nil && cfg.TimeMachine.Enabled
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

func validateTimeMachineAggregate(cfg *config.Config) error {
	return timemachine.ValidateAggregate(cfg.SMB.SharePath, tmDir(cfg),
		filepath.Dir(cfg.Paths.SambaConf), filepath.Dir(samba.PassDB))
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
		writeJSON(w, map[string]any{"profile": s.cfg.TimeMachine, "diagnostics": s.tmDiagnostics(),
			"users": timeMachineUsers(s.cfg), "legacy": s.timeMachineLegacyPreview(ctx)})
	case "/volumes", "/folders":
		volumes, err := timemachine.Volumes(ctx, tmPath(s.cfg))
		if err != nil {
			tmError(w, err, http.StatusServiceUnavailable)
			return
		}
		if path == "/volumes" {
			writeJSON(w, volumes)
			return
		}
		volume, err := timemachine.Find(volumes, r.URL.Query().Get("uuid"))
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
		writeJSON(w, s.tmDiagnostics())
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
			err = s.applyTimeMachine(applyCtx, request)
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
		writeJSON(w, map[string]any{"status": result, "diagnostics": s.tmDiagnostics()})
	}
}

func tmError(w http.ResponseWriter, err error, status int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	writeJSON(w, map[string]string{"error": err.Error()})
}

func (s *Server) tmDiagnostics() map[string]any {
	out := map[string]any{"smb": s.smb.Status(), "disk": "disabled", "bonjour": "disabled"}
	if s.tm.applyError != "" {
		out["apply_error"] = s.tm.applyError
	}
	if s.tm.recoveryError != "" {
		out["recovery_error"] = s.tm.recoveryError
	}
	if !tmEnabled(s.cfg) {
		return out
	}
	p := s.cfg.TimeMachine
	out["limit_gib"] = p.LimitGiB
	out["smb_url"] = (&url.URL{Scheme: "smb", Host: s.cfg.SMB.ListenIP, Path: "/" + p.Share}).String()
	out["disk"] = "waiting"
	out["permissions"] = "pending"
	out["xattr"] = "pending"
	if s.tm.target != nil {
		out["disk"], out["permissions"] = "ready", "verified"
		if p.ReadOnly {
			out["xattr"] = "skipped"
		} else {
			out["xattr"] = "verified"
		}
	}
	if s.tm.diskError != "" {
		out["disk_error"] = s.tm.diskError
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

func (s *Server) validateTimeMachine(ctx context.Context, next *config.Config, request timeMachineRequest) (timemachine.Volume, error) {
	if err := samba.ValidateTimeMachine(next, tmPath(next)); err != nil {
		return timemachine.Volume{}, err
	}
	if err := validateTimeMachineAggregate(next); err != nil {
		return timemachine.Volume{}, err
	}
	if err := samba.CheckAvailable(next); err != nil {
		return timemachine.Volume{}, err
	}
	volume, err := selectVolume(ctx, next)
	if err == nil && !request.ReadOnly && volume.Free == 0 {
		err = errors.New("USB volume has no free space")
	}
	return volume, err
}

func selectVolume(ctx context.Context, cfg *config.Config) (timemachine.Volume, error) {
	profile := cfg.TimeMachine
	volumes, err := timemachine.Volumes(ctx, tmPath(cfg))
	if err != nil {
		return timemachine.Volume{}, err
	}
	volume, err := timemachine.Find(volumes, profile.UUID)
	if err != nil {
		return timemachine.Volume{}, err
	}
	if !profile.ReadOnly && profile.LimitGiB > volume.Total/(1<<30) {
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
	if tmEnabled(cfg) && cfg.TimeMachine.SMBUser {
		if _, err := samba.TimeMachineSMBUser(cfg.SMB.Users, cfg.TimeMachine.User, cfg.TimeMachine.ReadOnly); err != nil {
			return errTimeMachineUserInUse
		}
	}
	return nil
}

func timeMachineAccount(old *config.Config, profile *config.TimeMachine) (string, error) {
	// Only a profile already on disk can retain its dedicated account.
	profile.SMBUser = true
	user, err := samba.TimeMachineSMBUser(old.SMB.Users, profile.User, profile.ReadOnly)
	if err == nil {
		profile.User = user.Name
		return user.Password, nil
	}
	if old.TimeMachine != nil && !old.TimeMachine.SMBUser && old.TimeMachine.User != "" &&
		strings.EqualFold(old.TimeMachine.User, profile.User) {
		profile.User, profile.SMBUser = old.TimeMachine.User, false
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
	next.TimeMachine = &request.TimeMachine
	password, err := timeMachineAccount(&old, next.TimeMachine)
	if err != nil {
		return "", err
	}
	volume, err := s.validateTimeMachine(ctx, &next, request)
	if err != nil {
		return "", err
	}
	identity, err := existingTimeMachineIdentity(ctx, &next)
	if err != nil {
		return "", err
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
	if err := samba.ValidateCandidate(ctx, &next, tmPath(&next)); err != nil {
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
	if old.TimeMachine != nil && *old.TimeMachine == request.TimeMachine && s.tm.target != nil && s.smb.Running() {
		if password == "" || samba.ValidatePassword(password) != nil {
			return "check_in_finder", nil
		}
		return "connected", samba.CheckSMB(ctx, &next, password)
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
