package samba

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/FlyMater/keenetic-smb-ui-tm/internal/acl"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/config"
	"github.com/FlyMater/keenetic-smb-ui-tm/internal/timemachine"
)

const PassDB = "/opt/etc/samba/private/passdb.tdb"

var (
	numericPID = regexp.MustCompile(`^[1-9][0-9]+$`)
	safeUser   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,30}$`)
	safeShare  = regexp.MustCompile(`^[\p{L}\p{N}][\p{L}\p{N} _.-]{0,62}$`)
)

func legacyProtocol(protocol string) bool {
	switch strings.ToUpper(protocol) {
	case "CORE", "COREPLUS", "LANMAN1", "LANMAN2", "NT1":
		return true
	default:
		return false
	}
}

func ValidateTimeMachine(cfg *config.Config, target string) error {
	p := cfg.TimeMachine
	if p == nil || !p.Enabled {
		return nil
	}
	if !safeUser.MatchString(strings.ToLower(p.User)) || strings.EqualFold(p.User, "root") || strings.EqualFold(p.User, "nobody") {
		return errors.New("invalid dedicated Time Machine account name")
	}
	if !safeShare.MatchString(p.Share) || p.Share != strings.TrimSpace(p.Share) ||
		len(p.Share)+len("dk0=adVN=,adVF=0x82") > 255 {
		return errors.New("invalid Time Machine share name")
	}
	if !safeShare.MatchString(cfg.SMB.ShareName) {
		return errors.New("invalid aggregate share name")
	}
	if !cfg.SMB.Enabled {
		return errors.New("enable managed Samba before configuring Time Machine")
	}
	ip := net.ParseIP(cfg.SMB.ListenIP)
	if ip == nil || ip.To4() == nil || ip.IsUnspecified() || ip.IsLoopback() || ip.IsMulticast() ||
		strings.TrimSpace(cfg.SMB.Ports) != "445" {
		return errors.New("Time Machine requires a dedicated IPv4 address on TCP port 445")
	}
	switch strings.ToUpper(cfg.SMB.MinProtocol) {
	case "SMB2", "SMB2_02", "SMB2_10", "SMB3", "SMB3_00", "SMB3_02", "SMB3_11":
	default:
		if !legacyProtocol(cfg.SMB.MinProtocol) {
			return errors.New("unrecognized minimum Samba protocol")
		}
	}
	for _, s := range []string{cfg.SMB.Workgroup, cfg.SMB.ServerString, cfg.SMB.ShareName,
		cfg.SMB.SharePath, cfg.SMB.MaxProtocol, cfg.Paths.SambaConf, cfg.Paths.SambaPidDir, target} {
		if strings.ContainsAny(s, "\r\n\x00") || strings.Contains(s, "\\") {
			return errors.New("unsupported Samba setting in Time Machine mode")
		}
	}
	for _, path := range []string{cfg.SMB.SharePath, cfg.Paths.SambaConf, cfg.Paths.SambaPidDir, target} {
		if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.Contains(path, "%") {
			return errors.New("Time Machine requires absolute Samba paths without substitutions")
		}
	}
	if filepath.Base(cfg.SMB.SharePath) == "" || timemachine.Contains(cfg.SMB.SharePath, target) ||
		timemachine.Contains(target, cfg.SMB.SharePath) ||
		timemachine.Contains(cfg.SMB.SharePath, filepath.Dir(PassDB)) ||
		timemachine.Contains(cfg.SMB.SharePath, filepath.Dir(cfg.Paths.SambaConf)) {
		return errors.New("aggregated share overlaps private Samba state")
	}
	if _, err := timemachine.FolderParts(p.Folder); err != nil {
		return err
	}
	if p.SMBUser {
		if _, err := TimeMachineSMBUser(cfg.SMB.Users, p.User, p.ReadOnly); err != nil {
			return err
		}
	}
	for _, user := range cfg.SMB.Users {
		if !safeUser.MatchString(strings.ToLower(user.Name)) || (!p.SMBUser && strings.EqualFold(user.Name, p.User)) {
			return errors.New("Time Machine requires distinct and valid SMB account names")
		}
	}
	// The free-form global section may include other shares, substitute paths,
	// or override identity, protocol, access and passdb settings. Admit only
	// settings that cannot bypass the Time Machine isolation.
	for _, line := range cfg.SMB.ExtraOptions {
		key, value, ok := strings.Cut(line, "=")
		if !ok && strings.TrimSpace(line) == "" {
			continue
		}
		if strings.ContainsAny(value, "\r\n\\\x00") {
			return errors.New("invalid extra Samba option")
		}
		switch strings.Join(strings.Fields(strings.ToLower(key)), " ") {
		case "log level", "max log size", "kernel oplocks", "strict sync", "sync always",
			"server signing", "socket options", "aio read size", "aio write size", "veto files":
		default:
			return fmt.Errorf("extra Samba option is incompatible with Time Machine: %q", strings.TrimSpace(key))
		}
	}
	for _, reserved := range []string{"global", "ipc$", "homes", "printers", "print$", cfg.SMB.ShareName} {
		if strings.EqualFold(p.Share, reserved) {
			return errors.New("Time Machine share name is already in use")
		}
	}
	for _, folder := range cfg.Folders {
		rel := acl.CleanPath(folder.Path)
		if !folder.Enabled || rel == "" || LocalDirectory(rel) != "" {
			continue
		}
		if rel == ".." || strings.HasPrefix(rel, "../") || strings.ContainsAny(rel, "\r\n\\%\x00") {
			return errors.New("invalid path in existing folder ACL")
		}
		if !safeShare.MatchString(acl.ShareName(folder)) || strings.EqualFold(p.Share, acl.ShareName(folder)) {
			return errors.New("invalid or conflicting name in existing folder ACL")
		}
		for path := filepath.Join(cfg.SMB.SharePath, rel); path != cfg.SMB.SharePath; path = filepath.Dir(path) {
			if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
				return errors.New("existing folder ACL points through a symbolic link")
			} else if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	if strings.Contains(strings.ToLower(RenderDisks(cfg, nil, nil)), "\n["+strings.ToLower(p.Share)+"]\n") {
		return errors.New("Time Machine share name is already in use")
	}
	return nil
}

// TimeMachineSMBUser resolves a single configured account and enforces its access.
func TimeMachineSMBUser(users []config.User, name string, readOnly bool) (config.User, error) {
	var match config.User
	for _, user := range users {
		if !strings.EqualFold(user.Name, name) {
			continue
		}
		if match.Name != "" {
			return config.User{}, errors.New("Time Machine SMB account name is ambiguous")
		}
		match = user
	}
	if !safeUser.MatchString(strings.ToLower(match.Name)) || strings.EqualFold(match.Name, "root") ||
		strings.EqualFold(match.Name, "nobody") || !match.Enabled {
		return config.User{}, errors.New("select an enabled SMB user for Time Machine")
	}
	if match.ReadOnly && !readOnly {
		return config.User{}, errors.New("Time Machine requires an SMB user with write access")
	}
	return match, nil
}

func ValidatePassword(password string) error {
	// Samba's PASSWD_FD credential reader accepts at most 127 bytes.
	if len(password) > 127 || strings.ContainsAny(password, "\r\n\x00") ||
		password != strings.TrimSpace(password) {
		return errors.New("invalid Time Machine password")
	}
	return nil
}

func ValidateCandidate(ctx context.Context, cfg *config.Config, targets map[string]string, local map[string]bool) error {
	f, err := os.CreateTemp("", "keenetic-smb-testparm-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString(RenderDisks(cfg, targets, local))
	closeErr := f.Close()
	if err := errors.Join(err, closeErr); err != nil {
		return err
	}
	if out, err := exec.CommandContext(ctx, "testparm", "-s", f.Name()).CombinedOutput(); err != nil {
		return fmt.Errorf("testparm: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func HasAccount(ctx context.Context, conf, name string) bool {
	out, err := exec.CommandContext(ctx, "pdbedit", "-s", conf, "-L", "-u", name).Output()
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(out), "\n") {
		user, _, ok := strings.Cut(line, ":")
		if ok && strings.EqualFold(user, name) {
			return true
		}
	}
	return false
}

func SetPassword(ctx context.Context, conf, name, password string) error {
	cmd := exec.CommandContext(ctx, "smbpasswd", "-c", conf, "-a", "-s", name)
	cmd.Stdin = strings.NewReader(password + "\n" + password + "\n")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("cannot set Time Machine password: %w", err)
	}
	return nil
}

func UnixIdentity(ctx context.Context, name string) (timemachine.Identity, error) {
	var id timemachine.Identity
	values := []*uint32{&id.UID, &id.GID}
	for i, flag := range []string{"-u", "-g"} {
		out, err := exec.CommandContext(ctx, "id", flag, name).Output()
		if err != nil {
			return id, fmt.Errorf("cannot resolve Unix account %q: %w", name, err)
		}
		n, err := strconv.ParseUint(strings.TrimSpace(string(out)), 10, 32)
		if err != nil || n == 0 {
			return id, errors.New("Time Machine account must have a non-root UID and GID")
		}
		*values[i] = uint32(n)
	}
	out, err := exec.CommandContext(ctx, "id", "-G", name).Output()
	if err != nil {
		return id, err
	}
	for _, s := range strings.Fields(string(out)) {
		n, err := strconv.ParseUint(s, 10, 32)
		if err != nil || n == 0 {
			return id, errors.New("Time Machine account must not belong to the root group")
		}
		id.Groups = append(id.Groups, uint32(n))
	}
	return id, nil
}

func UnixExists(ctx context.Context, name string) bool {
	return exec.CommandContext(ctx, "id", "-u", name).Run() == nil
}

func DeleteUnix(ctx context.Context, name string) error {
	return exec.CommandContext(ctx, "deluser", name).Run()
}

func CheckListener(ctx context.Context, ip string) error {
	dialer := net.Dialer{Timeout: 300 * time.Millisecond}
	for i := 0; i < 10; i++ {
		con, err := dialer.DialContext(ctx, "tcp4", net.JoinHostPort(ip, "445"))
		if err == nil {
			return con.Close()
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return errors.New("managed Samba is not listening on the configured IP and port")
}

func CheckAvailable(cfg *config.Config) error {
	iface, err := net.InterfaceByName(cfg.SMB.BindDevice)
	if err != nil {
		return err
	}
	addrs, err := iface.Addrs()
	if err != nil {
		return err
	}
	hasDedicatedIP := false
	for _, addr := range addrs {
		ip, network, err := net.ParseCIDR(addr.String())
		if err == nil && ip.Equal(net.ParseIP(cfg.SMB.ListenIP)) {
			ones, bits := network.Mask.Size()
			hasDedicatedIP = ones == 32 && bits == 32
		}
	}
	if !hasDedicatedIP {
		return errors.New("configured Samba IP is not assigned as a dedicated /32 address on the LAN interface")
	}
	return nil
}

func EnsurePortFree(ip string) error {
	listener, err := net.Listen("tcp4", net.JoinHostPort(ip, "445"))
	if err != nil {
		return fmt.Errorf("cannot bind the managed Samba IP on TCP 445: %w", err)
	}
	return listener.Close()
}

func CheckSMB(ctx context.Context, cfg *config.Config, password string) error {
	p := cfg.TimeMachine
	run := func(command string) error {
		r, w, err := os.Pipe()
		if err != nil {
			return err
		}
		defer r.Close()
		if _, err := fmt.Fprintln(w, password); err != nil {
			_ = w.Close()
			return err
		}
		if err := w.Close(); err != nil {
			return err
		}
		cmd := exec.CommandContext(ctx, "smbclient", "//"+cfg.SMB.ListenIP+"/"+p.Share,
			"-I", cfg.SMB.ListenIP, "-p", "445", "-U", p.User, "--use-kerberos=off", "-c", command)
		for _, entry := range os.Environ() {
			if !strings.HasPrefix(entry, "PASSWD=") && !strings.HasPrefix(entry, "PASSWD_FILE=") &&
				!strings.HasPrefix(entry, "PASSWD_FD=") {
				cmd.Env = append(cmd.Env, entry)
			}
		}
		cmd.Env = append(cmd.Env, "PASSWD_FD=3")
		cmd.ExtraFiles = []*os.File{r}
		// smbclient diagnostics can contain credential material. Report only
		// the stage that failed to the caller.
		return cmd.Run()
	}
	if err := run("ls"); err != nil {
		return fmt.Errorf("SMB authentication or directory listing failed: %w", err)
	}
	if p.ReadOnly {
		return nil
	}
	f, err := os.CreateTemp("", ".keenetic-tm-check-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.WriteString("Keenetic Time Machine check\n")
	if err := errors.Join(err, f.Close()); err != nil {
		return err
	}
	name := filepath.Base(f.Name())
	if err := run("mkdir " + name); err != nil {
		return fmt.Errorf("SMB cannot create a test directory: %w", err)
	}
	remoteFile := name + `\probe`
	if err := run(fmt.Sprintf("put %q %s", f.Name(), remoteFile)); err != nil {
		_ = run("del " + remoteFile)
		_ = run("rmdir " + name)
		return fmt.Errorf("SMB write failed: %w", err)
	}
	if err := errors.Join(run("del "+remoteFile), run("rmdir "+name)); err != nil {
		return fmt.Errorf("SMB test file cleanup failed: %w", err)
	}
	return nil
}
