//go:build !windows

package assertions

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/pkg/securefile"
)

func TestPrivateFileRequiresExactOwnerPermissions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private")
	if err := securefile.WriteAtomic(path, []byte("private")); err != nil {
		t.Fatal(err)
	}
	PrivateFile(t, path)
	for _, mode := range []os.FileMode{0o644, 0o666, 0o400, 0o700} {
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
		if err := privateFileError(path); err == nil {
			t.Fatalf("accepted non-private mode %o", mode)
		}
	}
	if err := privateFileError(t.TempDir()); err == nil {
		t.Fatal("accepted directory as a private file")
	}
}
