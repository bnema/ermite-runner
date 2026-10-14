// SPDX-License-Identifier: MIT

package guest

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"bnema.dev/ermite-runner/internal/execstream"
	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// 100 secrets of 48 KiB (4.8 MB) do not fit one frame (MaxFrame is 2 MiB) but
// do as NamedValue frames: this crosses the real transport, DriveJob on the
// host side and waitForRun on the executor side.
func TestManyLargeSecretsCrossTheStream(t *testing.T) {
	secrets := map[string]string{}
	for i := 0; i < 100; i++ {
		secrets[fmt.Sprintf("S%03d", i)] = strings.Repeat(string(rune('a'+i%26)), 48<<10)
	}
	vars := map[string]string{"A": "1", "B": "2"}

	// the old shape, every secret in the Run frame, would not have worked
	err := execstream.NewConn(discard{}).Send(&execpb.Frame{Body: &execpb.Frame_Run{Run: &execpb.Run{Job: &execpb.JobSpec{Contexts: strings.Join(valuesOf(secrets), "")}}}})
	if err == nil {
		t.Fatal("test premise: all the secrets must exceed one frame")
	}

	hostSide, guestSide := net.Pipe()
	defer func() { _ = hostSide.Close(); _ = guestSide.Close() }()
	got := make(chan error, 1)
	go func() {
		c := execstream.NewConn(guestSide)
		_ = c.Send(&execpb.Frame{Body: &execpb.Frame_Hello{Hello: &execpb.Hello{}}})
		run, vals, err := waitForRun(context.Background(), c, 10*time.Second)
		switch {
		case err != nil:
			got <- err
		case run.GetJob().GetJobKey() != "k" || len(vals.secrets) != 100 || len(vals.vars) != 2 || vals.vars["B"] != "2":
			got <- fmt.Errorf("run %v, %d secrets, vars %v", run.GetJob().GetJobKey(), len(vals.secrets), vals.vars)
		default:
			for k, v := range secrets {
				if vals.secrets[k] != v {
					got <- fmt.Errorf("secret %s differs", k)
					return
				}
			}
			got <- nil
			_ = c.Send(&execpb.Frame{Body: &execpb.Frame_Exit{Exit: &execpb.Exit{}}})
		}
	}()
	_, exit, err := execstream.DriveJob(hostSide, &execpb.Run{Job: &execpb.JobSpec{JobKey: "k"}}, nil, execstream.JobHandler{Secrets: secrets, Vars: vars})
	if err != nil || exit == nil {
		t.Fatalf("DriveJob: %v", err)
	}
	if err := <-got; err != nil {
		t.Fatal(err)
	}
}

type discard struct{}

func (discard) Write(p []byte) (int, error) { return len(p), nil }
func (discard) Read([]byte) (int, error)    { select {} }

func valuesOf(m map[string]string) []string {
	var out []string
	for _, v := range m {
		out = append(out, v)
	}
	return out
}
