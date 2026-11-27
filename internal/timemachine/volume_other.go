//go:build !linux

package timemachine

import (
	"context"
	"errors"
)

var errPlatform = errors.New("Time Machine requires Linux")

type Identity struct {
	UID, GID uint32
	Groups   []uint32
}
type Target struct {
	MountID string
	Inode   uint64
}

func Volumes(context.Context, string) ([]Volume, error)     { return nil, errPlatform }
func ValidateAggregate(string, ...string) error             { return errPlatform }
func OpenTarget(Volume, string, *Identity) (*Target, error) { return nil, errPlatform }
func (*Target) Close() error                                { return nil }
func (*Target) KeepCreatedFolder()                          {}
func (*Target) RemoveCreatedFolder() error                  { return nil }
func (*Target) Folders() ([]string, error)                  { return nil, errPlatform }
func (*Target) Bind(string) error                           { return errPlatform }
func Detach(string) error                                   { return nil }
func (*Target) Probe(context.Context, Identity, bool) error { return errPlatform }
func ProbeFile(bool) error                                  { return errPlatform }
