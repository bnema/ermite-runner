// SPDX-License-Identifier: MIT

package vm

import (
	"context"
	"fmt"
	"net/netip"
	"strconv"
	"strings"

	"bnema.dev/ermite-runner/internal/cmdrun"
)

// tapPrefix must match the "ertap*" wildcard of the nftables policy.
const tapPrefix = "ertap"

func tapName(slot int) string { return tapPrefix + strconv.Itoa(slot) }

// createTap creates a persistent TAP owned by uid (the user Firecracker runs
// as, so it may open it), gives the host side its /30 address and brings it up.
// It needs CAP_NET_ADMIN. On error the TAP is removed again.
func createTap(ctx context.Context, run cmdrun.Runner, name string, uid int, gateway netip.Addr) error {
	u := strconv.Itoa(uid)
	if _, err := run.Run(ctx, nil, "ip", "tuntap", "add", "dev", name, "mode", "tap", "user", u, "group", u); err != nil {
		return fmt.Errorf("create tap %s: %w", name, err)
	}
	for _, args := range [][]string{
		{"addr", "add", gateway.String() + "/30", "dev", name},
		{"link", "set", "dev", name, "up"},
	} {
		if _, err := run.Run(ctx, nil, "ip", args...); err != nil {
			_ = deleteTap(context.WithoutCancel(ctx), run, name)
			return fmt.Errorf("configure tap %s: %w", name, err)
		}
	}
	return nil
}

// deleteTap removes a TAP; a missing device is not an error.
func deleteTap(ctx context.Context, run cmdrun.Runner, name string) error {
	out, err := run.Run(ctx, nil, "ip", "link", "del", "dev", name)
	if err != nil && (strings.Contains(string(out), "Cannot find device") || strings.Contains(err.Error(), "Cannot find device")) {
		return nil
	}
	return err
}

// leftoverTaps lists interfaces named like ours ("ertap<N>").
func leftoverTaps(ctx context.Context, run cmdrun.Runner) ([]string, error) {
	out, err := run.Run(ctx, nil, "ip", "-o", "link", "show")
	if err != nil {
		return nil, err
	}
	return parseTapNames(string(out)), nil
}

// parseTapNames extracts "ertap<digits>" interface names from `ip -o link show`
// ("3: ertap0: <...>" or "3: ertap0@if2: <...>").
func parseTapNames(s string) []string {
	var out []string
	for _, line := range strings.Split(s, "\n") {
		_, rest, ok := strings.Cut(line, ": ")
		if !ok {
			continue
		}
		name, _, _ := strings.Cut(rest, ":")
		name, _, _ = strings.Cut(name, "@")
		if n, ok := strings.CutPrefix(name, tapPrefix); ok && n != "" {
			if _, err := strconv.Atoi(n); err == nil {
				out = append(out, name)
			}
		}
	}
	return out
}
