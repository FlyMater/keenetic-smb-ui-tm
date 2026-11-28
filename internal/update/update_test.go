package update

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

type testTransport func(*http.Request) (*http.Response, error)

func (fn testTransport) RoundTrip(r *http.Request) (*http.Response, error) { return fn(r) }

func responseClient(t *testing.T, wantURL string, code int, body string) *http.Client {
	t.Helper()
	return &http.Client{Transport: testTransport(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet || r.URL.String() != wantURL {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL)
		}
		return &http.Response{
			StatusCode: code,
			Status:     http.StatusText(code),
			Body:       io.NopCloser(strings.NewReader(body)),
		}, nil
	})}
}

func TestCheckProjectRelease(t *testing.T) {
	const asset = "keenetic-smb-ui_1.8.2_aarch64-3.10.ipk"
	const ownURL = "https://github.com/FlyMater/keenetic-smb-ui-tm/releases/download/v1.8.2/" + asset
	const foreignURL = "https://github.com/example/keenetic-smb-ui-tm/releases/download/v1.8.2/" + asset
	tests := []struct {
		name, current, url string
		status             int
		wantNewer, wantURL bool
		wantError          bool
	}{
		{name: "release from project repository", current: "1.8.1", url: ownURL, status: 200, wantNewer: true, wantURL: true},
		{name: "current version", current: "1.8.2", url: ownURL, status: 200},
		{name: "older version", current: "1.8.3", url: ownURL, status: 200},
		{name: "foreign asset rejected", current: "1.8.1", url: foreignURL, status: 200, wantNewer: true, wantError: true},
		{name: "no published release", current: "1.8.1", status: 404, wantError: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			body := fmt.Sprintf(`{"tag_name":"v1.8.2","assets":[
				{"name":"extra_%s","browser_download_url":%q,"size":42},
				{"name":%q,"browser_download_url":%q,"size":22}]}`, asset, ownURL, asset, tc.url)
			info := check(context.Background(), responseClient(t, repoAPI, tc.status, body), repoAPI, tc.current, "aarch64-3.10")
			if info.Repository != "FlyMater/keenetic-smb-ui-tm" || info.Newer != tc.wantNewer ||
				(info.URL == ownURL) != tc.wantURL || (info.Error != "") != tc.wantError {
				t.Errorf("unexpected result: %+v", info)
			}
		})
	}
}

func TestDownloadPackageVerifiesReleaseMetadata(t *testing.T) {
	const payload = "test package"
	const endpoint = "https://github.com/FlyMater/keenetic-smb-ui-tm/releases/download/v1.8.2/keenetic-smb-ui_1.8.2_aarch64-3.10.ipk"
	info := Info{Newer: true, URL: endpoint, size: int64(len(payload)), digest: fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(payload)))}
	for _, body := range []string{payload, "short", payload + " too long", "0" + payload[1:]} {
		var dst bytes.Buffer
		err := downloadPackage(context.Background(), responseClient(t, endpoint, 200, body), info, &dst)
		if body == payload {
			if err != nil || dst.String() != payload {
				t.Fatalf("valid package: %v", err)
			}
		} else if err == nil {
			t.Errorf("invalid package %q was accepted", body)
		}
	}
}
