// SPDX-License-Identifier: MIT
package deps

import (
	"bnema.dev/ermite-runner/internal/execstream/execpb"
	"fmt"
	"strings"
	"testing"
)

func request() *execpb.DepsRequest {
	return &execpb.DepsRequest{GoVersion: "1.27.3", Updates: []*execpb.DepsUpdate{{Ecosystem: "gomod", Name: "example.org/a", Version: "v1.2.0"}, {Ecosystem: "gomod", Name: "example.org/b", Version: "v1.3.0"}}}
}
func TestScript(t *testing.T) {
	s, err := Script(request())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"GOFLAGS=-mod=mod", "GOTOOLCHAIN=local", "GOPROXY=https://proxy.golang.org", "GOSUMDB=sum.golang.org", "GOPRIVATE= GONOSUMDB=", "go get 'example.org/a@v1.2.0' 'example.org/b@v1.3.0'"} {
		if !strings.Contains(s, want) {
			t.Errorf("missing %q: %s", want, s)
		}
	}
	if strings.Count(s, "go mod tidy") != 1 {
		t.Fatal(s)
	}
}
func TestValidation(t *testing.T) {
	for _, v := range []string{"1.27", "1.27.3"} {
		r := request()
		r.GoVersion = v
		if _, err := Allowed(r); err != nil {
			t.Fatal(err)
		}
	}
	for _, v := range []string{"", "go1.27", "1.27rc1", "1.27;echo hi", "2.0", "1.27.3\n"} {
		r := request()
		r.GoVersion = v
		if _, err := Allowed(r); err == nil {
			t.Errorf("accepted %q", v)
		}
	}
	r := request()
	r.Updates[0].Ecosystem = "npm"
	if _, err := Allowed(r); err == nil {
		t.Fatal("accepted npm")
	}
	for _, d := range []string{"../x", "/tmp", "x/../y", "x\\y"} {
		r := request()
		r.Updates[0].Directory = d
		if _, err := Allowed(r); err == nil {
			t.Errorf("accepted %s", d)
		}
	}
}
func TestCollector(t *testing.T) {
	c := &Collector{Allowed: map[string]bool{"go.mod": true}}
	for i := 0; i < 2; i++ {
		if err := c.Add(&execpb.DepsFileChunk{Path: "go.mod", Offset: uint64(i * MaxChunk), Data: make([]byte, MaxChunk), Last: i == 1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Finish(&execpb.DepsResult{FileCount: 1}); err != nil {
		t.Fatal(err)
	}
	if len(c.Files["go.mod"]) != MaxFile {
		t.Fatal("size")
	}
	if err := c.Add(&execpb.DepsFileChunk{Path: "go.mod", Last: true}); err == nil {
		t.Fatal("accepted late chunk")
	}
}
func TestCollectorReject(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []*execpb.DepsFileChunk
	}{
		{"path", []*execpb.DepsFileChunk{{Path: "../go.mod", Last: true}}},
		{"offset", []*execpb.DepsFileChunk{{Path: "go.mod", Offset: 1}}},
		{"chunk", []*execpb.DepsFileChunk{{Path: "go.mod", Data: make([]byte, MaxChunk+1)}}},
		{"duplicate", []*execpb.DepsFileChunk{{Path: "go.mod", Last: true}, {Path: "go.mod", Last: true}}},
		{"interleaved", []*execpb.DepsFileChunk{{Path: "go.mod"}, {Path: "go.sum"}}},
		{"file", []*execpb.DepsFileChunk{{Path: "go.mod", Data: make([]byte, MaxChunk)}, {Path: "go.mod", Offset: MaxChunk, Data: make([]byte, MaxChunk)}, {Path: "go.mod", Offset: MaxFile, Data: []byte{1}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Collector{Allowed: map[string]bool{"go.mod": true, "go.sum": true}}
			var err error
			for _, f := range tc.chunks {
				err = c.Add(f)
				if err != nil {
					break
				}
			}
			if err == nil {
				t.Fatal("accepted invalid chunks")
			}
		})
	}
}
func TestCollectorTotalAndCount(t *testing.T) {
	c := &Collector{Allowed: map[string]bool{}}
	for i := 0; i < 65; i++ {
		c.Allowed[fmt.Sprint(i)] = true
	}
	for i := 0; i < 64; i++ {
		if err := c.Add(&execpb.DepsFileChunk{Path: fmt.Sprint(i), Last: true}); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.Add(&execpb.DepsFileChunk{Path: "64", Last: true}); err == nil {
		t.Fatal("accepted 65 files")
	}
	c = &Collector{Allowed: map[string]bool{}}
	for i := 0; i < 9; i++ {
		p := fmt.Sprint(i)
		c.Allowed[p] = true
		err := c.Add(&execpb.DepsFileChunk{Path: p, Data: make([]byte, MaxChunk), Last: true})
		if (err != nil) != (i == 8) {
			t.Fatalf("total at %d: %v", i, err)
		}
	}
}
func TestCollectorFinal(t *testing.T) {
	for _, c := range []*Collector{{}, {current: "go.mod"}} {
		if err := c.Finish(&execpb.DepsResult{FileCount: 1}); err == nil {
			t.Fatal("accepted incomplete report")
		}
	}
}

func TestCheckCloneURL(t *testing.T) {
	for _, tc := range []struct {
		url   string
		http  bool
		valid bool
	}{
		{"https://forge.example/o/r.git", false, true},
		{"https://forge.example/o/r.git", true, true},
		{"http://localhost:8080/o/r.git", true, true},
		{"http://localhost:8080/o/r.git", false, false},
		{"http://u:p@localhost/o/r.git", true, false},
		{"https://u@forge.example/o/r.git", false, false},
		{"http:///o/r.git", true, false},
		{"git://forge.example/o/r.git", true, false},
	} {
		if err := checkCloneURL(tc.url, tc.http); (err == nil) != tc.valid {
			t.Errorf("%s http=%v: err=%v", tc.url, tc.http, err)
		}
	}
}
