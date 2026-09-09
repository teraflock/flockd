//go:build windows

package llamacpp

import (
	"os/exec"

	"golang.org/x/sys/windows"
)

// terminate asks llama-server to stop. Windows has no SIGTERM; the
// closest thing is a console control event, which only reaches a child
// that shares the daemon's console (interactive `flockd --standalone`).
// CTRL_BREAK is the only event that can target one process group; the
// child was created with CREATE_NEW_PROCESS_GROUP so its pid is the group.
// llama-server's console handler only maps CTRL_C to SIGINT, so a
// CTRL_BREAK ends it through the default handler — an in-process exit,
// not a drain; the drain itself happens before stop() is reached (the
// governor's yield grace and the engine's request accounting).
//
// Without a console (logon task, desktop app) there is nothing to send:
// return nil and let the supervisor's grace timer TerminateProcess the
// child; the job object (process_windows.go) covers a daemon that never
// gets that far.
func terminate(cmd *exec.Cmd) error {
	if !hasConsole() {
		return nil
	}
	return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(cmd.Process.Pid))
}
