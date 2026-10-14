// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeCtrDocker is a docker CLI for the container steps: it records every
// call (one argument per line between "--" markers, like fakeDocker) and
// answers like the real one. Behaviour is chosen by files in its directory:
//
//	pull_fail        N: the first N pulls fail
//	health           words, one per inspect: starting, healthy, unhealthy
//	create_fail      docker create exits 125
//	exec_out         what docker exec prints
//	hang_exec        docker exec waits until it gets a signal
type fakeCtr struct {
	dir, bin, log string
}

func newFakeCtr(t *testing.T) *fakeCtr {
	t.Helper()
	d := t.TempDir()
	f := &fakeCtr{dir: d, bin: filepath.Join(d, "docker"), log: filepath.Join(d, "calls")}
	script := `#!/bin/sh
D=` + d + `
{ echo "--"; for a in "$@"; do printf "%s\n" "$a"; done; echo "ENV:CVAR=$CVAR:EMPTYV=${EMPTYV-unset}:PATH=$PATH"; } >> $D/calls
[ "$1" = "--config" ] && { CFG=$2; shift 2; }
hang() { trap 'exit 130' INT TERM; while :; do sleep 0.05; done; }
[ -f $D/hang_$1 ] && hang
if [ -f $D/hang_for ]; then case "$*" in *"$(cat $D/hang_for)"*) hang ;; esac; fi
case "$1" in
version) echo "'1.48'" ;;
ps) case "$*" in *--quiet*) ;; *status=running*) id=$(echo "$*" | sed 's/.*id=\([^ ]*\).*/\1/'); echo "$id Up Less than a second" ;; esac ;;
network) ;;
login) cat > $D/login_stdin
  if [ -f $D/lock_cfg ]; then mkdir -p $CFG/sub; : > $CFG/sub/f; chmod 500 $CFG/sub; fi ;;
pull) if [ -f $D/pull_fail ]; then n=$(cat $D/pull_fail); if [ "$n" -gt 0 ]; then echo $((n-1)) > $D/pull_fail; echo "pull denied"; exit 1; fi; fi; echo "pulled $2" ;;
create) [ -f $D/create_fail ] && { echo "unknown flag"; exit 125; }
  [ -f $D/create_out ] && { cat $D/create_out; exit 0; }
  n=$(cat $D/seq 2>/dev/null || echo 0); n=$((n+1)); echo $n > $D/seq; printf "%064d\n" $n ;;
start) echo "$2" ;;
inspect) case "$*" in
  *Healthcheck*) if [ -s $D/health ]; then w=$(head -1 $D/health); sed -i 1d $D/health; echo "$w"; fi ;;
  *Config.Env*) printf 'HOME=/github/home\nGITHUB_ACTIONS=true\nPATH=/usr/bin:/bin\n' ;;
  esac ;;
port) [ -f $D/ports ] && cat $D/ports ;;
logs) echo "service log of $3" ;;
rm) echo "$3" ;;
exec) if [ -f $D/add_path ]; then f=$(echo "$GITHUB_PATH" | sed "s#^/__w#$(cat $D/add_path)#"); echo /added >> "$f"; fi
  [ -f $D/exec_env ] && echo "ENV GITHUB_WORKSPACE=$GITHUB_WORKSPACE RUNNER_TEMP=$RUNNER_TEMP HOME=${HOME:-<unset>} GITHUB_OUTPUT=$GITHUB_OUTPUT GITHUB_EVENT_PATH=$GITHUB_EVENT_PATH"
  [ -f $D/hang_exec ] && { trap 'exit 130' INT TERM; while :; do sleep 0.05; done; }
  [ -f $D/exec_out ] && cat $D/exec_out
  if [ -f $D/exec_exit ]; then n=$(cat $D/exec_exit); [ "$n" != 0 ] && echo 0 > $D/exec_exit && exit $n; fi ;;
esac
exit 0
`
	if err := os.WriteFile(f.bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *fakeCtr) set(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(f.dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fakeCtr) calls(t *testing.T) [][]string { return calls(t, f.log) }

// verbs are the docker subcommands called, in order.
func (f *fakeCtr) verbs(t *testing.T) []string {
	var out []string
	for _, c := range f.calls(t) {
		if c[0] == "--config" && len(c) > 2 {
			c = c[2:]
		}
		v := c[0]
		if v == "network" && len(c) > 1 {
			v += " " + c[1]
		}
		out = append(out, v)
	}
	return out
}

// ctrRun runs a workflow whose job has the given container/services YAML.
func ctrRun(t *testing.T, f *fakeCtr, jobYAML, steps string, tweak func(*Config)) (*recorder, Result) {
	t.Helper()
	return ctrRunC(t, f, nil, jobYAML, steps, tweak)
}

func ctrRunC(t *testing.T, f *fakeCtr, cancel *Canceller, jobYAML, steps string, tweak func(*Config)) (*recorder, Result) {
	t.Helper()
	needTools(t, "bash")
	return runLocalC(t, cancel, wfHead+jobYAML+"    steps:\n"+steps, "j", func(c *Config) {
		c.Actions.DockerPath = f.bin
		c.Actions.Backoff = func() time.Duration { return time.Millisecond }
		c.Actions.ExternalsDir = fakeNode(t)
		if tweak != nil {
			tweak(c)
		}
	})
}

// concl is the conclusion of the step called name.
func (r *recorder) concl(name string) Conclusion {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, s := range r.steps {
		if s.Name == name {
			return s.Conclusion
		}
	}
	return ""
}
