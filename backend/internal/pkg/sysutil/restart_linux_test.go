package sysutil

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRestartServiceRunsShutdownHandlers(t *testing.T) {
	const helper = "SUB2API_TEST_GRACEFUL_RESTART_HELPER"
	const completed = "shutdown-handler-and-defer-completed"
	if os.Getenv(helper) == "1" {
		shutdown := make(chan os.Signal, 1)
		signal.Notify(shutdown, syscall.SIGTERM)
		defer signal.Stop(shutdown)
		defer fmt.Fprintln(os.Stdout, completed)
		if err := RestartService(); err != nil {
			t.Fatal(err)
		}
		select {
		case received := <-shutdown:
			if received != syscall.SIGTERM {
				t.Fatalf("unexpected restart signal: %v", received)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("restart did not enter the application's graceful shutdown path")
		}
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	command := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestRestartServiceRunsShutdownHandlers$")
	command.Env = append(os.Environ(), helper+"=1")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("restart subprocess failed: %v\n%s", err, output)
	}
	if !strings.Contains(string(output), completed) {
		t.Fatalf("restart skipped the shutdown handler or its deferred cleanup:\n%s", output)
	}
}
