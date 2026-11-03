// Package securefile persists owner-private files using durable replacement.
package securefile

import (
	"io"
	"os"
	"path/filepath"
)

// WriteAtomic writes a complete owner-private file before replacing path. The
// parent directory must already exist. Failed writes leave the old file intact;
// a commit error can occur after replacement, so callers must fail closed rather
// than continue serving a previous in-memory version.
func WriteAtomic(path string, data []byte) error {
	return writeAtomic(path, data, replace)
}

// CreateAtomic publishes a complete owner-private file only if path is absent.
// A concurrent winner is reported as os.ErrExist; existing data is never replaced.
func CreateAtomic(path string, data []byte) error {
	return writeAtomic(path, data, publishNew)
}

func writeAtomic(path string, data []byte, publish func(string, string) error) error {
	f, err := createTemp(filepath.Dir(path))
	if err != nil {
		return err
	}
	name := f.Name()
	defer func() {
		_ = f.Close()
		_ = os.Remove(name)
	}()
	if n, err := f.Write(data); err != nil {
		return err
	} else if n != len(data) {
		return io.ErrShortWrite
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return publish(name, path)
}

// CheckPrivate verifies permissions on the opened file, rather than trusting
// mode bits that do not describe Windows access-control lists.
func CheckPrivate(f *os.File) error {
	return checkPrivate(f)
}
