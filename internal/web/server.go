// Package web serves the embedded web UI and its JSON API.
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/samba"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/shares"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/stats"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/timemachine"
)

//go:embed assets
var assets embed.FS

// Server bundles HTTP handlers with the domain managers.
type Server struct {
	operations sync.RWMutex
	tm         timeMachineRuntime
	cfg        *config.Config
	smb        *samba.Manager
	shares     *shares.Manager
	stats      *stats.Collector
	sessions   *sessions
	http       *http.Server
	ctx        context.Context
	cancel     context.CancelFunc
}

// New wires the server.
func New(cfg *config.Config) (*Server, error) {
	sh := shares.New(cfg)
	paths := []string{sh.Root()}
	for _, r := range cfg.Remotes {
		paths = append(paths, sh.MountPoint(r))
	}
	ctx, cancel := context.WithCancel(context.Background())
	s := &Server{
		cfg:      cfg,
		smb:      samba.New(cfg),
		shares:   sh,
		stats:    stats.New(cfg.Stats.HistoryLen, paths),
		sessions: newSessions(),
		ctx:      ctx,
		cancel:   cancel,
	}
	s.materializeLocales()
	startSizeWorker()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/locales", s.handleLocales)
	mux.HandleFunc("/api/locales/", s.handleLocale)
	mux.HandleFunc("/api/login", s.handleLogin)
	mux.HandleFunc("/api/logout", s.handleLogout)
	mux.HandleFunc("/api/session", s.handleSession)
	mux.HandleFunc("/api/status", s.auth(s.handleStatus))
	mux.HandleFunc("/api/config", s.auth(s.handleConfig))
	mux.HandleFunc("/api/settings", s.auth(s.handleSettings))
	mux.HandleFunc("/api/config/raw", s.auth(s.handleRawConfig))
	mux.HandleFunc("/api/service", s.auth(s.handleService))
	mux.HandleFunc("/api/remotes", s.auth(s.handleRemotes))
	mux.HandleFunc("/api/remotes/apply", s.auth(s.handleApply))
	mux.HandleFunc("/api/remotes/status", s.auth(s.handleRemoteStatus))
	mux.HandleFunc("/api/users", s.auth(s.handleUsers))
	mux.HandleFunc("/api/folders", s.auth(s.handleFolders))
	mux.HandleFunc("/api/fs", s.auth(s.handleFS))
	mux.HandleFunc("/api/fs/size", s.auth(s.handleFSSize))
	mux.HandleFunc("/api/fs/mkdir", s.auth(s.handleFSMkdir))
	mux.HandleFunc("/api/ndms/users", s.auth(s.handleNDMSUsers))
	mux.HandleFunc("/api/logs", s.auth(s.handleLogs))
	mux.HandleFunc("/api/update", s.auth(s.handleUpdate))
	mux.HandleFunc("/api/time-machine", s.auth(s.handleTimeMachine))
	mux.HandleFunc("/api/time-machine/", s.auth(s.handleTimeMachine))

	sub, err := fs.Sub(assets, "assets")
	if err != nil {
		return nil, err
	}
	mux.Handle("/", http.FileServer(http.FS(sub)))

	s.http = &http.Server{
		Addr:              cfg.Web.Listen,
		Handler:           logRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return s, nil
}

// Run starts the HTTP server.
func (s *Server) Run() error {
	ln, err := net.Listen("tcp", s.http.Addr)
	if err != nil {
		return err
	}
	log.Printf("web ui on %s", ln.Addr())
	err = s.http.Serve(ln)
	if err == http.ErrServerClosed {
		return nil
	}
	return err
}

// Shutdown stops the server gracefully.
func (s *Server) Shutdown(d time.Duration) error {
	ctx, cancel := contextWithTimeout(d)
	defer cancel()
	if s.cancel != nil {
		s.cancel()
	}
	err := s.http.Shutdown(ctx)
	s.operations.Lock()
	defer s.operations.Unlock()
	s.clearBonjour()
	if stopErr := s.smb.Stop(); stopErr != nil {
		return errors.Join(err, stopErr)
	}
	if s.tm.target != nil {
		_ = s.tm.target.Close()
		s.tm.target = nil
	}
	return errors.Join(err, timemachine.Detach(tmPath(s.cfg)))
}

// Bootstrap brings the managed stack up after start: IP alias, smb.conf,
// users, smbd and the aggregated mounts. Failures are logged, never fatal —
// the UI must stay reachable so the operator can fix things.
func (s *Server) Bootstrap() {
	s.operations.Lock()
	go func() {
		startStack := func() {
			if !s.cfg.SMB.Enabled {
				return
			}
			if _, bindErr := s.smb.EnsureBindIP(); bindErr != nil {
				log.Printf("bootstrap: bind ip: %v", bindErr)
			}
			ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
			defer cancel()
			if err := s.reconfigureSamba(ctx, true); err != nil {
				log.Printf("bootstrap: %v", err)
			}
		}
		watching := false
		watchShares := func() {
			if !watching && s.cfg.SMB.Enabled && s.tm.recoveryError == "" {
				watching = true
				go s.shares.Watch(s.ctx, 30*time.Second)
			}
		}
		if err := s.recoverTimeMachine(); err != nil {
			s.tm.recoveryError = err.Error()
			log.Printf("bootstrap: %v", err)
		} else {
			startStack()
			watchShares()
		}
		s.operations.Unlock()

		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-s.ctx.Done():
				return
			case <-ticker.C:
				s.operations.Lock()
				if s.tm.ownershipRecovery {
					if err := s.recoverTimeMachine(); err != nil {
						s.tm.recoveryError = err.Error()
					} else {
						s.tm.recoveryError = ""
						startStack()
						watchShares()
					}
					s.operations.Unlock()
					continue
				}
				ctx, cancel := context.WithTimeout(s.ctx, 20*time.Second)
				if err := s.reconcileTimeMachine(ctx, false, false); err != nil {
					log.Printf("Time Machine recovery: %v", err)
				}
				cancel()
				s.operations.Unlock()
			}
		}
	}()
}

