//go:build !windows

package llamacpp

import (
	"os/exec"
	"syscall"
)

func terminate(cmd *exec.Cmd) error {
	return cmd.Process.Signal(syscall.SIGTERM)
}
