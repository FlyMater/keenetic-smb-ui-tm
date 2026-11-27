package timemachine

import (
	"context"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/godbus/dbus/v5"
)

const (
	avahi           = "org.freedesktop.Avahi"
	avahiGroup      = avahi + ".EntryGroup."
	ipv4Protocol    = int32(0)
	noReverseRecord = uint32(16)
)

type Announcement struct {
	Interface string
	IP        string
	Share     string
}

func (a Announcement) Host() string {
	return "keenetic-tm-" + strings.ReplaceAll(a.IP, ".", "-") + ".local"
}

type Bonjour struct {
	conn    *dbus.Conn
	group   dbus.BusObject
	current Announcement
}

func (b *Bonjour) Close() {
	if b.conn != nil {
		_ = b.conn.Close()
		b.conn, b.group, b.current = nil, nil, Announcement{}
	}
}

func (b *Bonjour) Publish(ctx context.Context, a Announcement) error {
	ctx, cancel := context.WithTimeout(ctx, 4*time.Second)
	defer cancel()
	if b.current == a && b.group != nil && b.established(ctx) == nil {
		return nil
	}
	b.Close()
	iface, err := net.InterfaceByName(a.Interface)
	if err != nil {
		return err
	}
	conn, err := connectAvahi(ctx)
	if err != nil {
		return fmt.Errorf("system D-Bus unavailable: %w", err)
	}
	b.conn = conn
	if err := b.publish(ctx, int32(iface.Index), a); err != nil {
		b.Close()
		return err
	}
	b.current = a
	return nil
}

func connectAvahi(ctx context.Context) (*dbus.Conn, error) {
	conn, err := dbus.SystemBusPrivate()
	if err != nil {
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if err := conn.Auth(nil); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := conn.Hello(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	return conn, nil
}

func (b *Bonjour) publish(ctx context.Context, index int32, a Announcement) error {
	var owner string
	if err := b.conn.BusObject().CallWithContext(ctx, "org.freedesktop.DBus.GetNameOwner", 0, avahi).Store(&owner); err != nil {
		return fmt.Errorf("system Avahi is not available on D-Bus: %w", err)
	}
	var path dbus.ObjectPath
	server := b.conn.Object(owner, "/")
	if err := server.CallWithContext(ctx, avahi+".Server.EntryGroupNew", dbus.FlagNoAutoStart).Store(&path); err != nil {
		return err
	}
	b.group = b.conn.Object(owner, path)
	call := func(method string, args ...any) error {
		return b.group.CallWithContext(ctx, avahiGroup+method, dbus.FlagNoAutoStart, args...).Err
	}
	host := a.Host()
	if err := call("AddAddress", index, ipv4Protocol, noReverseRecord, host, a.IP); err != nil {
		return err
	}
	instance := strings.TrimSuffix(host, ".local")
	if err := call("AddService", index, ipv4Protocol, uint32(0), instance,
		"_smb._tcp", "local", host, uint16(445), [][]byte{}); err != nil {
		return err
	}
	if err := call("AddService", index, ipv4Protocol, uint32(0), instance,
		"_adisk._tcp", "local", host, uint16(9),
		[][]byte{[]byte("sys=adVF=0x100"), []byte("dk0=adVN=" + a.Share + ",adVF=0x82")}); err != nil {
		return err
	}
	if err := call("Commit"); err != nil {
		return err
	}
	for {
		err := b.established(ctx)
		if !errors.Is(err, errRegistering) {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

var errRegistering = errors.New("Avahi registration pending")

func (b *Bonjour) established(ctx context.Context) error {
	var state int32
	if err := b.group.CallWithContext(ctx, avahiGroup+"GetState", dbus.FlagNoAutoStart).Store(&state); err != nil {
		return err
	}
	switch state {
	case 2:
		return nil
	case 0, 1:
		return errRegistering
	case 3:
		return errors.New("Avahi name collision")
	default:
		return errors.New("Avahi registration failed")
	}
}
