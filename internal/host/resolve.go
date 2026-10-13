// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"encoding/json"
	"fmt"

	"connectrpc.com/connect"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

// maxResolveRefs is the limit of ResolveActionsRequest.refs.
const maxResolveRefs = 100

// resolveActions answers a ResolveRequest of the executor with
// RunnerService.ResolveActions. The job token stays here: the executor only
// gets what the server returns (the per-archive tokens included, which it
// masks). A call that fails without a verdict is retried like the other job
// calls; a failure of the call itself is reported to the executor, which fails
// "Set up job" (ActionManager: FailedToResolveActionDownloadInfoException).
func (j *jobRun) resolveActions(req *execpb.ResolveRequest) *execpb.ResolveResponse {
	if len(req.Uses) > maxResolveRefs {
		return &execpb.ResolveResponse{Error: fmt.Sprintf("cannot resolve %d actions at once (the limit is %d)", len(req.Uses), maxResolveRefs)}
	}
	refs := make([]*ermite.ActionRef, len(req.Uses))
	for i, u := range req.Uses {
		refs[i] = &ermite.ActionRef{Uses: u}
	}
	b := backoff{min: j.t.BackoffMin, max: j.t.BackoffMax}
	for attempt := 1; ; attempt++ {
		cctx, cancel := context.WithTimeout(j.ctx, j.t.CallTimeout)
		resp, err := j.client.ResolveActions(cctx, connect.NewRequest(&ermite.ResolveActionsRequest{JobId: j.job.JobId, Refs: refs}))
		cancel()
		if err == nil {
			return resolveResponse(resp.Msg, len(req.Uses))
		}
		if j.ctx.Err() != nil {
			return &execpb.ResolveResponse{Error: "the job was cancelled while its actions were resolved"}
		}
		switch classify(err) {
		case kindEnded, kindAbandoned:
			j.abandon(err)
			return &execpb.ResolveResponse{Error: "the server ended the job"}
		case kindPermanent:
			return &execpb.ResolveResponse{Error: fmt.Sprintf("resolving actions failed: %s", connect.CodeOf(err))}
		}
		j.log.Warn().Err(err).Int("attempt", attempt).Msg("ResolveActions failed")
		if attempt >= 3 || !sleep(j.ctx, b.next()) {
			return &execpb.ResolveResponse{Error: fmt.Sprintf("resolving actions failed: %s", connect.CodeOf(err))}
		}
	}
}

func resolveResponse(m *ermite.ResolveActionsResponse, want int) *execpb.ResolveResponse {
	if len(m.Actions) != want {
		return &execpb.ResolveResponse{Error: fmt.Sprintf("the server resolved %d actions out of %d", len(m.Actions), want)}
	}
	out := &execpb.ResolveResponse{}
	for _, a := range m.Actions {
		out.Actions = append(out.Actions, &execpb.ResolvedAction{
			Ref: a.Ref, NameWithOwner: a.NameWithOwner, Path: a.Path, ResolvedSha: a.ResolvedSha,
			TarballUrl: a.TarballUrl, Token: a.Token, Source: a.Source.String(), Error: a.Error,
		})
	}
	return out
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
