package securefile_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/securefile"
	"github.com/Wei-Shaw/sub2api/internal/testutil/assertions"
	"github.com/stretchr/testify/require"
	"golang.org/x/sys/windows"
)

func TestWriteAtomicLockedTargetKeepsOldVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state")
	require.NoError(t, securefile.WriteAtomic(path, []byte("old")))
	name, err := windows.UTF16PtrFromString(path)
	require.NoError(t, err)
	handle, err := windows.CreateFile(name, windows.GENERIC_READ, 0, nil, windows.OPEN_EXISTING, 0, 0)
	require.NoError(t, err)
	func() {
		defer func() { require.NoError(t, windows.CloseHandle(handle)) }()
		require.Error(t, securefile.WriteAtomic(path, []byte("new")), "a failed commit must never be acknowledged")
	}()
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "old", string(actual))
	assertions.PrivateFile(t, path)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}
