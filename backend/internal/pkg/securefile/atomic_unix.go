//go:build !windows

package securefile

import (
	"errors"
	"os"
	"path/filepath"
)

func createTemp(dir string) (*os.File, error) {
	return os.CreateTemp(dir, ".sub2api-*")
}

func replace(source, target string) error {
	if err := os.Rename(source, target); err != nil {
		return err
	}
	return syncParent(target)
}

func publishNew(source, target string) error {
	if err := os.Link(source, target); err != nil {
		return err
	}
	return syncParent(target)
}

func syncParent(target string) error {
	dir, err := os.Open(filepath.Dir(target))
	if err != nil {
		return err
	}
	return errors.Join(dir.Sync(), dir.Close())
}

func checkPrivate(f *os.File) error {
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return errors.New("expected owner-private regular file")
	}
	return nil
}
