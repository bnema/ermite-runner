// SPDX-License-Identifier: MIT

package executor

import (
	"encoding/base64"
	"strings"
	"testing"
)

// Recorded on github.com (ctx-inputs-vars): secrets.GITHUB_TOKEN is set and
// equals github.token. The token comes from the github context the server
// sends; it is masked in the log.
func TestSecretsHoldTheGithubToken(t *testing.T) {
	needTools(t, "bash")
	const tok = "ghs_testtoken123"
	yaml := `on: push
jobs:
  main:
    runs-on: ubuntu-24.04
    steps:
      - name: absent values
        env:
          V_SET: ${{ secrets.GITHUB_TOKEN != '' }}
          V_EQ: ${{ secrets.GITHUB_TOKEN == github.token }}
          V_NONE: ${{ secrets.NOT_DEFINED }}
          V_TOKEN: ${{ secrets.GITHUB_TOKEN }}
        run: |
          echo "V_SET=$V_SET V_EQ=$V_EQ V_NONE=$V_NONE"
          echo "token=$V_TOKEN b64=` + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok)) + `"
`
	rec, res := runLocal(t, yaml, "main", func(c *Config) {
		c.ContextsJSON = strings.Replace(testGitHubContext("main"), `"github":{`, `"github":{"token":"`+tok+`",`, 1)
	})
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	log := logOf(rec.step(t, "absent values"))
	if !strings.Contains(log, "V_SET=true V_EQ=true V_NONE=\n") {
		t.Errorf("log:\n%s", log)
	}
	if strings.Contains(log, tok) || strings.Contains(log, "eC1hY2Nlc3MtdG9rZW46") {
		t.Errorf("the token leaked into the log:\n%s", log)
	}
	if !strings.Contains(log, "  V_TOKEN: ***") || !strings.Contains(log, "token=*** b64=***") {
		t.Errorf("the token must be masked:\n%s", log)
	}
}

// Without a token from the server the secrets context is empty and nothing is
// masked.
func TestSecretsWithoutToken(t *testing.T) {
	s := newTestState(t)
	if d := s.Values(nil)["secrets"]; d == nil || len(s.Secrets()) != 0 {
		t.Fatalf("%v %v", d, s.Secrets())
	}
}
