// SPDX-License-Identifier: MIT

package executor

import (
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
	for _, bad := range []string{`{"entrypoint":"x","image":"i"}`, `{"nope":1,"image":"i"}`, `{"image":["a"]}`, `"-v"`} {
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