func (s *Server) auth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authenticated(r) {
			w.Header().Set("WWW-Authenticate", `Basic realm="keenetic-smb-ui"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodGet {
			s.operations.RLock()
			defer s.operations.RUnlock()
		} else {
			s.operations.Lock()
			defer s.operations.Unlock()
		}
		if s.cfg.Web.ReadOnly && r.Method != http.MethodGet {
			http.Error(w, "read-only mode", http.StatusForbidden)
			return
		}
		if r.Method != http.MethodGet && s.tm.recoveryError != "" {
			tmError(w, errors.New(s.tm.recoveryError), http.StatusServiceUnavailable)
			return
		}
		next(w, r)
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	snap := s.stats.Sample()
	writeJSON(w, map[string]any{
		"service": s.smb.Status(),
		"smb_ip":  s.cfg.SMB.ListenIP,
		"share":   s.cfg.SMB.ShareName,
		"root":    s.shares.Root(),
		"stats":   snap,
		"remotes": s.shares.Statuses(),
		"version": Version,
	})
}

func (s *Server) handleConfig(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		safe := s.cfg.Snapshot()
		for i := range safe.SMB.Users {
			safe.SMB.Users[i].Password = ""
		}
		for i := range safe.Remotes {
			safe.Remotes[i].Password = ""
		}
		writeJSON(w, &safe)
	case http.MethodPost:
		var incoming config.Config
		if err := json.NewDecoder(r.Body).Decode(&incoming); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		err := s.cfg.Update(func(c *config.Config) error {
			incoming.Paths.ConfigFile = c.Paths.ConfigFile
			incoming.Paths.DataDir = c.Paths.DataDir
			if incoming.Paths.SambaConf == "" {
				incoming.Paths.SambaConf = c.Paths.SambaConf
			}
			if incoming.Paths.SambaPidDir == "" {
				incoming.Paths.SambaPidDir = c.Paths.SambaPidDir
			}
			incoming.TimeMachine = c.TimeMachine
			incoming.SMB.AggregateDisabled = c.SMB.AggregateDisabled
			if tmEnabled(c) {
				if incoming.Paths != c.Paths || incoming.SMB.ListenIP != c.SMB.ListenIP ||
					incoming.SMB.BindDevice != c.SMB.BindDevice || incoming.SMB.Ports != c.SMB.Ports {
					return errors.New("disable Time Machine before changing the Samba listen address or runtime paths")
				}
				if err := validateTimeMachineUserChange(&incoming); err != nil {
					return err
				}
				if err := samba.ValidateTimeMachine(&incoming, tmPath(&incoming)); err != nil {
					return err
				}
				if err := validateTimeMachineAggregate(&incoming); err != nil {
					return err
				}
			}
			for i := range incoming.SMB.Users {
				for _, old := range c.SMB.Users {
					if incoming.SMB.Users[i].Name == old.Name && incoming.SMB.Users[i].Password == "" {
						incoming.SMB.Users[i].Password = old.Password
					}
				}
			}
			for i := range incoming.Remotes {
				for _, old := range c.Remotes {
					if incoming.Remotes[i].ID == old.ID && incoming.Remotes[i].Password == "" {
						incoming.Remotes[i].Password = old.Password
					}
				}
			}
			if c.TimeMachine != nil && !c.TimeMachine.SMBUser {
				for _, user := range incoming.SMB.Users {
					if strings.EqualFold(user.Name, c.TimeMachine.User) {
						return errors.New("account is reserved for Time Machine")
					}
				}
			}
			*c = incoming
			return nil
		})
		if errors.Is(err, errTimeMachineUserInUse) {
			tmError(w, err, http.StatusConflict)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		writeJSON(w, map[string]string{"status": "saved"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var incoming struct {
		Web   *config.Web   `json:"web"`
		Stats *config.Stats `json:"stats"`
	}
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&incoming); err != nil || incoming.Web == nil || incoming.Stats == nil {
		http.Error(w, "expected web and stats settings", http.StatusBadRequest)
		return
	}
	if err := decoder.Decode(new(json.RawMessage)); err != io.EOF {
		http.Error(w, "expected a single settings object", http.StatusBadRequest)
		return
	}
	if err := s.cfg.Update(func(c *config.Config) error {
		if incoming.Web.Password == "" {
			incoming.Web.Password = c.Web.Password
		}
		c.Web = *incoming.Web
		c.Stats = *incoming.Stats
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "saved"})
}

func (s *Server) handleRawConfig(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	target := ""
	if s.tm.target != nil {
		target = tmPath(s.cfg)
	}
	if _, err := w.Write([]byte(samba.RenderWithTimeMachine(s.cfg, target))); err != nil {
		log.Printf("raw config write: %v", err)
	}
}

func (s *Server) handleService(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	action := r.URL.Query().Get("action")
	var err error
	msgs := map[string]string{}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	switch action {
	case "start", "restart":
		s.tm.paused = false
		if changed, e := s.smb.EnsureBindIP(); e != nil {
			err = e
		} else if changed {
			msgs["ip"] = "assigned " + s.cfg.SMB.ListenIP
		}
		if err == nil {
			err = s.reconfigureSamba(ctx, true)
		}
	case "stop":
		s.tm.paused = true
		s.clearBonjour()
		err = s.smb.Stop()
		if err == nil {
			if s.tm.target != nil {
				_ = s.tm.target.Close()
				s.tm.target = nil
			}
			err = timemachine.Detach(tmPath(s.cfg))
		}
	default:
		http.Error(w, "unknown action", http.StatusBadRequest)
		return
	}
	if err != nil {
		writeJSON(w, map[string]any{"status": "error", "error": err.Error(), "service": s.smb.Status()})
		return
	}
	writeJSON(w, map[string]any{"status": "ok", "service": s.smb.Status(), "info": msgs})
}

func (s *Server) handleRemotes(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var payload struct {
		Remotes []config.Remote `json:"remotes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := s.cfg.Update(func(c *config.Config) error {
		// keep passwords of entries that did not send one
		old := map[string]string{}
		for _, r := range c.Remotes {
			old[r.ID] = r.Password
		}
		for i := range payload.Remotes {
			if payload.Remotes[i].Password == "" {
				payload.Remotes[i].Password = old[payload.Remotes[i].ID]
			}
			if payload.Remotes[i].ID == "" {
				payload.Remotes[i].ID = fmt.Sprintf("r%d", time.Now().UnixNano())
			}
			payload.Remotes[i].Name = strings.TrimSpace(payload.Remotes[i].Name)
		}
		c.Remotes = payload.Remotes
		return nil
	}); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, map[string]string{"status": "saved"})
}

