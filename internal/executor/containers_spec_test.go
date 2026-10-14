// SPDX-License-Identifier: MIT

package executor

import (
	"path"
	"path/filepath"
	"strings"
	"testing"

	"bnema.dev/ermite-actions/expr"
)

func TestParseVolume(t *testing.T) {
	for in, want := range map[string]mount{
		"/data":          {target: "/data"},
		"vol:/data":      {source: "vol", target: "/data"},
		"/data:ro":       {target: "/data", readOnly: true},
		"/h:/c:ro":       {source: "/h", target: "/c", readOnly: true},
		"/h:/c:rw":       {source: "/h", target: "/c"},
		"/h:/c:ro:extra": {target: "/h:/c:ro:extra"},
		"/data:RO":       {target: "/data", readOnly: true},
	} {
		if got := parseVolume(in); got != want {
			t.Errorf("%q: %+v, want %+v", in, got, want)
		}
	}
}

func TestContainerPathTranslation(t *testing.T) {
	c := &container{maps: []pathMap{{"/h/work", "/__w"}, {"/opt/tc", "/__t"}}}
	c.mounts = []mount{{source: "/h/work", target: "/__w"}}
	for in, want := range map[string]string{
		"/h/work": "/__w", "/h/work/_temp/x": "/__w/_temp/x", "/h/workx": "/h/workx", "": "", "/etc": "/etc",
	} {
		if got := c.translateToContainer(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
	if got := c.translateToHost("/__w/a/b"); got != "/h/work/a/b" {
		t.Error(got)
	}
	// ResolvePathForStepHost translates quoted paths under a mount only.
	if got := c.resolvePath(`"/h/work/a"`); got != "/__w/a" {
		t.Error(got)
	}
	if got := c.resolvePath("/opt/tc/x"); got != "/opt/tc/x" {
		t.Errorf("no mount source covers it: %s", got)
	}
}

func TestRegistryHost(t *testing.T) {
	for in, want := range map[string]string{
		"ubuntu:24.04": "", "owner/img": "", "ghcr.io/owner/img:1": "ghcr.io",
		"localhost:5000/img": "localhost:5000", "a/b/c": "a",
	} {
		if got := parseRegistryHost(in); got != want {
			t.Errorf("%q: %q want %q", in, got, want)
		}
	}
}

func mustValue(t *testing.T, js string) expr.Value {
	t.Helper()
	v, err := expr.FromJSON([]byte(js))
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func TestConvertContainerForms(t *testing.T) {
	j := &job{}
	v := mustValue(t, `{"image":"docker://ubuntu:24.04","env":{"A":"1","B":2},"ports":[8080,"9000:9001"],"volumes":["v:/d"],"options":"--cpus 1","credentials":{"username":"u","password":"p"}}`)
	spec, err := j.convertContainer(v, false)
	if err != nil || spec.image != "ubuntu:24.04" || len(spec.env) != 2 || spec.env[1].Value != "2" ||
		strings.Join(spec.ports, ",") != "8080,9000:9001" || spec.credPass != "p" || spec.options != "--cpus 1" {
		t.Fatalf("%+v %v", spec, err)
	}
	if s, err := j.convertContainer(mustValue(t, `""`), false); s != nil || err != nil {
		t.Errorf("empty string: %v %v", s, err)
	}
	if s, err := j.convertContainer(mustValue(t, `{"image":""}`), true); s != nil || err != nil {
		t.Errorf("empty image: %v %v", s, err)
	}
	for _, bad := range []string{`{"entrypoint":"x","image":"i"}`, `{"nope":1,"image":"i"}`, `{"image":["a"]}`} {
		if _, err := j.convertContainer(mustValue(t, bad), false); err == nil {
			t.Errorf("%s accepted", bad)
		}
	}
	// A service may only set entrypoint and command when the server allows it.
	if _, err := j.convertContainer(mustValue(t, `{"image":"i","command":"x"}`), true); err == nil {
		t.Error("command without the feature")
	}
	j.baseEnv = []string{"ACTIONS_SERVICE_CONTAINER_COMMAND=true"}
	if s, err := j.convertContainer(mustValue(t, `{"image":"i","command":"x","entrypoint":"e"}`), true); err != nil || s.command != "x" || s.entrypoint != "e" {
		t.Errorf("%+v %v", s, err)
	}
}

func TestImageReferenceGrammar(t *testing.T) {
	good := []string{"ubuntu", "ubuntu:24.04", "ghcr.io/o/img:1", "localhost:5000/a/b:tag", "a/b_c.d-e", "redis@sha256:" + strings.Repeat("a", 64),
		"r.io:443/x/y:1@sha256:" + strings.Repeat("0", 64), "Ubuntu:24.04",
		"[::1]:5000/x", "[::1]/x/y:tag", "[2001:db8::1]:443/a/b@sha256:" + strings.Repeat("a", 64)}
	bad := []string{"", "--help", "-x", "-", " a", "a b", "a\nb", "a;b", "a$(x)", "a:", "a:-x", "a@sha256:zz", "/a", "a//b", "a:b:c", "[::1/x", "[]/x", "[zz]/x", "[::1]x", "[::1]:/x", strings.Repeat("a", 1100)}
	for _, g := range good {
		if !validImageRef(g) {
			t.Errorf("rejected %q", g)
		}
	}
	for _, b := range bad {
		if validImageRef(b) {
			t.Errorf("accepted %q", b)
		}
	}
	j := &job{}
	if _, err := j.convertContainer(mustValue(t, `"--help"`), false); err == nil || !strings.Contains(err.Error(), "Invalid container image '--help'") {
		t.Errorf("container: --help: %v", err)
	}
}

func TestServiceNamesAndContainerIDs(t *testing.T) {
	f := newFakeCtr(t)
	for _, name := range []string{"-x", ".y", "a b", "a/b"} {
		rec, res := ctrRun(t, f, "    services:\n      \""+name+"\":\n        image: redis:7\n", "      - run: echo never\n", nil)
		if res.Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.step(t, "Set up job")), "Invalid service name") {
			t.Errorf("%q: %+v", name, res)
		}
	}
	for id, ok := range map[string]bool{strings.Repeat("a", 64): true, strings.Repeat("0", 64): true, "Usage: docker create": false, strings.Repeat("a", 63): false, strings.Repeat("A", 64): false, "--help": false} {
		if containerIDRe.MatchString(id) != ok {
			t.Errorf("id %q", id)
		}
	}
	// A help text printed by create is not taken for an id: nothing is started.
	f.set(t, "create_out", "Usage:  docker create [OPTIONS] IMAGE\n")
	rec, res := ctrRun(t, f, "    container: ubuntu:24.04\n", "      - run: echo never\n", nil)
	if res.Conclusion != ConclusionFailure || !strings.Contains(logOf(rec.step(t, "Initialize containers")), "##[error]Docker create did not return a container id: 'Usage:  docker create [OPTIONS] IMAGE'") {
		t.Errorf("%+v\n%s", res, logOf(rec.step(t, "Initialize containers")))
	}
	for _, c := range f.calls(t) {
		if c[0] == "start" {
			t.Errorf("started %v", c)
		}
	}
}

func TestTranslateToHostDoesNotEscapeTheMapping(t *testing.T) {
	c := &container{maps: []pathMap{{"/h/work", "/__w"}, {"/opt/tc", "/__t"}}}
	for in, want := range map[string]string{
		"/__w/a/b":            "/h/work/a/b",
		"/__w/a/../b":         "/h/work/b",
		"/__w/../outside/x":   "/outside/x",
		"/__w/a/../../../etc": "/etc",
		"/__w//a///b/":        "/h/work/a/b",
		"/__w":                "/h/work",
		"/__t/../__w/x":       "/h/work/x",
		"/__wx/y":             "/__wx/y",
		"rel/../x":            "rel/../x",
	} {
		got := c.translateToHost(in)
		if got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
		if strings.HasPrefix(in, "/__w") && !strings.HasPrefix(got, "/h/work") && got != path.Clean(in) {
			t.Errorf("%q escaped to %q", in, got)
		}
	}
}

// A matcher file named by a command of the container is read only when it is
// in a directory shared with the host: ".." cannot reach another host file.
func TestMatcherPathFromTheContainerStaysInside(t *testing.T) {
	f := newFakeCtr(t)
	var outside string
	rec, res := ctrRun(t, f, ctrJob, "      - name: s\n        run: echo\n", func(c *Config) {
		outside = filepath.Join(filepath.Dir(c.Layout.WorkRoot), "outside")
		writeFiles(t, filepath.Dir(c.Layout.WorkRoot), files{"outside/m.json": `{"problemMatcher":[{"owner":"leak","pattern":[{"regexp":"x"}]}]}`})
		f.set(t, "exec_out", "::add-matcher::/__w/../outside/m.json\n::add-matcher::"+outside+"/m.json\n::add-matcher::/etc/hostname\n")
	})
	if res.Conclusion != ConclusionFailure {
		t.Fatalf("%+v", res)
	}
	log := logOf(rec.step(t, "s"))
	if strings.Contains(log, "leak") || strings.Count(log, "##[error]") < 2 || !strings.Contains(log, "Could not find file") && !strings.Contains(log, "ould not find") {
		t.Errorf("log:\n%s", log)
	}
}
