// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"encoding/json"
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
	got  *ermite.ResolveActionsRequest
	resp *ermite.ResolveActionsResponse
	err  error
}

func (c *resolveStub) ResolveActions(_ context.Context, r *connect.Request[ermite.ResolveActionsRequest]) (*connect.Response[ermite.ResolveActionsResponse], error) {
	c.got = r.Msg
	if c.err != nil {
		return nil, c.err
	}
	return connect.NewResponse(c.resp), nil
}

func resolveJob(c jobClient) *jobRun {
	return &jobRun{client: c, ctx: context.Background(), job: &ermite.Job{JobId: "j1"}, log: zerowrap.Logger{},
		t: Timing{CallTimeout: time.Second, BackoffMin: time.Millisecond, BackoffMax: time.Millisecond}}
}

func TestResolveActions(t *testing.T) {
	c := &resolveStub{resp: &ermite.ResolveActionsResponse{Actions: []*ermite.ResolvedAction{
		{Ref: "a/b@v1", NameWithOwner: "a/b", ResolvedSha: strings.Repeat("a", 40), TarballUrl: "https://x/t", Token: "tok", Source: ermite.ActionSource_ACTION_SOURCE_ERMITE},
		{Ref: "a/c@v1", Error: "boom"},
	}}}
	resp := resolveJob(c).resolveActions(&execpb.ResolveRequest{Id: 7, Uses: []string{"a/b@v1", "a/c@v1"}})
	if resp.Error != "" || len(resp.Actions) != 2 {
		t.Fatalf("%+v", resp)
	}
	if c.got.JobId != "j1" || len(c.got.Refs) != 2 || c.got.Refs[1].Uses != "a/c@v1" {
		t.Errorf("request %+v", c.got)
	}
	if a := resp.Actions[0]; a.Token != "tok" || a.TarballUrl != "https://x/t" || a.NameWithOwner != "a/b" {
		t.Errorf("%+v", a)
	}
	if resp.Actions[1].Error != "boom" {
		t.Errorf("%+v", resp.Actions[1])
	}
}

func TestResolveActionsFailures(t *testing.T) {
	c := &resolveStub{err: connect.NewError(connect.CodeInvalidArgument, nil)}
	if r := resolveJob(c).resolveActions(&execpb.ResolveRequest{Uses: []string{"a/b@v1"}}); !strings.Contains(r.Error, "invalid_argument") {
		t.Errorf("%+v", r)
	}
	c = &resolveStub{resp: &ermite.ResolveActionsResponse{}}
	if r := resolveJob(c).resolveActions(&execpb.ResolveRequest{Uses: []string{"a/b@v1"}}); r.Error == "" {
		t.Errorf("a short answer must fail: %+v", r)
	}
	many := make([]string, maxResolveRefs+1)
	if r := resolveJob(c).resolveActions(&execpb.ResolveRequest{Uses: many}); r.Error == "" {
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
