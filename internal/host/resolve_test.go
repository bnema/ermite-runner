// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/bnema/zerowrap"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

type resolveStub struct {
	stubClient
	got    *ermite.ResolveActionsRequest
	resp   *ermite.ResolveActionsResponse
	err    error
	fn     func(*ermite.ResolveActionsRequest) *ermite.ResolveActionsResponse
	onCall func(*ermite.ResolveActionsRequest)
	calls  int
}

func (c *resolveStub) ResolveActions(_ context.Context, r *connect.Request[ermite.ResolveActionsRequest]) (*connect.Response[ermite.ResolveActionsResponse], error) {
	c.got = r.Msg
	c.calls++
	if c.onCall != nil {
		c.onCall(r.Msg)
	}
	if c.err != nil {
		return nil, c.err
	}
	if c.fn != nil {
		return connect.NewResponse(c.fn(r.Msg)), nil
	}
	return connect.NewResponse(c.resp), nil
}

func resolveJob(c jobClient) *jobRun {
	j := &jobRun{client: c, ctx: context.Background(), job: &ermite.Job{JobId: "j1"}, log: zerowrap.Logger{},
		t: Timing{CallTimeout: time.Second, BackoffMin: time.Millisecond, BackoffMax: time.Millisecond}}
	j.setOrigins(`{"github":{"server_url":"https://ermite.example"}}`, "https://ermite.example", "")
	return j
}

func TestResolveActions(t *testing.T) {
	c := &resolveStub{resp: &ermite.ResolveActionsResponse{Actions: []*ermite.ResolvedAction{
		{Ref: "a/b@v1", NameWithOwner: "a/b", ResolvedSha: strings.Repeat("a", 40), TarballUrl: "https://ermite.example/t", Token: "tok", Source: ermite.ActionSource_ACTION_SOURCE_ERMITE},
		{Ref: "a/c@v1", Error: "boom"},
	}}}
	resp := resolveJob(c).resolveActions(context.Background(), &execpb.ResolveRequest{Id: 7, Uses: []string{"a/b@v1", "a/c@v1"}})
	if resp.Error != "" || len(resp.Actions) != 2 {
		t.Fatalf("%+v", resp)
	}
	if c.got.JobId != "j1" || len(c.got.Refs) != 2 || c.got.Refs[1].Uses != "a/c@v1" {
		t.Errorf("request %+v", c.got)
	}
	if a := resp.Actions[0]; a.Token != "tok" || a.AuthOrigin != "https://ermite.example" || a.TarballUrl != "https://ermite.example/t" || a.NameWithOwner != "a/b" {
		t.Errorf("%+v", a)
	}
	if resp.Actions[1].Error != "boom" {
		t.Errorf("%+v", resp.Actions[1])
	}
}

func TestResolveActionsFailures(t *testing.T) {
	c := &resolveStub{err: connect.NewError(connect.CodeInvalidArgument, nil)}
	if r := resolveJob(c).resolveActions(context.Background(), &execpb.ResolveRequest{Uses: []string{"a/b@v1"}}); !strings.Contains(r.Error, "invalid_argument") {
		t.Errorf("%+v", r)
	}
	c = &resolveStub{resp: &ermite.ResolveActionsResponse{}}
	if r := resolveJob(c).resolveActions(context.Background(), &execpb.ResolveRequest{Uses: []string{"a/b@v1"}}); r.Error == "" {
		t.Errorf("a short answer must fail: %+v", r)
	}
	many := make([]string, maxResolveRefs+1)
	if r := resolveJob(c).resolveActions(context.Background(), &execpb.ResolveRequest{Uses: many}); r.Error == "" {
		t.Error("more than 100 refs must fail")
	}
}

