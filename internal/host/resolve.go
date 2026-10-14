// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// Limits of what the sandbox may ask the server through the host. The sandbox
// runs untrusted code, so the host does not trust its requests.
//
// A job asks once per level of the actions it uses (the runner's depth limit is
// 9, so at most 10 rounds for the steps of the job and the repository
// composites below them), and once more for every local composite action that
// has repository actions in its steps, when its step runs: a workflow has at
// most 1,000 steps. The budget below is generous for that and bounded:
// maxResolveRounds requests and maxResolveRefsTotal references per job, whatever
// they ask. (Local composites restart the depth at 1 as in the runner; if the
// recording of ermite-conformance corpus/act-composite-depth shows that
// github.com limits them, the executor will count them, and these limits stay
// as the safety net.)
const (
	maxResolveRefs      = 100  // per request: ResolveActionsRequest.refs
	maxResolveRounds    = 128  // per job
	maxResolveRefsTotal = 2048 // per job
	maxRefLen           = 512
	maxSegmentLen       = 100
	maxRefNameLen       = 255
)

// checkUses validates a "uses" value of a request: owner/repo[/path]@ref with
// the characters repository names, paths and refs have. The server decides
// whether the action exists; this keeps garbage and oversized strings from
// reaching it.
func checkUses(u string) error {
	if u == "" || len(u) > maxRefLen {
		return fmt.Errorf("%q is not a valid action reference", cut(u))
	}
	name, ref, ok := strings.Cut(u, "@")
	if !ok || ref == "" || len(ref) > maxRefNameLen || strings.ContainsFunc(ref, badRefRune) {
		return fmt.Errorf("%q is not a valid action reference", cut(u))
	}
	segs := strings.Split(name, "/")
	if len(segs) < 2 {
		return fmt.Errorf("%q is not a valid action reference", cut(u))
	}
	for _, seg := range segs {
		if seg == "" || seg == "." || seg == ".." || len(seg) > maxSegmentLen || strings.ContainsFunc(seg, badSegmentRune) {
			return fmt.Errorf("%q is not a valid action reference", cut(u))
		}
	}
	return nil
}

func cut(s string) string {
	if len(s) > 80 {
		return s[:80] + "..."
	}
	return s
}

