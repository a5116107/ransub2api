package securefile_test

import (
	"bytes"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/securefile"
	"github.com/Wei-Shaw/sub2api/internal/testutil/assertions"
	"github.com/stretchr/testify/require"
)

func TestCreateAtomicConcurrentWritersNeverReplaceWinner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "key")
	const writers = 16
	results := make(chan error, writers)
	var wg sync.WaitGroup
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results <- securefile.CreateAtomic(path, bytes.Repeat([]byte{byte(i)}, 65))
		}()
	}
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			require.ErrorIs(t, err, os.ErrExist)
		}
	}
	require.Equal(t, 1, winners)
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Len(t, actual, 65)
	require.Equal(t, bytes.Repeat(actual[:1], 65), actual)
	assertions.PrivateFile(t, path)
	require.ErrorIs(t, securefile.CreateAtomic(path, []byte("replacement")), os.ErrExist)
	after, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, actual, after)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1)
}

func TestCheckPrivateValidatesOpenedFilePermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "key")
	require.NoError(t, securefile.CreateAtomic(path, []byte("private")))
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	require.NoError(t, securefile.CheckPrivate(f))
	exposed := filepath.Join(t.TempDir(), "exposed")
	require.NoError(t, os.WriteFile(exposed, []byte("exposed"), 0o644))
	g, err := os.Open(exposed)
	require.NoError(t, err)
	t.Cleanup(func() { _ = g.Close() })
	require.Error(t, securefile.CheckPrivate(g))
}

func TestWriteAtomicCreatesAndReplacesPrivateFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	for _, data := range [][]byte{[]byte("first"), bytes.Repeat([]byte{0, 1, 255}, 32768), nil} {
		require.NoError(t, securefile.WriteAtomic(path, data))
		actual, err := os.ReadFile(path)
		require.NoError(t, err)
		require.True(t, bytes.Equal(data, actual))
		assertions.PrivateFile(t, path)
		entries, err := os.ReadDir(dir)
		require.NoError(t, err)
		require.Len(t, entries, 1, "temporary files must not survive a successful replacement")
	}
}

func TestWriteAtomicReplacesExistingPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state")
	require.NoError(t, os.WriteFile(path, []byte("old"), 0o644))
	require.NoError(t, securefile.WriteAtomic(path, []byte("new")))
	actual, err := os.ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, "new", string(actual))
	assertions.PrivateFile(t, path)
}

func TestWriteAtomicFailurePreservesExistingDataAndCleansTemp(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "state")
	require.NoError(t, os.Mkdir(target, 0o700))
	sentinel := filepath.Join(target, "previous")
	require.NoError(t, os.WriteFile(sentinel, []byte("old"), 0o600))
	require.Error(t, securefile.WriteAtomic(target, []byte("new")))
	actual, err := os.ReadFile(sentinel)
	require.NoError(t, err)
	require.Equal(t, "old", string(actual))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Len(t, entries, 1, "failed commits must remove their private temporary file")
}

func TestWriteAtomicMissingParentDoesNotBootstrapState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "absent", "state")
	require.ErrorIs(t, securefile.WriteAtomic(path, []byte("new")), os.ErrNotExist)
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries, "callers retain explicit bootstrap and directory ownership")
}

func TestWriteAtomicInvalidTargetCleansTemp(t *testing.T) {
	dir := t.TempDir()
	require.Error(t, securefile.WriteAtomic(filepath.Join(dir, "bad\x00name"), []byte("new")))
	entries, err := os.ReadDir(dir)
	require.NoError(t, err)
	require.Empty(t, entries)
}
