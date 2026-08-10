//go:build windows

package llamacpp

import "os/exec"

// Windows has no SIGTERM; Kill is the pragmatic option until we wire up
// GenerateConsoleCtrlEvent / job objects. TODO(windows).
func terminate(cmd *exec.Cmd) error {
	return cmd.Process.Kill()
}
