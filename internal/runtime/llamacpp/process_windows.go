//go:build windows

package llamacpp

import (
	"errors"
	"fmt"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// Windows child-process control (flockd#31), cgo-free via x/sys/windows.
//
// Job object: every llama-server the supervisor spawns is assigned to one
// job created with JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE. The daemon holds
// the only handle, so when the daemon exits for any reason — clean stop,
// panic, taskkill /F, a crash — the kernel closes the handle and kills
// every process in the job. No orphaned llama-server.exe keeps the model
// in VRAM or squats on the loopback port. Nested jobs are supported since
// Windows 8, so this also works when the daemon itself runs inside a job
// (Task Scheduler puts its tasks in one).
//
// Creation flags: CREATE_NEW_PROCESS_GROUP so a console control event
// can target the child alone (its pid is its group id); CREATE_NO_WINDOW
// only when the daemon has no console (logon task, desktop app): without
// it a console child of a console-less parent opens a visible console
// window, and with it the child gets a hidden console of its own, out of
// reach of the parent's GenerateConsoleCtrlEvent (terminate_windows.go).

var (
	kernel32             = windows.NewLazySystemDLL("kernel32.dll")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
)

// hasConsole reports whether this process is attached to a console.
func hasConsole() bool {
	if err := procGetConsoleWindow.Find(); err != nil {
		return false
	}
	h, _, _ := procGetConsoleWindow.Call()
	return h != 0
}

func childSysProcAttr() *syscall.SysProcAttr {
	flags := uint32(windows.CREATE_NEW_PROCESS_GROUP)
	if !hasConsole() {
		flags |= windows.CREATE_NO_WINDOW
	}
	return &syscall.SysProcAttr{CreationFlags: flags}
}

// childGuard owns the kill-on-close job object.
type childGuard struct {
	job windows.Handle
}

func newChildGuard() (*childGuard, error) {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		return nil, fmt.Errorf("llamacpp: CreateJobObject: %w", err)
	}
	info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		_ = windows.CloseHandle(job)
		return nil, fmt.Errorf("llamacpp: SetInformationJobObject: %w", err)
	}
	return &childGuard{job: job}, nil
}

// adopt puts a started child into the job. Called right after Start: the
// window in which a daemon death would still orphan the child is the few
// microseconds between the two calls.
func (g *childGuard) adopt(cmd *exec.Cmd) error {
	if g == nil || g.job == 0 {
		return errors.New("llamacpp: no job object")
	}
	if cmd == nil || cmd.Process == nil {
		return errors.New("llamacpp: adopt: process not started")
	}
	// os.Process does not export its handle; open one with just the
	// rights AssignProcessToJobObject needs.
	h, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return fmt.Errorf("llamacpp: OpenProcess(%d): %w", cmd.Process.Pid, err)
	}
	defer windows.CloseHandle(h)
	if err := windows.AssignProcessToJobObject(g.job, h); err != nil {
		return fmt.Errorf("llamacpp: AssignProcessToJobObject(%d): %w", cmd.Process.Pid, err)
	}
	return nil
}

// Close releases the job handle, which kills any process still in it.
func (g *childGuard) Close() error {
	if g == nil || g.job == 0 {
		return nil
	}
	err := windows.CloseHandle(g.job)
	g.job = 0
	return err
}
