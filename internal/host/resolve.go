// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// Limits of what the sandbox may ask the server through the host. The sandbox
// runs untrusted code, so the host does not trust its requests.
//
// Resolutions are cached for the job, by "uses" value: an action that was
// resolved once is never asked again (the same sha for the whole job is what the
// server promises anyway), so a local composite action used 500 times costs one
// resolution. The budget counts only what reaches the server:
//
//   - a request from the sandbox has at most maxRequestRefs references (a
//     workflow has at most 1,000 steps), split into calls of maxResolveRefs, the
//     limit of ResolveActionsRequest;
//   - a job makes at most maxResolveRounds calls (up to 10 levels of repository
//     composites, then one call per local composite that brings new actions) and
//     asks for at most maxResolveRefsTotal distinct references.
//
// These are explicit limits of ermite, not of GitHub: a job that needs more
// distinct actions than that fails "Set up job" with the message below.
const (
	maxRequestRefs      = 1000
	maxResolveRefs      = 100  // per call to the server: ResolveActionsRequest.refs
	maxResolveRounds    = 128  // calls per job
	maxResolveRefsTotal = 2048 // distinct references per job
	maxRefLen           = 1024 // the whole "uses" value
	maxOwnerRepoLen     = 100  // owner and repository names
	maxPathElemLen      = 255  // an element of the path of the action: a file name
	maxRefNameLen       = 255
)

// checkUses validates a "uses" value of a request: owner/repo[/path]@ref. The
// owner and the repository have the characters of repository names; the path of
// the action is a path in the repository (any characters but control ones, with
// no empty, "." or ".." element and no backslash, which the runner reads as a
// separator, action.ParseUses); the ref has the characters git allows. The
// server decides whether the action exists; this keeps garbage and oversized
// strings from reaching it.
func checkUses(u string) error {
	bad := func() error { return fmt.Errorf("%q is not a valid action reference", cut(u)) }
	if u == "" || len(u) > maxRefLen || !utf8.ValidString(u) {
		return bad()
	}
	name, ref, ok := strings.Cut(u, "@")
	if !ok || ref == "" || len(ref) > maxRefNameLen || strings.ContainsFunc(ref, badRefRune) {
		return bad()
	}
	segs := strings.Split(name, "/")
	if len(segs) < 2 {
		return bad()
	}
	for _, seg := range segs[:2] {
		if seg == "" || seg == "." || seg == ".." || len(seg) > maxOwnerRepoLen || strings.ContainsFunc(seg, badSegmentRune) {
			return bad()
		}
	}
	for _, seg := range segs[2:] {
		if seg == "" || seg == "." || seg == ".." || len(seg) > maxPathElemLen || strings.ContainsFunc(seg, badPathRune) {
			return bad()
		}
	}
	return nil
}

