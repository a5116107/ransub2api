// Package sysutil provides system-level utilities for process management.
package sysutil

import (
	"fmt"
	"log"
	"os"
	"runtime"
	"syscall"
	"time"
)

// RestartService sends SIGTERM through the application's normal shutdown path.
// A Linux service supervisor (systemd Restart=always or a container restart
// policy) starts the service again after request draining and cleanup finish.
func RestartService() error {
	if runtime.GOOS != "linux" {
		log.Println("Service restart via signal requires Linux with a restart supervisor")
		return nil
	}

	process, err := os.FindProcess(os.Getpid())
	if err != nil {
		return fmt.Errorf("find service process for restart: %w", err)
	}

	log.Println("Initiating service restart through graceful shutdown...")

	// Let the response finish before the server begins its normal drain.
	// Exiting here directly would skip active requests and deferred cleanup.
	go func() {
		time.Sleep(100 * time.Millisecond)
		if err := process.Signal(syscall.SIGTERM); err != nil {
			log.Printf("Service restart signal failed: %v", err)
		}
	}()

	return nil
}

// RestartServiceAsync is a fire-and-forget version of RestartService.
// It logs errors instead of returning them, suitable for goroutine usage.
func RestartServiceAsync() {
	if err := RestartService(); err != nil {
		log.Printf("Service restart failed: %v", err)
		log.Println("Please restart the service manually: sudo systemctl restart sub2api")
	}
}
