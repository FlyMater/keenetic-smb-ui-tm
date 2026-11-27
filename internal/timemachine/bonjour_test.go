package timemachine

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBonjourUsesDedicatedAddress(t *testing.T) {
	a := Announcement{IP: "192.168.160.2", Share: "Mac Studio"}
	if got := a.Host(); got != "keenetic-tm-192-168-160-2.local" {
		t.Fatalf("unexpected Bonjour host %q", got)
	}
}

func TestBonjourPublication(t *testing.T) {
	iface, ip := os.Getenv("KEENETIC_TEST_AVAHI_IF"), os.Getenv("KEENETIC_TEST_AVAHI_IP")
	if iface == "" || ip == "" {
		t.Skip("requires an isolated D-Bus and Avahi")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	var b Bonjour
	defer b.Close()
	a := Announcement{Interface: iface, IP: ip, Share: "Mac Backup"}
	if err := b.Publish(ctx, a); err != nil {
		t.Fatal(err)
	}
	for _, service := range []string{"_smb._tcp", "_adisk._tcp"} {
		out, err := exec.CommandContext(ctx, "avahi-browse", "-prt", service).CombinedOutput()
		if err != nil || !strings.Contains(string(out), a.Host()+";"+ip+";") {
			t.Fatalf("%s did not resolve to the configured IP: %v: %s", service, err, out)
		}
		if service == "_adisk._tcp" && !strings.Contains(string(out), "adVN=Mac Backup") {
			t.Fatalf("missing backup share in %s: %s", service, out)
		}
	}
}

func TestBonjourUnavailable(t *testing.T) {
	ifaces, err := net.Interfaces()
	if err != nil || len(ifaces) == 0 {
		t.Skip("no network interfaces")
	}
	t.Setenv("DBUS_SYSTEM_BUS_ADDRESS", "unix:path="+filepath.Join(t.TempDir(), "missing-socket"))
	var b Bonjour
	err = b.Publish(context.Background(), Announcement{Interface: ifaces[0].Name, IP: "192.168.160.2", Share: "Mac"})
	if err == nil || !strings.Contains(err.Error(), "D-Bus") {
		t.Fatalf("expected actionable D-Bus error, got %v", err)
	}
	if b.group != nil || b.conn != nil {
		t.Fatal("failed publication retained an active D-Bus group")
	}
}