func TestSetGitHubToken(t *testing.T) {
	cases := map[string]string{
		`{"github":{"job":"j","token":""},"needs":{}}`: `{"github":{"job":"j","token":"T"},"needs":{}}`,
		`{"github":{"job":"j"}}`:                       `{"github":{"token":"T","job":"j"}}`,
		`{"needs":{}}`:                                 `{"needs":{},"github":{"token":"T"}}`,
	}
	for in, want := range cases {
		got, err := SetGitHubToken(in, "T")
		if err != nil || got != want {
			t.Errorf("%s: %s %v, want %s", in, got, err, want)
		}
	}
	if got, _ := SetGitHubToken(`{"a":1}`, ""); got != `{"a":1}` {
		t.Errorf("empty token changed the contexts: %s", got)
	}
	if _, err := SetGitHubToken(`[]`, "T"); err == nil {
		t.Error("not an object")
	}
	var m map[string]any
	if got, _ := SetGitHubToken(`{}`, `a"b\`); json.Unmarshal([]byte(got), &m) != nil {
		t.Errorf("invalid JSON: %s", got)
	}
}

func TestCheckUses(t *testing.T) {
	long := func(n int) string { return strings.Repeat("a", n) }
	for u, ok := range map[string]bool{
		"actions/checkout@v4": true, "o/r/sub/dir@refs/heads/feat/x": true, "o/r@" + strings.Repeat("a", 40): true, "o/r.go_x-y@v1.2.3": true,
		// GO-011: the path of the action is a path, not a repository name.
		"o/r/action+tools@v1": true, "o/r/déploiement/日本語@v1": true, "o/r/with space/x@v1": true, "o/r/a(b)[c]{d}~e#f%g@v1": true,
		"o/r/" + long(255) + "@v1": true, "o/r/.github/actions/x@v1": true, "o/r/.hidden@v1": true, "o/r/a..b@v1": true,
		"": false, "o/r": false, "o@v1": false, "o/r@": false, "/r@v1": false, "o//r@v1": false, "o/../r@v1": false, "o/r/..@v1": false,
		"o/r/a/../b@v1": false, "o/r/./a@v1": false, "o/r/a//b@v1": false, "o/r/a/@v1": false,
		"o/r/a\\b@v1": false, "o/r/a\x00b@v1": false, "o/r/a\nb@v1": false, "o/r/a\x7fb@v1": false, "o/r/\xff@v1": false,
		"o/r/" + long(256) + "@v1": false, long(101) + "/r@v1": false, "o/" + long(101) + "@v1": false,
		"o/r@" + long(256): false, "o/r/" + strings.Repeat("a/", 600) + "x@v1": false,
		"o/r@v1 x": false, "o/r@v1\n": false, "o/r@v~1": false, "o/r@v:1": false, "o /r@v1": false, "o/r@v1@v2": false,
		"o/r\x00@v1": false, "ö/r@v1": false, "o/r+x@v1": false, "o/r:x@v1": false,
	} {
		if err := checkUses(u); (err == nil) != ok {
			t.Errorf("%q: %v, want ok=%v", cut(u), err, ok)
		}
	}
}

// GO-003: whatever the sandbox sends, the server is called with valid
// references only, and with a bounded number of calls and references.
func TestResolveActionsRejectsAndBudgets(t *testing.T) {
	c := &resolveStub{resp: &ermite.ResolveActionsResponse{Actions: []*ermite.ResolvedAction{{Ref: "a/b@v1"}}}}
	j := resolveJob(c)
	for _, uses := range [][]string{nil, {}, {""}, {"nonsense"}, {"a/b@v1", "bad ref"}} {
		c.got = nil
		if r := j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: uses}); r.Error == "" || c.got != nil {
			t.Errorf("%q: error %q, called=%v", uses, r.Error, c.got != nil)
		}
	}
	// Distinct references, one per request: the rounds are the limit.
	c.resp = nil
	j = resolveJob(c)
	c.fn = func(r *ermite.ResolveActionsRequest) *ermite.ResolveActionsResponse { return echo(r) }
	var calls int
	for i := 0; i < maxResolveRounds+20; i++ {
		c.got = nil
		r := j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: []string{fmt.Sprintf("a/b%d@v1", i)}})
		if c.got != nil {
			calls++
		}
		if i >= maxResolveRounds && r.Error == "" {
			t.Fatalf("round %d is over the budget and was served", i)
		}
	}
	if calls != maxResolveRounds {
		t.Errorf("%d calls, the budget is %d", calls, maxResolveRounds)
	}
	// Distinct references in big requests: the references are the limit.
	j = resolveJob(c)
	served := 0
	for i := 0; i < 40; i++ {
		big := make([]string, maxResolveRefs)
		for k := range big {
			big[k] = fmt.Sprintf("a/b%d_%d@v1", i, k)
		}
		if r := j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: big}); r.Error == "" {
			served++
		}
	}
	if served != maxResolveRefsTotal/maxResolveRefs {
		t.Errorf("served %d requests of %d refs, the budget is %d refs", served, maxResolveRefs, maxResolveRefsTotal)
	}
}

func echo(r *ermite.ResolveActionsRequest) *ermite.ResolveActionsResponse {
	out := &ermite.ResolveActionsResponse{}
	for _, ref := range r.Refs {
		out.Actions = append(out.Actions, &ermite.ResolvedAction{Ref: ref.Uses, NameWithOwner: "a/b"})
	}
	return out
}

// GO-010: a resolution is cached for the job, so repeated invocations of a local
// composite action cost nothing, and a big request is split into calls.
func TestResolveActionsCacheAndBatches(t *testing.T) {
	c := &resolveStub{}
	c.fn = echo
	j := resolveJob(c)
	for i := 0; i < 500; i++ {
		r := j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: []string{"a/b@v1", "a/c/sub@v2"}})
		if r.Error != "" || len(r.Actions) != 2 || r.Actions[1].Ref != "a/c/sub@v2" {
			t.Fatalf("invocation %d: %+v", i, r)
		}
	}
	if c.calls != 1 {
		t.Errorf("%d calls for 500 invocations of the same actions", c.calls)
	}
	// A new action goes through; only it is asked.
	c.got = nil
	if r := j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: []string{"a/b@v1", "a/new@v1"}}); r.Error != "" || len(c.got.Refs) != 1 || c.got.Refs[0].Uses != "a/new@v1" {
		t.Errorf("%+v %v", r, c.got)
	}
	// Duplicates in one request are asked once, answered in order.
	j = resolveJob(c)
	c.calls = 0
	r := j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: []string{"a/x@v1", "a/y@v1", "a/x@v1"}})
	if r.Error != "" || len(r.Actions) != 3 || r.Actions[2].Ref != "a/x@v1" || r.Actions[1].Ref != "a/y@v1" || c.calls != 1 || len(c.got.Refs) != 2 {
		t.Errorf("%+v calls %d", r, c.calls)
	}
	// 250 distinct references: three calls of at most 100, in order.
	j = resolveJob(c)
	c.calls = 0
	var sizes []int
	c.onCall = func(r *ermite.ResolveActionsRequest) { sizes = append(sizes, len(r.Refs)) }
	many := make([]string, 250)
	for i := range many {
		many[i] = fmt.Sprintf("a/b%d@v1", i)
	}
	r = j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: many})
	if r.Error != "" || len(r.Actions) != 250 || r.Actions[249].Ref != many[249] || fmt.Sprint(sizes) != "[100 100 50]" {
		t.Errorf("%q calls %v", r.Error, sizes)
	}
	// An error of one action is not cached.
	c.fn = func(r *ermite.ResolveActionsRequest) *ermite.ResolveActionsResponse {
		return &ermite.ResolveActionsResponse{Actions: []*ermite.ResolvedAction{{Ref: r.Refs[0].Uses, Error: "not found"}}}
	}
	j = resolveJob(c)
	c.calls = 0
	_ = j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: []string{"a/gone@v1"}})
	_ = j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: []string{"a/gone@v1"}})
	if c.calls != 2 {
		t.Errorf("an error was cached: %d calls", c.calls)
	}
	// More than the request limit.
	if r := j.resolveActions(context.Background(), &execpb.ResolveRequest{Uses: make([]string, maxRequestRefs+1)}); r.Error == "" {
		t.Error("a request over the limit was served")
	}
}

// GO-002 on the host side: the token only goes with an ermite archive whose URL
// is on an origin of ermite; with guest forge access the URL is rewritten.
func TestResolveResponseTokenOrigin(t *testing.T) {
	j := resolveJob(&resolveStub{})
	ermiteSrc, ghSrc := ermite.ActionSource_ACTION_SOURCE_ERMITE, ermite.ActionSource_ACTION_SOURCE_GITHUB
	conv := func(j *jobRun, src ermite.ActionSource, url, tok string) *execpb.ResolvedAction {
		r := j.resolveResponse(&ermite.ResolveActionsResponse{Actions: []*ermite.ResolvedAction{{Source: src, TarballUrl: url, Token: tok}}}, 1)
		return r.Actions[0]
	}
	if a := conv(j, ermiteSrc, "https://ermite.example/-/github/api/repos/o/r/tarball/abc", "t"); a.Token != "t" || a.AuthOrigin != "https://ermite.example" {
		t.Errorf("%+v", a)
	}
	for name, a := range map[string]*execpb.ResolvedAction{
		"github source":     conv(j, ghSrc, "https://ermite.example/x", "t"),
		"foreign origin":    conv(j, ermiteSrc, "https://evil.example/x", "t"),
		"subdomain":         conv(j, ermiteSrc, "https://x.ermite.example/x", "t"),
		"other port":        conv(j, ermiteSrc, "https://ermite.example:8443/x", "t"),
		"other scheme":      conv(j, ermiteSrc, "http://ermite.example/x", "t"),
		"not a url":         conv(j, ermiteSrc, "ermite.example/x", "t"),
		"credentials in it": conv(j, ermiteSrc, "https://u:p@ermite.example/x", "t"),
	} {
		if a.Token != "" || a.AuthOrigin != "" {
			t.Errorf("%s: token kept: %+v", name, a)
		}
	}
	// No token, no origin: github.com archives are anonymous.
	if a := conv(j, ghSrc, "https://codeload.github.com/o/r/tar.gz/abc", ""); a.Token != "" || a.AuthOrigin != "" {
		t.Errorf("%+v", a)
	}
	// Guest forge access: the sandbox reaches ermite at the runner's origin.
	j.setOrigins(`{"github":{"server_url":"https://ermite.example"}}`, "https://runner.internal:8088", "http://10.0.2.2:8088")
	a := conv(j, ermiteSrc, "https://ermite.example/-/github/api/repos/o/r/tarball/abc", "t")
	if a.TarballUrl != "http://10.0.2.2:8088/-/github/api/repos/o/r/tarball/abc" || a.AuthOrigin != "http://10.0.2.2:8088" || a.Token != "t" {
		t.Errorf("%+v", a)
	}
}
