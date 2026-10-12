// SPDX-License-Identifier: MIT

package executor

import (
	"sort"
	"strings"

	"bnema.dev/ermite-actions/workflow"
)

// tokenScope is a scope of the GITHUB_TOKEN as GitHub's runner lists it.
type tokenScope struct {
	// key is the name in a workflow's "permissions" mapping.
	key string
	// display is the name "Set up job" prints.
	display string
	// writeAll is the level "write-all" grants: GitHub keeps models and
	// vulnerability-alerts at read, whatever the workflow asks.
	writeAll string
}

// tokenScopes are the scopes GitHub lists in "GITHUB_TOKEN Permissions" for
// "read-all" and "write-all" (recordings/permissions-display.json: p-read-all,
// p-write-all). "id-token" is not here: it is never listed, with any level
// (p-job-mixed asks for id-token: write). "copilot-requests" is listed for
// write-all only (read-all grants nothing for it, and "read" is rejected).
// "metadata" is not a scope a workflow can set (rejected); it is always read.
var tokenScopes = []tokenScope{
	{"actions", "Actions", "write"},
	{"artifact-metadata", "ArtifactMetadata", "write"},
	{"attestations", "Attestations", "write"},
	{"checks", "Checks", "write"},
	{"code-quality", "CodeQuality", "write"},
	{"contents", "Contents", "write"},
	{"copilot-requests", "CopilotRequests", "write"},
	{"deployments", "Deployments", "write"},
	{"discussions", "Discussions", "write"},
	{"drives", "Drives", "write"},
	{"issues", "Issues", "write"},
	{"models", "Models", "read"},
	{"packages", "Packages", "write"},
	{"pages", "Pages", "write"},
	{"pull-requests", "PullRequests", "write"},
	{"repository-projects", "RepositoryProjects", "write"},
	{"security-events", "SecurityEvents", "write"},
	{"statuses", "Statuses", "write"},
	{"vulnerability-alerts", "VulnerabilityAlerts", "read"},
}

// permissionLine is one line of the group: "<Display>: <level>".
type permissionLine struct{ name, level string }

// tokenPermissionLines returns the lines of the "GITHUB_TOKEN Permissions"
// group for the effective permissions of a job, as GitHub prints them on a
// push by a repository owner (the recorded case):
//
//   - the permissions of the job replace those of the workflow (no merge);
//     without either key the repository default applies;
//   - lines are sorted by display name (ordinal), Metadata included;
//   - Metadata is always listed, as read, whatever the workflow says;
//   - a scope set to "none" or left out is not listed, nor is id-token;
//   - "permissions: {}" lists Metadata only;
//   - "read-all" lists every scope but copilot-requests as read;
//   - "write-all" lists every scope as write except models and
//     vulnerability-alerts (read);
//   - a mapping lists its scopes with the given level; a scope that GitHub
//     does not know is left out (it rejects the workflow before this point
//     for the invalid ones it was shown).
//
// Not recorded: a private repository, a fork pull request, an organisation
// whose default token is restricted differently, a "pull_request_target"
// event. They change what the server grants; the workflow side is the same.
func tokenPermissionLines(wf *workflow.Workflow, jb *workflow.Job) []permissionLine {
	p := jb.Permissions
	if p == nil {
		p = wf.Permissions
	}
	if p == nil {
		return []permissionLine{{"Contents", "read"}, {"Metadata", "read"}, {"Packages", "read"}}
	}
	lines := []permissionLine{{"Metadata", "read"}}
	switch strings.ToLower(p.All) {
	case "read-all":
		for _, s := range tokenScopes {
			if s.key != "copilot-requests" {
				lines = append(lines, permissionLine{s.display, "read"})
			}
		}
	case "write-all":
		for _, s := range tokenScopes {
			lines = append(lines, permissionLine{s.display, s.writeAll})
		}
	default:
		level := map[string]string{}
		for _, sc := range p.Scopes {
			level[strings.ToLower(sc.Scope)] = strings.ToLower(sc.Level)
		}
		for _, s := range tokenScopes {
			if l := level[s.key]; l == "read" || l == "write" {
				lines = append(lines, permissionLine{s.display, l})
			}
		}
	}
	sort.Slice(lines, func(a, b int) bool { return lines[a].name < lines[b].name })
	return lines
}
