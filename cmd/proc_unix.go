//go:build !windows

package cmd

import (
	"os"
	"os/exec"
	"syscall"
)

// startDetached starts a process in its own session, so it outlives the CLI.
func startDetached(exe string, args []string, output *os.File) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}

func stopProcess(pid int) error {
	return syscall.Kill(pid, syscall.SIGTERM)
}
