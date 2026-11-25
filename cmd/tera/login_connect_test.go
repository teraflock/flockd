package main

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

func TestClaimCodeFromInput(t *testing.T) {
	for in, want := range map[string]string{
		"ABCD-1234":                               "ABCD-1234",
		"  ABCD-1234\r\n":                         "ABCD-1234",
		"`ABCD-1234`":                             "ABCD-1234",
		"tera login --claim-code ABCD-1234":       "ABCD-1234",
		"tera login --claim-code=ABCD-1234":       "ABCD-1234",
		"teraflock://enroll?code=ABCD-1234":       "ABCD-1234",
		"teraflock://enroll?code=ABCD-1234&x=1":   "ABCD-1234",
		"teraflock://enroll?v=1&code=abcd_1234 ":  "abcd_1234",
		"teraflock://enroll?code=ABCD%2D1234":     "ABCD-1234",
		"":                                        "",
		"   ":                                     "",
		"short":                                   "",
		"yes please":                              "",
		"https://app.teraflock.com/connect":       "",
		"teraflock://enroll?code=has%20space-123": "",
		strings.Repeat("a", 65):                   "",
	} {
		if got := claimCodeFromInput(in); got != want {
			t.Errorf("claimCodeFromInput(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestPromptClaimCode(t *testing.T) {
	t.Run("a pasted code is returned and the console is named", func(t *testing.T) {
		var out bytes.Buffer
		opened := ""
		code, err := promptClaimCode(strings.NewReader("ABCD-1234\n"), &out, func(u string) error {
			opened = u
			return nil
		})
		if err != nil || code != "ABCD-1234" {
			t.Fatalf("code=%q err=%v", code, err)
		}
		if opened != consoleConnectURL || !strings.Contains(out.String(), consoleConnectURL) {
			t.Fatalf("opened %q; output:\n%s", opened, out.String())
		}
	})
	t.Run("no trailing newline is still a code", func(t *testing.T) {
		code, err := promptClaimCode(strings.NewReader("ABCD-1234"), &bytes.Buffer{}, nil)
		if err != nil || code != "ABCD-1234" {
			t.Fatalf("code=%q err=%v", code, err)
		}
	})
	t.Run("one bad line gets a second try", func(t *testing.T) {
		var out bytes.Buffer
		code, err := promptClaimCode(strings.NewReader("\nteraflock://enroll?code=ABCD-1234\n"), &out, nil)
		if err != nil || code != "ABCD-1234" {
			t.Fatalf("code=%q err=%v", code, err)
		}
		code, err = promptClaimCode(strings.NewReader("nope\nABCD-1234\n"), &out, nil)
		if err != nil || code != "ABCD-1234" {
			t.Fatalf("code=%q err=%v", code, err)
		}
		if !strings.Contains(out.String(), "does not look like a claim code") {
			t.Fatalf("no hint after a bad line:\n%s", out.String())
		}
	})
	t.Run("nothing to read ends with the two commands that work", func(t *testing.T) {
		for _, in := range []string{"", "nope\nstill nope\nABCD-1234\n"} {
			_, err := promptClaimCode(strings.NewReader(in), &bytes.Buffer{}, nil)
			if !errors.Is(err, errNoClaimCode) {
				t.Fatalf("input %q: err = %v", in, err)
			}
		}
		for _, want := range []string{"--claim-code", "--url", consoleConnectURL} {
			if !strings.Contains(errNoClaimCode.Error(), want) {
				t.Errorf("remedy does not mention %s: %s", want, errNoClaimCode)
			}
		}
	})
	t.Run("a browser that will not open is not an error", func(t *testing.T) {
		code, err := promptClaimCode(strings.NewReader("ABCD-1234\n"), &bytes.Buffer{}, func(string) error {
			return errors.New("no browser")
		})
		if err != nil || code != "ABCD-1234" {
			t.Fatalf("code=%q err=%v", code, err)
		}
	})
}
