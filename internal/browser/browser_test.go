package browser

import (
	"strings"
	"testing"
)

func TestCommandPerOS(t *testing.T) {
	cases := map[string][]string{
		"darwin":  {"open", "https://x"},
		"windows": {"rundll32", "url.dll,FileProtocolHandler", "https://x"},
		"linux":   {"xdg-open", "https://x"},
		"freebsd": {"xdg-open", "https://x"},
	}
	for goos, want := range cases {
		got := command(goos, "https://x").Args
		// exec.Command resolves Path separately; Args[0] keeps the name.
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("%s: args = %q, want %q", goos, got, want)
		}
	}
}
