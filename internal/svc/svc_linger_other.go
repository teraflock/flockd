//go:build !linux

package svc

import "context"

// Linger reports nothing here: lingering is a systemd concept, and the
// launchd agent and the Windows logon task have no equivalent. applies is
// always false off Linux.
func Linger(context.Context) (user string, on, known, applies bool) {
	return "", false, false, false
}