func badSegmentRune(r rune) bool {
	return (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '-' && r != '_' && r != '.'
}

// badRefRune: a ref is a branch, a tag or a sha; no control characters, spaces
// or characters git refuses.
func badRefRune(r rune) bool {
	return r <= ' ' || r == 0x7f || strings.ContainsRune("~^:?*[\\@", r) // "@": a "uses" has exactly one (action.ParseUses)
}

// resolveActions answers a ResolveRequest of the executor with
// RunnerService.ResolveActions. The job token stays here: the executor only
// gets what the server returns (the per-archive tokens included, which it
// masks). Requests come one at a time (see execstream.JobHandler.Resolve). A
// call that fails without a verdict is retried like the other job calls; a
// failure of the call itself is reported to the executor, which fails "Set up
// job" (ActionManager: FailedToResolveActionDownloadInfoException). ctx ends
// with the session.
func (j *jobRun) resolveActions(ctx context.Context, req *execpb.ResolveRequest) *execpb.ResolveResponse {
	fail := func(format string, a ...any) *execpb.ResolveResponse {
		return &execpb.ResolveResponse{Error: fmt.Sprintf(format, a...)}
	}
	if len(req.Uses) == 0 {
		return fail("an empty request for action resolution")
	}
	if len(req.Uses) > maxResolveRefs {
		return fail("cannot resolve %d actions at once (the limit is %d)", len(req.Uses), maxResolveRefs)
	}
	for _, u := range req.Uses {
		if err := checkUses(u); err != nil {
			return fail("%v", err)
		}
	}
	j.mu.Lock()
	j.resolveRounds++
	j.resolveRefs += len(req.Uses)
	over := j.resolveRounds > maxResolveRounds || j.resolveRefs > maxResolveRefsTotal
	j.mu.Unlock()
	if over {
		return fail("this job asked to resolve too many actions (the limits are %d requests and %d references)", maxResolveRounds, maxResolveRefsTotal)
	}
	refs := make([]*ermite.ActionRef, len(req.Uses))
	for i, u := range req.Uses {
		refs[i] = &ermite.ActionRef{Uses: u}
	}
	// The job ending, or the session, stops the call.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(j.ctx, cancel)
	defer stop()
	b := backoff{min: j.t.BackoffMin, max: j.t.BackoffMax}
	for attempt := 1; ; attempt++ {
		cctx, ccancel := context.WithTimeout(ctx, j.t.CallTimeout)
		resp, err := j.client.ResolveActions(cctx, connect.NewRequest(&ermite.ResolveActionsRequest{JobId: j.job.JobId, Refs: refs}))
		ccancel()
		if err == nil {
			return j.resolveResponse(resp.Msg, len(req.Uses))
		}
		if ctx.Err() != nil {
			return fail("the job was cancelled while its actions were resolved")
		}
		switch classify(err) {
		case kindEnded, kindAbandoned:
			j.abandon(err)
			return fail("the server ended the job")
		case kindPermanent:
			return fail("resolving actions failed: %s", connect.CodeOf(err))
		}
		j.log.Warn().Err(err).Int("attempt", attempt).Msg("ResolveActions failed")
		if attempt >= 3 || !sleep(ctx, b.next()) {
			return fail("resolving actions failed: %s", connect.CodeOf(err))
		}
	}
}

// resolveResponse converts the answer of the server. The token of an archive is
// a secret that the sandbox sends in an Authorization header, so it only goes
// with an archive of the ermite source whose tarball URL is on an origin this
// host knows to be ermite: the public origin of the job (github.server_url) or
// the origin of the runner's own URL. The origin the sandbox must send it to is
// AuthOrigin; with guest forge access the tarball URL on the public origin is
// rewritten to the runner's, like the URLs of the contexts. A token that does
// not satisfy this is dropped (the download then fails like an unauthorised
// one).
func (j *jobRun) resolveResponse(m *ermite.ResolveActionsResponse, want int) *execpb.ResolveResponse {
	if len(m.Actions) != want {
		return &execpb.ResolveResponse{Error: fmt.Sprintf("the server resolved %d actions out of %d", len(m.Actions), want)}
	}
	out := &execpb.ResolveResponse{}
	for _, a := range m.Actions {
		ra := &execpb.ResolvedAction{
			Ref: a.Ref, NameWithOwner: a.NameWithOwner, Path: a.Path, ResolvedSha: a.ResolvedSha,
			TarballUrl: a.TarballUrl, Source: a.Source.String(), Error: a.Error,
		}
		if a.Source == ermite.ActionSource_ACTION_SOURCE_ERMITE {
			if origin := originOf(a.TarballUrl); origin != "" {
				if j.guestForge != "" && origin == j.publicOrigin {
					ra.TarballUrl = j.guestForge + strings.TrimPrefix(a.TarballUrl, a.TarballUrl[:strings.Index(a.TarballUrl, "://")+3+hostLen(a.TarballUrl)])
					origin = j.guestForge
				}
				if a.Token != "" && (origin == j.guestForge || origin == j.publicOrigin || origin == j.serverOrigin) {
					ra.Token, ra.AuthOrigin = a.Token, origin
				}
			}
		}
		out.Actions = append(out.Actions, ra)
	}
	return out
}

// setOrigins records the origins that are ermite for this job: the public one
// (the origin of github.server_url as the server sent it), the runner's own
// (ERMITE_RUNNER_URL) and the guest forge origin, "" when it is off.
func (j *jobRun) setOrigins(contexts, serverURL, guestForge string) {
	j.serverOrigin = originOf(serverURL)
	j.guestForge = guestForge
	if guestForge != "" {
		j.guestForge = originOf(guestForge)
	}
	if top, err := parseObject([]byte(contexts)); err == nil {
		for _, m := range top {
			if m.key != "github" {
				continue
			}
			gh, err := parseObject(m.val)
			if err != nil {
				break
			}
			for _, g := range gh {
				var s string
				if g.key == "server_url" && json.Unmarshal(g.val, &s) == nil {
					j.publicOrigin = originOf(s)
				}
			}
		}
	}
}

// SetGitHubToken returns the contexts JSON of a job with github.token set to
// token (the job repository token, Job.github_token). The key is added first,
// like the runner does (ExecutionContext.cs:1023: token, then what the server
// gave); an empty token leaves the contexts as they are.
func SetGitHubToken(contexts, token string) (string, error) {
	if token == "" {
		return contexts, nil
	}
	top, err := parseObject([]byte(contexts))
	if err != nil {
		return "", fmt.Errorf("job contexts: %w", err)
	}
	gi := -1
	for i, m := range top {
		if m.key == "github" {
			gi = i
		}
	}
	var gh []jsonMember
	if gi >= 0 {
		if gh, err = parseObject(top[gi].val); err != nil {
			return "", fmt.Errorf("job contexts: github: %w", err)
		}
	}
	tv, err := json.Marshal(token)
	if err != nil {
		return "", err
	}
	set := false
	for i, m := range gh {
		if m.key == "token" {
			gh[i].val, set = tv, true
		}
	}
	if !set {
		gh = append([]jsonMember{{"token", tv}}, gh...)
	}
	ghv, err := encodeObject(gh)
	if err != nil {
		return "", err
	}
	if gi >= 0 {
		top[gi].val = ghv
	} else {
		top = append(top, jsonMember{"github", ghv})
	}
	out, err := encodeObject(top)
	return string(out), err
}

// hostLen is the length of the authority of an absolute URL.
func hostLen(u string) int {
	rest := u[strings.Index(u, "://")+3:]
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		return i
	}
	return len(rest)
}
