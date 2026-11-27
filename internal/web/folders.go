package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/tommilevs/keenetic-smb-ui/internal/acl"
	"github.com/tommilevs/keenetic-smb-ui/internal/config"
	"github.com/tommilevs/keenetic-smb-ui/internal/samba"
)

// handleFolders lists folder ACLs (GET) or replaces them (POST). The UI also
// gets the effective access matrix so it can show inheritance results.
func (s *Server) handleFolders(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		users := make([]string, 0, len(s.cfg.SMB.Users))
		for _, u := range s.cfg.SMB.Users {
			if u.Enabled && u.Name != "" {
				users = append(users, u.Name)
			}
		}
		matrix := make(map[string]map[string]string, len(s.cfg.Folders))
		for _, f := range s.cfg.Folders {
			row := make(map[string]string, len(users))
			for _, u := range users {
				row[u] = string(acl.Effective(s.cfg.Folders, f.Path, u))
			}
			matrix[f.ID] = row
		}
		writeJSON(w, map[string]any{
			"folders": s.cfg.Folders,
			"users":   users,
			"matrix":  matrix,
		})
	case http.MethodPost:
		var payload struct {
			Folders []config.Folder `json:"folders"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		for i := range payload.Folders {
			payload.Folders[i].Path = acl.CleanPath(payload.Folders[i].Path)
			if payload.Folders[i].ID == "" {
				payload.Folders[i].ID = "f" + strings.ReplaceAll(acl.CleanPath(payload.Folders[i].Path), "/", "-")
				if payload.Folders[i].ID == "f" {
					payload.Folders[i].ID = "f-root"
				}
			}
		}
		if err := s.cfg.Update(func(c *config.Config) error {
			c.Folders = payload.Folders
			if tmEnabled(c) {
				return samba.ValidateTimeMachine(c, tmPath(c))
			}
			return nil
		}); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		ctx, cancel := contextWithTimeout(30 * time.Second)
		defer cancel()
		if err := s.reconfigureSamba(ctx, false); err != nil {
			writeJSON(w, map[string]any{"status": "saved", "warning": err.Error()})
			return
		}
		writeJSON(w, map[string]string{"status": "saved"})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}
