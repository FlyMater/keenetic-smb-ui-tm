// Package update checks GitHub releases and installs a newer .ipk via opkg.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	repository = "FlyMater/keenetic-smb-ui-tm"
	repoAPI    = "https://api.github.com/repos/" + repository + "/releases/latest"
)

// Info describes the result of an update check.
type Info struct {
	Repository string `json:"repository"`
	Current    string `json:"current"`
	Latest     string `json:"latest"`
	URL        string `json:"url,omitempty"`
	Notes      string `json:"notes,omitempty"`
	Newer      bool   `json:"newer"`
	Error      string `json:"error,omitempty"`
	size       int64
	digest     string
}

type release struct {
	TagName string `json:"tag_name"`
	Body    string `json:"body"`
	Assets  []struct {
		Name   string `json:"name"`
		URL    string `json:"browser_download_url"`
		Size   int64  `json:"size"`
		Digest string `json:"digest"`
	} `json:"assets"`
}

// Check compares the running version with the latest GitHub release and picks
// the asset matching the package architecture (e.g. aarch64-3.10).
func Check(ctx context.Context, current, archSuffix string) Info {
	client := &http.Client{Timeout: 15 * time.Second}
	return check(ctx, client, repoAPI, current, archSuffix)
}

func check(ctx context.Context, client *http.Client, endpoint, current, archSuffix string) Info {
	info := Info{Repository: repository, Current: current}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "keenetic-smb-ui")
	resp, err := client.Do(req)
	if err != nil {
		info.Error = err.Error()
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		info.Error = "В репозитории пока нет опубликованных релизов"
		return info
	}
	if resp.StatusCode != http.StatusOK {
		info.Error = fmt.Sprintf("github api: %s", resp.Status)
		return info
	}
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		info.Error = err.Error()
		return info
	}
	if tag := versionRE.FindString(rel.TagName); tag == "" || tag != rel.TagName {
		info.Error = "Тег релиза должен быть в формате v1.2.3"
		return info
	}
	info.Latest = strings.TrimPrefix(rel.TagName, "v")
	info.Notes = rel.Body
	info.Newer = compare(current, info.Latest) < 0
	if info.Newer {
		name := "keenetic-smb-ui_" + info.Latest + "_" + archSuffix + ".ipk"
		for _, a := range rel.Assets {
			if a.Name == name && a.Size > 0 && releaseAssetURL(a.URL, rel.TagName, name) {
				info.URL, info.size, info.digest = a.URL, a.Size, a.Digest
				break
			}
		}
	}
	if info.Newer && info.URL == "" {
		info.Error = "в релизе нет пакета для этой архитектуры (" + archSuffix + ")"
	}
	return info
}

func releaseAssetURL(raw, tag, asset string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") ||
		u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	repoPath := "/" + repository + "/"
	return len(u.Path) >= len(repoPath) && strings.EqualFold(u.Path[:len(repoPath)], repoPath) &&
		u.Path[len(repoPath):] == "releases/download/"+tag+"/"+asset
}

var versionRE = regexp.MustCompile(`^v?(\d+)\.(\d+)\.(\d+)`)

func parseVersion(v string) (int, int, int) {
	m := versionRE.FindStringSubmatch(strings.TrimSpace(v))
	if m == nil {
		return 0, 0, 0
	}
	a, _ := strconv.Atoi(m[1])
	b, _ := strconv.Atoi(m[2])
	c, _ := strconv.Atoi(m[3])
	return a, b, c
}

func compare(a, b string) int {
	a1, a2, a3 := parseVersion(a)
	b1, b2, b3 := parseVersion(b)
	x := []int{a1, a2, a3}
	y := []int{b1, b2, b3}
	for i := range x {
		if x[i] < y[i] {
			return -1
		}
		if x[i] > y[i] {
			return 1
		}
	}
	return 0
}

// Install downloads the selected package before handing it to opkg. The
// package scripts restart the application, so opkg runs outside the request.
func Install(ctx context.Context, info Info) error {
	opkg, err := exec.LookPath("opkg")
	if err != nil {
		return err
	}
	pkg, err := os.CreateTemp("", "keenetic-smb-ui-update-*.ipk")
	if err != nil {
		return err
	}
	handedOff := false
	defer func() {
		if !handedOff {
			_ = os.Remove(pkg.Name())
		}
	}()
	client := &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 10 || req.URL.Scheme != "https" {
				return errors.New("недопустимое перенаправление при загрузке пакета")
			}
			return nil
		},
	}
	err = downloadPackage(ctx, client, info, pkg)
	if err := errors.Join(err, pkg.Close()); err != nil {
		return err
	}
	cmd := installCommand(opkg, pkg.Name())
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	handedOff = true
	go func() {
		if err := cmd.Wait(); err != nil {
			log.Printf("opkg update: %v", err)
		}
	}()
	return nil
}

func installCommand(opkg, pkg string) *exec.Cmd {
	// Keenetic's system shell drops positional arguments supplied after -c.
	cmd := exec.Command("/bin/sh", "-c", `
trap 'rm -f "$KEENETIC_UPDATE_PACKAGE"' EXIT
sleep 2
"$KEENETIC_UPDATE_OPKG" install "$KEENETIC_UPDATE_PACKAGE"
`)
	cmd.Env = append(cmd.Environ(), "KEENETIC_UPDATE_OPKG="+opkg, "KEENETIC_UPDATE_PACKAGE="+pkg)
	return cmd
}

func downloadPackage(ctx context.Context, client *http.Client, info Info, dst io.Writer) error {
	if !info.Newer || info.URL == "" || info.size <= 0 {
		return errors.New("пакет обновления не выбран")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, info.URL, nil)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("загрузка пакета: %s", resp.Status)
	}
	hash := sha256.New()
	size, err := io.Copy(io.MultiWriter(dst, hash), io.LimitReader(resp.Body, info.size))
	if err != nil {
		return err
	}
	extra, err := io.Copy(io.Discard, io.LimitReader(resp.Body, 1))
	if err != nil {
		return err
	}
	if size != info.size || extra != 0 {
		return errors.New("размер пакета не совпадает с данными релиза")
	}
	if info.digest != "" && !strings.EqualFold(info.digest, fmt.Sprintf("sha256:%x", hash.Sum(nil))) {
		return errors.New("контрольная сумма пакета не совпадает с данными релиза")
	}
	return nil
}
