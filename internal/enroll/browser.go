package enroll

import (
	"fmt"
	"os/exec"
	"runtime"
)

// openBrowser launches the default browser, per-OS.
func openBrowser(url string) error {
	var cmd *exec.Cmd
	switch runtime.GOOS {
	case "darwin":
		cmd = exec.Command("open", url)
	case "windows":
		cmd = exec.Command("rundll32", "url.dll,FileProtocolHandler", url)
	default:
		cmd = exec.Command("xdg-open", url)
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("enroll: open browser: %w", err)
	}
	return nil
}