func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	errs := s.shares.Apply()
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := s.reconfigureSamba(ctx, false); err != nil {
		errs = append(errs, err)
	}
	paths := []string{s.shares.Root()}
	for _, rm := range s.cfg.Remotes {
		paths = append(paths, s.shares.MountPoint(rm))
	}
	s.stats.SetSharePaths(paths)
	msgs := make([]string, 0, len(errs))
	for _, e := range errs {
		msgs = append(msgs, e.Error())
	}
	writeJSON(w, map[string]any{"status": "applied", "errors": msgs})
}

func (s *Server) handleRemoteStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, s.shares.Statuses())
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var users []config.User
	if err := json.NewDecoder(r.Body).Decode(&users); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	err := s.cfg.Update(func(c *config.Config) error {
		old := map[string]string{}
		for _, u := range c.SMB.Users {
			old[u.Name] = u.Password
		}
		for i := range users {
			if c.TimeMachine != nil && !c.TimeMachine.SMBUser && strings.EqualFold(users[i].Name, c.TimeMachine.User) {
				return errors.New("account is reserved for Time Machine")
			}
			if users[i].Password == "" {
				users[i].Password = old[users[i].Name]
			}
		}
		c.SMB.Users = users
		if tmEnabled(c) {
			if err := validateTimeMachineUserChange(c); err != nil {
				return err
			}
			return samba.ValidateTimeMachine(c, tmPath(c))
		}
		return nil
	})
	if errors.Is(err, errTimeMachineUserInUse) {
		tmError(w, err, http.StatusConflict)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	if err := s.reconfigureSamba(ctx, true); err != nil {
		writeJSON(w, map[string]any{"status": "saved", "warning": err.Error()})
		return
	}
	writeJSON(w, map[string]string{"status": "saved"})
}

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	const logPath = "/opt/var/keenetic-smb-ui/log/samba.log"
	data, err := os.ReadFile(logPath)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "log not available: %v\n", err)
		return
	}
	const tail = 32768
	if len(data) > tail {
		data = data[len(data)-tail:]
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(data)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}

func logRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		// The UI is embedded in the binary and ships with every release; never
		// let a browser serve a stale bundle after an update.
		w.Header().Set("Cache-Control", "no-store, must-revalidate")
		next.ServeHTTP(w, r)
		log.Printf("%s %s %s", r.Method, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}
