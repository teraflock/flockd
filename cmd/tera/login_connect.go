package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"net/url"
	"strings"
)

// consoleConnectURL is where a signed-in operator gets a single-use claim
// code for a machine. It is what a plain `tera login` sends people to.
//
// A plain `tera login` used to open the invite page (enroll.login_url)
// with the loopback parameters and nothing else. That page only answers a
// link that carries an invite token, so the command the README, the brew
// caveats and install.sh all lead with ended on "That invite link doesn't
// work". The console page needs no token: sign in, press the button, get a
// code. The code comes back by paste rather than a loopback redirect, so
// the same command works over SSH and on a box with no browser.
const consoleConnectURL = "https://app.teraflock.com/connect"

// claimCodeFromInput pulls the claim code out of what an operator pastes
// from the console page: the code itself, the whole
// `tera login --claim-code <code>` line the page shows, or its
// "Open in the Teraflock app" link (teraflock://enroll?code=<code>).
// It returns "" when nothing in the input has the shape of a code.
func claimCodeFromInput(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	cand := s
	if i := strings.Index(s, "code="); i >= 0 && strings.Contains(s[:i], "://") {
		cand = s[i+len("code="):]
		if j := strings.IndexAny(cand, "& \t#"); j >= 0 {
			cand = cand[:j]
		}
		if u, err := url.QueryUnescape(cand); err == nil {
			cand = u
		}
	} else if f := strings.Fields(s); len(f) > 0 {
		cand = f[len(f)-1]
		if eq := strings.LastIndex(cand, "="); eq >= 0 { // --claim-code=<code>
			cand = cand[eq+1:]
		}
	}
	cand = strings.Trim(cand, "\"'`")
	if !validClaimCode(cand) {
		return ""
	}
	return cand
}

// validClaimCode is the shape the desktop app accepts from an invite link
// (desktop src-tauri/src/deeplink.rs): 8 to 64 characters, letters,
// digits, '-' or '_'. Loose on purpose, so a new code format needs no CLI
// release; tight enough that a pasted sentence or URL is not sent to the
// coordinator as a code.
func validClaimCode(code string) bool {
	if len(code) < 8 || len(code) > 64 {
		return false
	}
	for _, c := range []byte(code) {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_'
		if !ok {
			return false
		}
	}
	return true
}

// errNoClaimCode is returned when the prompt ends without a usable code.
// Its text is the whole remedy: both forms it names work without a prompt.
var errNoClaimCode = errors.New("no claim code entered: get one at " + consoleConnectURL +
	" and run `tera login --claim-code <code>`, or, with an invite link, `tera login --url '<invite link>'`")

// promptClaimCode sends the operator to the console for a code and reads
// it back from in. openBrowser is tried only when asked to (an interactive
// terminal); the address is printed either way, so it can be opened on
// another machine. One retry on input that is not a code: a stray Enter
// or a half-selected paste should not end the command.
func promptClaimCode(in io.Reader, out io.Writer, openBrowser func(string) error) (string, error) {
	fmt.Fprintln(out, "Connect this machine to your Teraflock account:")
	fmt.Fprintln(out, "  1. Open", consoleConnectURL, "and sign in")
	fmt.Fprintln(out, "  2. Press \"Get a code for this computer\" and paste the code here")
	if openBrowser != nil {
		// Non-fatal: the address is on screen.
		_ = openBrowser(consoleConnectURL)
	}
	r := bufio.NewReader(in)
	for attempt := 0; attempt < 2; attempt++ {
		fmt.Fprint(out, "Code: ")
		line, err := r.ReadString('\n')
		if code := claimCodeFromInput(line); code != "" {
			return code, nil
		}
		if err != nil { // EOF: nothing was piped in, or the terminal closed
			fmt.Fprintln(out)
			return "", errNoClaimCode
		}
		if strings.TrimSpace(line) != "" {
			fmt.Fprintln(out, styleWarn.Render("!"), "that does not look like a claim code (it is 8 or more letters, digits and dashes)")
		}
	}
	return "", errNoClaimCode
}
