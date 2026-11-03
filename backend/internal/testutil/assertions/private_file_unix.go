//go:build !windows

package assertions

import (
	"fmt"
	"os"
)

func privateFileError(path string) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return fmt.Errorf("expected regular file with mode 0600, got %s", info.Mode())
	}
	return nil
}
