//go:build integration && codexcli && windows

package repository_test

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Own the complete CLI subtree. Some native helpers inherit stdout after the
// main CLI exits; waiting for EOF or using a file leaks handles on Windows.
// Starting suspended closes the spawn/assignment race: no helper can escape
// the unnamed job before the parent installs kill-on-close ownership.
func priorityCLIRun(cmd *exec.Cmd) ([]byte, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, err
	}
	closeJob := func() error {
		if job == 0 {
			return nil
		}
		err := windows.CloseHandle(job)
		job = 0
		return err
	}
	defer func() { _ = closeJob() }()
	limits := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	_, err = windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits)))
	if err != nil {
		return nil, err
	}
	var output bytes.Buffer
	cmd.Stdout, cmd.Stderr = &output, &output
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_SUSPENDED}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	abort := func(cause error) ([]byte, error) {
		killErr := cmd.Process.Kill()
		closeErr := closeJob()
		waitErr := cmd.Wait()
		return output.Bytes(), errors.Join(cause, killErr, closeErr, waitErr)
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE|windows.SYNCHRONIZE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return abort(err)
	}
	defer func() { _ = windows.CloseHandle(process) }()
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		return abort(err)
	}
	if err := resumePriorityCLI(uint32(cmd.Process.Pid)); err != nil {
		return abort(err)
	}
	// Observe the actual process, not output EOF. Context cancellation still
	// kills it through exec.CommandContext, then the job reclaims descendants.
	_, processErr := windows.WaitForSingleObject(process, windows.INFINITE)
	closeErr := closeJob()
	waitErr := cmd.Wait()
	return output.Bytes(), errors.Join(processErr, closeErr, waitErr)
}

func resumePriorityCLI(pid uint32) error {
	snapshot, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPTHREAD, 0)
	if err != nil {
		return err
	}
	defer func() { _ = windows.CloseHandle(snapshot) }()
	entry := windows.ThreadEntry32{Size: uint32(unsafe.Sizeof(windows.ThreadEntry32{}))}
	for err := windows.Thread32First(snapshot, &entry); err == nil; err = windows.Thread32Next(snapshot, &entry) {
		if entry.OwnerProcessID != pid {
			continue
		}
		thread, err := windows.OpenThread(windows.THREAD_SUSPEND_RESUME, false, entry.ThreadID)
		if err != nil {
			return err
		}
		_, resumeErr := windows.ResumeThread(thread)
		return errors.Join(resumeErr, windows.CloseHandle(thread))
	}
	return fmt.Errorf("suspended CLI %d has no primary thread", pid)
}
