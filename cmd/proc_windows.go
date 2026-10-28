//go:build windows

package cmd

import (
	"os"
	"os/exec"
	"syscall"
)

const createNewProcessGroup = 0x00000200

// startDetached starts a process in its own process group, so it outlives the CLI.
func startDetached(exe string, args []string, output *os.File) error {
	cmd := exec.Command(exe, args...)
	cmd.Stdout = output
	cmd.Stderr = output
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup}
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}

func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

func stopProcess(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}
