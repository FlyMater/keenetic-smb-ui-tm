package web

import (
	"embed"
	"encoding/json"
	"log"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Locale files live next to the config so an operator can drop in a translation
// without rebuilding the binary:
//
//	/opt/etc/keenetic-smb-ui/locales/ru.json
//	/opt/etc/keenetic-smb-ui/locales/en.json
//
// A file is a flat map of Russian source strings to translated strings plus a
// "_label" key with the human-readable language name. Missing keys fall back to
// the original Russian text, so a partial translation is still usable.
//
//go:embed locales/*.json
var localeFS embed.FS

// localesDir returns where translations are materialised, next to config.json.
func (s *Server) localesDir() string {
	dir := filepath.Dir(s.cfg.Paths.ConfigFile)
	if dir == "." || dir == "" {
		dir = "/opt/etc/keenetic-smb-ui"
	}
	return filepath.Join(dir, "locales")
}

// materializeLocales writes the embedded defaults once, leaving user edits alone.
func (s *Server) materializeLocales() {
	dir := s.localesDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("locales: mkdir %s: %v", dir, err)
		return
	}
	entries, err := fs.ReadDir(localeFS, "locales")
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		embedded, err := localeFS.ReadFile("locales/" + e.Name())
		if err != nil {
			continue
		}
		var defaults map[string]any
		if err := json.Unmarshal(embedded, &defaults); err != nil {
			continue
		}
		dst := filepath.Join(dir, e.Name())
		merged := defaults
		if cur, err := os.ReadFile(dst); err == nil {
			var existing map[string]any
			if json.Unmarshal(cur, &existing) == nil {
				merged = existing
				added := 0
				for k, v := range defaults {
					if _, ok := merged[k]; !ok {
						merged[k] = v
						added++
					}
				}
				if added == 0 {
					continue // nothing new: keep the user's file byte-for-byte
				}
				log.Printf("locales: %s: added %d new keys", e.Name(), added)
			}
		}
		out, err := json.MarshalIndent(merged, "", "  ")
		if err != nil {
			continue
		}
		if err := os.WriteFile(dst, append(out, '\n'), 0o644); err != nil {
			log.Printf("locales: write %s: %v", dst, err)
		}
	}
}

// localesList scans the directory and returns code/label pairs for the UI.
func (s *Server) localesList() []map[string]string {
	dir := s.localesDir()
	entries, err := os.ReadDir(dir)
	if err != nil {
		return []map[string]string{{"code": "ru", "label": "Русский"}}
	}
	out := make([]map[string]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		code := strings.TrimSuffix(e.Name(), ".json")
		label := code
		if data, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
			var meta struct {
				Label string `json:"_label"`
			}
			if json.Unmarshal(data, &meta) == nil && meta.Label != "" {
				label = meta.Label
			}
		}
		out = append(out, map[string]string{"code": code, "label": label})
	}
	sort.Slice(out, func(i, j int) bool { return out[i]["code"] < out[j]["code"] })
	if len(out) == 0 {
		out = append(out, map[string]string{"code": "ru", "label": "Русский"})
	}
	return out
}

// handleLocales serves the list of available translations (public: needed on the
// login screen before authentication).
func (s *Server) handleLocales(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"languages": s.localesList(),
		"default":   "ru",
		"dir":       s.localesDir(),
	})
}

// handleLocale serves one dictionary by code.
func (s *Server) handleLocale(w http.ResponseWriter, r *http.Request) {
	code := strings.TrimPrefix(r.URL.Path, "/api/locales/")
	code = strings.TrimSuffix(code, ".json")
	if code == "" || strings.ContainsAny(code, "/\\.") || len(code) > 16 {
		http.Error(w, "bad locale", http.StatusBadRequest)
		return
	}
	path := filepath.Join(s.localesDir(), code+".json")
	data, err := os.ReadFile(path)
	if err != nil {
		// fall back to the embedded copy
		if data, err = localeFS.ReadFile("locales/" + code + ".json"); err != nil {
			http.Error(w, "unknown locale", http.StatusNotFound)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}
