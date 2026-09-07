package main

import (
	"bytes"
	"strings"
	"testing"
)

// `tera redeem` once pointed at a domain that is not the app and promised
// a payout rail and minimum that do not exist (flockd#32). Lock in the
// console URL and keep the copy free of unbacked promises: the page carries
// the current truth.
func TestRedeemDefaultURL(t *testing.T) {
	c := cmdRedeem()
	if got := c.Flags().Lookup("url").DefValue; got != "https://app.teraflock.com/redeem" {
		t.Fatalf("default redeem url = %q", got)
	}
}

func TestRedeemCopyMakesNoPromises(t *testing.T) {
	c := cmdRedeem()
	var out bytes.Buffer
	c.SetOut(&out)
	// An empty URL keeps the test from opening a real page on a developer
	// machine; only the printed copy is under test here.
	c.SetArgs([]string{"--url", ""})
	_ = c.Execute()
	s := out.String()
	for _, banned := range []string{"$25", "minimum", "Stripe"} {
		if strings.Contains(s, banned) {
			t.Errorf("redeem output advertises %q:\n%s", banned, s)
		}
	}
	if !strings.Contains(s, "Redeem earned credits at") {
		t.Errorf("redeem output missing the pointer line:\n%s", s)
	}
}
