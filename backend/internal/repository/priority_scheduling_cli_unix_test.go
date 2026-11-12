//go:build integration && codexcli && !windows

package repository_test

import "os/exec"

func priorityCLIRun(cmd *exec.Cmd) ([]byte, error) {
	return cmd.CombinedOutput()
}
