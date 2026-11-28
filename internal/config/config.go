// Package config loads and persists the application configuration.
package config

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/acl"
)

// User is an SMB account served by the application's Samba instance.
type User struct {
	Name     string `json:"name"`
	Password string `json:"password,omitempty"`
	ReadOnly bool   `json:"readonly"`
	Enabled  bool   `json:"enabled"`
}

// Remote is an external share (usually a Proxmox host) that is mounted and
// re-exported inside the aggregated tree.
type Remote struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Host     string `json:"host"`
	Share    string `json:"share"`
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
	SubPath  string `json:"subpath,omitempty"`
	Enabled  bool   `json:"enabled"`
	ReadOnly bool   `json:"readonly"`
	Options  string `json:"options,omitempty"`
}

// SMB holds settings of the managed Samba instance.
type SMB struct {
	Enabled           bool     `json:"enabled"`
	ListenIP          string   `json:"listen_ip"`
	BindDevice        string   `json:"bind_device"`
	Workgroup         string   `json:"workgroup"`
	ServerString      string   `json:"server_string"`
	ShareName         string   `json:"share_name"`
	SharePath         string   `json:"share_path"`
	AggregateDisabled bool     `json:"aggregate_disabled,omitempty"`
	ShareReadOnly     bool     `json:"share_readonly"`
	ForceUser         bool     `json:"force_user"`
	Ports             string   `json:"ports"`
	MinProtocol       string   `json:"min_protocol"`
	MaxProtocol       string   `json:"max_protocol"`
	Users             []User   `json:"users"`
	GuestOK           bool     `json:"guest_ok"`
	ExtraOptions      []string `json:"extra_options,omitempty"`
}

// Web holds HTTP UI settings.
type Web struct {
	Listen   string `json:"listen"`
	User     string `json:"user"`
	Password string `json:"password"`
	ReadOnly bool   `json:"readonly"`
}

// Stats holds sampling preferences.
type Stats struct {
	IntervalSec int `json:"interval_sec"`
	HistoryLen  int `json:"history_len"`
}

// TimeMachine identifies one local backup destination and its Samba account.
type TimeMachine struct {
	Enabled  bool   `json:"enabled"`
	UUID     string `json:"uuid"`
	Folder   string `json:"folder"`
	Share    string `json:"share"`
	User     string `json:"user"`
	SMBUser  bool   `json:"smb_user,omitempty"`
	ReadOnly bool   `json:"readonly"`
	LimitGiB uint64 `json:"limit_gib"`
	Bonjour  bool   `json:"bonjour"`
}

// LocalDisk records an explicit publication choice for a USB filesystem.
type LocalDisk struct {
	UUID        string       `json:"uuid"`
	Directory   string       `json:"directory,omitempty"`
	Publish     bool         `json:"publish"`
	TimeMachine *TimeMachine `json:"time_machine,omitempty"`
}

// Disks includes the legacy singleton until the first save from the disk screen.
func (c *Config) Disks() []LocalDisk {
	if c.TimeMachine == nil || c.TimeMachine.UUID == "" {
		return c.LocalDisks
	}
	return append(append([]LocalDisk(nil), c.LocalDisks...), LocalDisk{
		UUID: c.TimeMachine.UUID, TimeMachine: c.TimeMachine,
	})
}

// Paths describes runtime locations (not user editable via UI).
type Paths struct {
	ConfigFile  string `json:"-"`
	DataDir     string `json:"-"`
	SambaConf   string `json:"samba_conf"`
	SambaPidDir string `json:"samba_pid_dir"`
}

// Folder is a directory inside the exported share with its own access list.
// The type lives in the acl package; this alias keeps configuration code short.
type Folder = acl.Folder

// Config is the root document persisted to disk.
type Config struct {
	Web         Web          `json:"web"`
	SMB         SMB          `json:"smb"`
	Remotes     []Remote     `json:"remotes"`
	Folders     []Folder     `json:"folders"`
	Stats       Stats        `json:"stats"`
	Paths       Paths        `json:"paths"`
	TimeMachine *TimeMachine `json:"time_machine,omitempty"`
	LocalDisks  []LocalDisk  `json:"local_disks,omitempty"`
}

// mu serialises Save/Update across goroutines without living inside Config
// (so Config stays copyable and JSON-encodable without vet warnings).
var mu sync.Mutex

