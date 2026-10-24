package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// The lock serializes cooperating CLI runs. External editors must still avoid
// concurrent writes: filesystem rename is atomic, but is not a compare-and-swap.
type teamsDeployment struct {
	path, lock, backup string
	original, expected []byte
	info               os.FileInfo
	changed            bool
	restored           bool
}

func lockTeamsDeployment(path string) (*teamsDeployment, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	lock := path + ".harness-refine.lock"
	f, err := os.OpenFile(lock, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, fmt.Errorf("acquire exclusive teams lock: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(lock)
		return nil, err
	}
	d := &teamsDeployment{path: path, lock: lock}
	d.info, err = os.Lstat(path)
	if err == nil && !d.info.Mode().IsRegular() {
		err = fmt.Errorf("teams path must be a regular file, not a symlink")
	}
	if err == nil {
		d.original, err = os.ReadFile(path)
	}
	if err != nil {
		d.close()
		return nil, err
	}
	d.expected = d.original
	d.backup = path + ".harness-refine.original"
	backup, err := os.OpenFile(d.backup, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		os.Remove(lock)
		return nil, fmt.Errorf("create original backup (existing backup needs operator review): %w", err)
	}
	if _, err = backup.Write(d.original); err == nil {
		err = backup.Sync()
	}
	closeErr := backup.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		os.Remove(lock)
		return nil, err
	}
	return d, nil
}

func (d *teamsDeployment) check() error {
	info, err := os.Lstat(d.path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || !os.SameFile(d.info, info) || info.Mode() != d.info.Mode() {
		return fmt.Errorf("service teams file changed externally; refusing to overwrite")
	}
	current, err := os.ReadFile(d.path)
	if err != nil {
		return err
	}
	if !bytes.Equal(current, d.expected) {
		return fmt.Errorf("service teams contents changed externally; refusing to overwrite")
	}
	return nil
}

func (d *teamsDeployment) replace(data []byte) error {
	if err := d.check(); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(d.path), ".harness-teams-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if err = f.Chmod(d.info.Mode().Perm()); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	newInfo, statErr := f.Stat()
	if err == nil {
		err = statErr
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := d.check(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), d.path); err != nil {
		return err
	}
	d.expected = append([]byte(nil), data...)
	d.changed = true
	d.info = newInfo
	return nil
}

func (d *teamsDeployment) deploy(candidate string) error {
	data, err := os.ReadFile(candidate)
	if err != nil {
		return err
	}
	return d.replace(data)
}

func (d *teamsDeployment) restore(url, key string, timeout time.Duration) error {
	if !d.changed {
		return nil
	}
	if err := d.replace(d.original); err != nil {
		return err
	}
	if err := reloadServiceTimeout(url, key, timeout); err != nil {
		return err
	}
	d.restored = true
	return nil
}

func (d *teamsDeployment) close() error {
	var backupErr error
	if d.backup != "" && (!d.changed || d.restored) {
		backupErr = os.Remove(d.backup)
	}
	lockErr := os.Remove(d.lock)
	if backupErr != nil {
		return backupErr
	}
	return lockErr
}