// badPathRune: control characters (NUL included) and the backslash. Invalid
// UTF-8 is refused for the whole value by utf8.ValidString; a literal U+FFFD is a
// valid character.
func badPathRune(r rune) bool { return r < ' ' || r == 0x7f || r == '\\' }

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
// masks). Requests come one at a time (see execstream.JobHandler.Resolve). What
// was resolved before in the job comes from the cache; the rest goes to the
// server in calls of at most maxResolveRefs, under the budget above. A call that
// fails without a verdict is retried like the other job calls; a failure of the
// call itself is reported to the executor, which fails "Set up job"
// (ActionManager: FailedToResolveActionDownloadInfoException). ctx ends with the
// session.
func (j *jobRun) resolveActions(ctx context.Context, req *execpb.ResolveRequest) *execpb.ResolveResponse {
	fail := func(format string, a ...any) *execpb.ResolveResponse {
		return &execpb.ResolveResponse{Error: fmt.Sprintf(format, a...)}
	}
	if len(req.Uses) == 0 {
		return fail("an empty request for action resolution")
	}
	if len(req.Uses) > maxRequestRefs {
		return fail("cannot resolve %d actions at once (the limit is %d)", len(req.Uses), maxRequestRefs)
	}
	for _, u := range req.Uses {
		if err := checkUses(u); err != nil {
			return fail("%v", err)
		}
	}
	// The job ending, or the session, stops the calls.
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(j.ctx, cancel)
	defer stop()

	got := map[string]*execpb.ResolvedAction{}
	var pending []string
	j.mu.Lock()
	for _, u := range req.Uses {
		if _, dup := got[u]; dup {
			continue
		}
		if c, ok := j.resolved[u]; ok {
			got[u] = c
			continue
		}
		got[u] = nil
		pending = append(pending, u)
	}
	j.mu.Unlock()
	for len(pending) > 0 {
		n := min(len(pending), maxResolveRefs)
		chunk := pending[:n]
		pending = pending[n:]
		j.mu.Lock()
		j.resolveRounds++
		j.resolveRefs += len(chunk)
		over := j.resolveRounds > maxResolveRounds || j.resolveRefs > maxResolveRefsTotal
		j.mu.Unlock()
		if over {
			return fail("this job asked to resolve too many actions (the limits are %d requests and %d distinct references)", maxResolveRounds, maxResolveRefsTotal)
		}
		res, errResp := j.callResolve(ctx, chunk)
		if errResp != nil {
			return errResp
		}
		j.mu.Lock()
		for i, u := range chunk {
			got[u] = res[i]
			if res[i].Error == "" { // an error is not cached: it may be a passing one
				if j.resolved == nil {
					j.resolved = map[string]*execpb.ResolvedAction{}
				}
				j.resolved[u] = res[i]
			}
		}
		j.mu.Unlock()
	}
	out := &execpb.ResolveResponse{}
	for _, u := range req.Uses {
		out.Actions = append(out.Actions, got[u])
	}
	return out
}

// callResolve makes one call to the server, with retries, and converts the
// answer; the second result is the error to send to the executor.
func (j *jobRun) callResolve(ctx context.Context, uses []string) ([]*execpb.ResolvedAction, *execpb.ResolveResponse) {
	fail := func(format string, a ...any) ([]*execpb.ResolvedAction, *execpb.ResolveResponse) {
		return nil, &execpb.ResolveResponse{Error: fmt.Sprintf(format, a...)}
	}
	refs := make([]*ermite.ActionRef, len(uses))
	for i, u := range uses {
		refs[i] = &ermite.ActionRef{Uses: u}
	}
	b := backoff{min: j.t.BackoffMin, max: j.t.BackoffMax}
	for attempt := 1; ; attempt++ {
		cctx, ccancel := context.WithTimeout(ctx, j.t.CallTimeout)
		resp, err := j.client.ResolveActions(cctx, connect.NewRequest(&ermite.ResolveActionsRequest{JobId: j.job.JobId, Refs: refs}))
		ccancel()
		if err == nil {
			conv := j.resolveResponse(resp.Msg, len(uses))
			if conv.Error == "" {
				j.cacheArchives(ctx, resp.Msg.Actions, conv.Actions)
			}
			if conv.Error != "" {
				return fail("%s", conv.Error)
			}
			return conv.Actions, nil
		}
		if ctx.Err() != nil {
			return fail("the job was cancelled while its actions were resolved")
		}
		switch classify(err) {
		case kindEnded, kindAbandoned:
			j.abandon(err)
			return fail("the server ended the job")
		case kindPermanent:
			return fail("resolving actions failed: %s", serverReason(err))
		}
		j.log.Warn().Err(err).Int("attempt", attempt).Msg("ResolveActions failed")
		if attempt >= 3 || !sleep(ctx, b.next()) {
			return fail("resolving actions failed: %s", serverReason(err))
		}
	}
}

// serverReason is the code of a failed call, followed by the message of the
// server when it gave one, so the job log names what could not be resolved.
// Only a message the server sent goes in the job log, which the readers of the
// repository see: a failure of the runner's own connection (DNS, TLS, an
// address) stays in the runner's log.
func serverReason(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) && connect.IsWireError(ce) && ce.Message() != "" {
		return fmt.Sprintf("%s: %s", ce.Code(), ce.Message())
	}
	return connect.CodeOf(err).String()
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
