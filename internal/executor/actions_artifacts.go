// SPDX-License-Identifier: MIT

package executor

import (
	"bytes"
	"io"
	"os"
	"strings"

	"bnema.dev/ermite-actions/action"
)

// @actions/artifact v2 (upload-artifact and download-artifact v4 and later)
// refuses to run when it believes the server is GitHub Enterprise Server:
// isGhes() (toolkit packages/artifact/src/internal/shared/config.ts) reads
// GITHUB_SERVER_URL only, and any host that is not github.com, *.ghe.com or
// *.localhost is GHES. ermite's server URL is its own, so the node steps that
// run that client get a server URL the client takes for GitHub, in the
// environment of their process only. It is the one place where ermite changes
// the environment of an action.
const (
	// artifactsServerURL is the GITHUB_SERVER_URL those steps get. isGhes()
	// treats *.localhost as GitHub; the host is never resolved nor connected to
	// (the client talks to ACTIONS_RESULTS_URL only).
	artifactsServerURL = "https://ermite.ghe.localhost"
	// ghesMessage is the error @actions/artifact v2 throws on GHES
	// (packages/artifact/src/internal/shared/errors.ts:32): a script that holds
	// it bundles the client.
	ghesMessage = "@actions/artifact v2.0.0+, upload-artifact@v4+ and download-artifact@v4+ are not currently supported on GHES."
	// sniffChunk is the size of the reads of the sniff.
	sniffChunk = 64 << 10
)

// artifactRepos are the repository actions that use @actions/artifact v2, any
// path inside them included (actions/upload-artifact/merge).
var artifactRepos = []string{"actions/upload-artifact", "actions/download-artifact"}

// setArtifactsServerURL decides the GITHUB_SERVER_URL of the process of a node
// action step that runs script (main, pre or post): artifactsServerURL when the
// job has the results service and the step is an artifact action or its script
// bundles @actions/artifact v2, else the job's own value.
func (j *job) setArtifactsServerURL(s *stepRun, script string) {
	s.serverURL = ""
	if !j.cfg.Results {
		return
	}
	if !isArtifactAction(s.n) && !j.bundlesArtifactClient(script) {
		return
	}
	s.serverURL = artifactsServerURL
	j.debug(s.num, "GITHUB_SERVER_URL set to "+artifactsServerURL+" for the artifacts service")
}

// isArtifactAction reports whether the step uses one of artifactRepos, in any
// case.
func isArtifactAction(n *node) bool {
	if n == nil || n.uses.Kind != action.UsesRepository {
		return false
	}
	name := n.uses.Owner + "/" + n.uses.Repo
	for _, r := range artifactRepos {
		if strings.EqualFold(name, r) {
			return true
		}
	}
	return false
}

// bundlesArtifactClient reports whether the file holds ghesMessage. The answer
// is kept per path for the job: the pre, main and post stages and other uses of
// the action read it once. A file that cannot be read does not hold it.
func (j *job) bundlesArtifactClient(script string) bool {
	j.mu.Lock()
	hit, ok := j.ghesSniff[script]
	j.mu.Unlock()
	if ok {
		return hit
	}
	hit = fileContains(script, []byte(ghesMessage))
	j.mu.Lock()
	if j.ghesSniff == nil {
		j.ghesSniff = map[string]bool{}
	}
	j.ghesSniff[script] = hit
	j.mu.Unlock()
	return hit
}

// fileContains reads the file in chunks and reports whether it holds needle;
// len(needle)-1 bytes of a chunk are kept for the next, so a match across two
// reads is found. A read error is a miss.
func fileContains(name string, needle []byte) bool {
	if len(needle) == 0 {
		return true
	}
	f, err := os.Open(name)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	keep := len(needle) - 1
	buf := make([]byte, keep+sniffChunk)
	have := 0
	for {
		n, err := f.Read(buf[have:])
		have += n
		if bytes.Contains(buf[:have], needle) {
			return true
		}
		if err == io.EOF {
			return false
		}
		if err != nil {
			return false
		}
		if have > keep {
			have = copy(buf, buf[have-keep:have])
		}
	}
}
