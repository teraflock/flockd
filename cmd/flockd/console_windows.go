//go:build windows

package main

import (
	"os"

	"golang.org/x/sys/windows"
)

// Release builds of flockd.exe are GUI-subsystem binaries (-H=windowsgui,
// .goreleaser.yaml): the per-user logon task (internal/svc) runs in the
// operator's interactive session, and a console-subsystem program started
// there opens a console window that sits on the desktop for as long as the
// daemon runs. A GUI-subsystem program gets no window — and, started from
// a terminal, no console either, so `flockd -version` and the "flockd:
// <err>" line in main would print nowhere. This attaches to the parent's
// console when there is one (a terminal, or tera capturing output) and
// points any standard handle the parent did not set up at it. Handles the
// parent did set up — the pipes tera reads `flockd -version` through —
// are left alone. Under the logon task there is no parent console, the
// call fails, and the log file (--log-file) stays the only sink, as
// before. A plain `go build` is still a console binary; everything here
// is a no-op for it.

var procAttachConsole = windows.NewLazySystemDLL("kernel32.dll").NewProc("AttachConsole")

const attachParentProcess = ^uint32(0) // ATTACH_PARENT_PROCESS

func init() { attachParentConsole() }

func attachParentConsole() {
	if err := procAttachConsole.Find(); err != nil {
		return
	}
	if r, _, _ := procAttachConsole.Call(uintptr(attachParentProcess)); r == 0 {
		return // no parent console (the logon task), or we already have one
	}
	os.Stdout = consoleFile(windows.STD_OUTPUT_HANDLE, os.Stdout, "stdout")
	os.Stderr = consoleFile(windows.STD_ERROR_HANDLE, os.Stderr, "stderr")
}

// consoleFile returns cur when the parent already supplied that standard
// handle (a pipe, a file), else a handle on the console we just attached.
func consoleFile(std uint32, cur *os.File, name string) *os.File {
	if h, err := windows.GetStdHandle(std); err == nil && h != 0 && h != windows.InvalidHandle {
		return cur
	}
	p, err := windows.UTF16PtrFromString("CONOUT$")
	if err != nil {
		return cur
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ|windows.GENERIC_WRITE,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, 0, 0)
	if err != nil {
		return cur
	}
	return os.NewFile(uintptr(h), name)
}
