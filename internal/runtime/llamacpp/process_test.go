package llamacpp

import (
	"fmt"
	"os"
	"os/exec"
	"time"
)

// runJobParentHelper is the second helper-process mode of TestMain: it
// plays the daemon. It spawns the fake llama-server child (this binary
// again, FLOCKD_FAKE_LLAMA_PORT set to port) under a child guard exactly
// the way supervisor.spawn does, prints the child's pid, then blocks until
// the test kills it — at which point the guard must take the child down
// too (process_windows_test.go).
func runJobParentHelper(port string) {
	guard, err := newChildGuard()
	if err != nil {
		fmt.Fprintln(os.Stderr, "guard:", err)
		os.Exit(1)
	}
	exe, err := os.Executable()
	if err != nil {
		os.Exit(1)
	}
	cmd := exec.Command(exe)
	cmd.Env = append(os.Environ(), "FLOCKD_JOB_PARENT_PORT=", "FLOCKD_FAKE_LLAMA_PORT="+port)
	cmd.SysProcAttr = childSysProcAttr()
	if err := cmd.Start(); err != nil {
		fmt.Fprintln(os.Stderr, "start child:", err)
		os.Exit(1)
	}
	if err := guard.adopt(cmd); err != nil {
		fmt.Fprintln(os.Stderr, "adopt:", err)
		os.Exit(1)
	}
	fmt.Printf("child %d\n", cmd.Process.Pid)
	// Block until killed. Not select{}: with no other goroutine the Go
	// runtime reports "all goroutines are asleep" and exits — which, on
	// the first CI run, killed the child through the job before the test
	// could even see it alive.
	for {
		time.Sleep(time.Hour)
	}
}
