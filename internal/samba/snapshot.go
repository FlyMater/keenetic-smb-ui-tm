package samba

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

func writeAtomic(path string, data []byte) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".keenetic-smb-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	_, writeErr := file.Write(data)
	err = errors.Join(writeErr, file.Sync(), file.Close())
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

// Transaction persists a recovery point before touching installed files. The
// journal is retained if any restoration step fails.
type Transaction struct {
	dir   string
	paths []string
}

func NewTransaction(dir string, paths ...string) (*Transaction, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	return &Transaction{dir: dir, paths: paths}, nil
}

// Prepare journals account creation while the existing smbd is still running.
func (t *Transaction) Prepare() error {
	pending := filepath.Join(t.dir, "pending")
	if _, err := os.Stat(pending); err == nil {
		return errors.New("an unfinished Time Machine transaction requires recovery")
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.Remove(filepath.Join(t.dir, "account")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return writeAtomic(pending, []byte("null"))
}

// Capture replaces the preparation marker after smbd has stopped, so the
// passdb backup is taken with no live writers.
func (t *Transaction) Capture() error {
	pending := filepath.Join(t.dir, "pending")
	data, err := os.ReadFile(pending)
	if err != nil {
		return err
	}
	if string(data) != "null" {
		return errors.New("an unfinished Time Machine transaction requires recovery")
	}
	existed := make([]bool, len(t.paths))
	for i, path := range t.paths {
		marker := t.snapshot(i)
		data, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if err := writeAtomic(marker, data); err != nil {
			return err
		}
		existed[i] = true
	}
	data, err = json.Marshal(existed)
	if err != nil {
		return err
	}
	return writeAtomic(pending, data)
}

func (t *Transaction) snapshot(i int) string {
	return filepath.Join(t.dir, fmt.Sprintf("backup-%d", i))
}

func (t *Transaction) Pending() bool {
	_, err := os.Stat(filepath.Join(t.dir, "pending"))
	return err == nil
}

func (t *Transaction) Restore() error {
	data, err := os.ReadFile(filepath.Join(t.dir, "pending"))
	if err != nil {
		return err
	}
	if string(data) == "null" {
		return nil
	}
	var existed []bool
	if err := json.Unmarshal(data, &existed); err != nil || len(existed) != len(t.paths) {
		return errors.New("invalid Time Machine recovery manifest")
	}
	// Read all required backups before replacing any installed file.
	backups := make([][]byte, len(t.paths))
	for i := range t.paths {
		if existed[i] {
			backups[i], err = os.ReadFile(t.snapshot(i))
			if err != nil {
				return err
			}
		}
	}
	var errs []error
	for i, path := range t.paths {
		if existed[i] {
			err = writeAtomic(path, backups[i])
		} else {
			err = os.Remove(path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("restore %s: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func (t *Transaction) RecordNewAccount(name string) error {
	if !safeUser.MatchString(name) || name == "root" || name == "nobody" {
		return errors.New("invalid Time Machine recovery account")
	}
	return writeAtomic(filepath.Join(t.dir, "account"), []byte(name))
}

func (t *Transaction) NewAccount() (string, error) {
	data, err := os.ReadFile(filepath.Join(t.dir, "account"))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	name := string(data)
	if !safeUser.MatchString(name) || name == "root" || name == "nobody" {
		return "", errors.New("invalid Time Machine recovery account")
	}
	return name, nil
}

func (t *Transaction) Complete() error {
	pending := filepath.Join(t.dir, "pending")
	committed := filepath.Join(t.dir, "committed")
	dir, err := os.Open(t.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	if err := os.Rename(pending, committed); err != nil {
		return err
	}
	if err := dir.Sync(); err != nil {
		return errors.Join(err, os.Rename(committed, pending))
	}
	for i := range t.paths {
		_ = os.Remove(t.snapshot(i))
	}
	_ = os.Remove(filepath.Join(t.dir, "account"))
	_ = os.Remove(committed)
	return nil
}