// Default returns configuration suitable for a fresh install.
func Default() *Config {
	return &Config{
		Web: Web{
			Listen:   "0.0.0.0:8099",
			User:     "admin",
			Password: "1235467890",
		},
		SMB: SMB{
			Enabled:      true,
			ListenIP:     "192.168.160.2",
			BindDevice:   "br0",
			Workgroup:    "WORKGROUP",
			ServerString: "keenetic-smb-ui",
			ShareName:    "all",
			SharePath:    "/opt/var/keenetic-smb-ui/share",
			Ports:        "445",
			ForceUser:    true,
			MinProtocol:  "SMB2",
			MaxProtocol:  "SMB3_11",
			Users: []User{
				{Name: "admin", Password: "1235467890", Enabled: true},
			},
		},
		Stats: Stats{IntervalSec: 5, HistoryLen: 120},
		Paths: Paths{
			SambaConf:   "/opt/etc/keenetic-smb-ui/smb.conf",
			SambaPidDir: "/opt/var/keenetic-smb-ui/run",
		},
	}
}

// Load reads the configuration file; a missing file yields defaults.
func Load(path string) (*Config, error) {
	cfg := Default()
	cfg.Paths.ConfigFile = path
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		if err := cfg.Save(); err != nil {
			return nil, err
		}
		return cfg, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, err
	}
	// Configs written before force_user existed default to enabling it: without
	// it ordinary Samba users cannot write into the root-owned aggregated tree.
	if !strings.Contains(string(data), `"force_user"`) {
		cfg.SMB.ForceUser = true
	}
	cfg.normalize()
	return cfg, nil
}

func (c *Config) normalize() {
	def := Default()
	if c.Web.Listen == "" {
		c.Web.Listen = def.Web.Listen
	}
	if c.Web.User == "" {
		c.Web.User = def.Web.User
	}
	if c.SMB.ListenIP == "" {
		c.SMB.ListenIP = def.SMB.ListenIP
	}
	if c.SMB.ShareName == "" {
		c.SMB.ShareName = def.SMB.ShareName
	}
	if c.SMB.SharePath == "" {
		c.SMB.SharePath = def.SMB.SharePath
	}
	if c.SMB.Workgroup == "" {
		c.SMB.Workgroup = def.SMB.Workgroup
	}
	if c.SMB.MinProtocol == "" {
		c.SMB.MinProtocol = def.SMB.MinProtocol
	}
	if c.SMB.MaxProtocol == "" {
		c.SMB.MaxProtocol = def.SMB.MaxProtocol
	}
	if c.SMB.Ports == "" {
		c.SMB.Ports = def.SMB.Ports
	}
	if c.Paths.SambaConf == "" {
		c.Paths.SambaConf = def.Paths.SambaConf
	}
	if c.Paths.SambaPidDir == "" {
		c.Paths.SambaPidDir = def.Paths.SambaPidDir
	}
	if c.Stats.IntervalSec <= 0 {
		c.Stats.IntervalSec = def.Stats.IntervalSec
	}
	if c.Stats.HistoryLen <= 0 {
		c.Stats.HistoryLen = def.Stats.HistoryLen
	}
	if c.Paths.ConfigFile == "" {
		c.Paths.ConfigFile = def.Paths.ConfigFile
	}
}

// Save writes the configuration atomically.
func (c *Config) Save() error {
	mu.Lock()
	defer mu.Unlock()
	return c.saveLocked()
}

func (c *Config) saveLocked() error {
	path := c.Paths.ConfigFile
	if path == "" {
		path = "/opt/etc/keenetic-smb-ui/config.json"
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	// Keep a rolling copy of the previous file: a wiped or replaced config is
	// the difference between "disks disappeared" and a two-second restore.
	if old, err := os.ReadFile(path); err == nil && len(old) > 0 {
		_ = os.WriteFile(path+".bak-auto", old, 0o600)
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// Update applies fn to a copy of the config and persists it on success.
func (c *Config) Update(fn func(*Config) error) error {
	mu.Lock()
	defer mu.Unlock()
	snapshot := c.clone()
	if err := fn(&snapshot); err != nil {
		return err
	}
	snapshot.normalize()
	if err := snapshot.saveLocked(); err != nil {
		return err
	}
	*c = snapshot
	return nil
}

func (c *Config) Snapshot() Config {
	mu.Lock()
	defer mu.Unlock()
	return c.clone()
}

// Replace restores a configuration after the corresponding file was recovered.
func (c *Config) Replace(restored Config) {
	mu.Lock()
	defer mu.Unlock()
	*c = restored.clone()
}

func (c *Config) clone() Config {
	copy := *c
	copy.SMB.Users = append([]User(nil), c.SMB.Users...)
	copy.SMB.ExtraOptions = append([]string(nil), c.SMB.ExtraOptions...)
	copy.Remotes = append([]Remote(nil), c.Remotes...)
	copy.Folders = append([]Folder(nil), c.Folders...)
	copy.LocalDisks = append([]LocalDisk(nil), c.LocalDisks...)
	for i := range copy.LocalDisks {
		if profile := copy.LocalDisks[i].TimeMachine; profile != nil {
			value := *profile
			copy.LocalDisks[i].TimeMachine = &value
		}
	}
	if c.TimeMachine != nil {
		profile := *c.TimeMachine
		copy.TimeMachine = &profile
	}
	return copy
}
