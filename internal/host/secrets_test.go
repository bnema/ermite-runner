// SPDX-License-Identifier: MIT

package host

import (
	"context"
	"fmt"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"google.golang.org/protobuf/proto"

	ermite "bnema.dev/ermite-api/gen/ermite"

	"bnema.dev/ermite-runner/internal/execstream/execpb"
)

const (
	secretValue  = "s3cr3t-value-DO-NOT-LOG"
	secretValue2 = "-----BEGIN-----\nline-two-DO-NOT-LOG\n-----END-----"
)

func withSecrets(trusted bool) func(*harness) {
	return func(h *harness) {
		j := testJob("j1")
		j.Secrets = map[string]string{"DB_PASSWORD": secretValue, "KEY": secretValue2}
		j.Vars = map[string]string{"REGION": "eu"}
		j.Trusted = trusted
		h.srv.jobs = nil
		h.srv.jobs = append(h.srv.jobs, j)
	}
}

func runWithSecrets(t *testing.T, features []string, trusted bool) (*harness, *execpb.Run, string) {
	t.Helper()
	seenRun.Store(nil)
	state := t.TempDir()
	h := newHarness(t, resultsScript(nil))
	h.runner.Cfg.StateDir = state
	h.runner.Features = func(context.Context) ([]string, error) { return features, nil }
	withSecrets(trusted)(h)
	stop := h.run(t)
	eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
	if err := stop(); err != nil {
		t.Fatal(err)
	}
	return h, seenRun.Load(), state
}

func TestSecretsAndVarsReachTheExecutorOnly(t *testing.T) {
	h, run, state := runWithSecrets(t, []string{FeatureSecrets, FeatureAnnotations}, true)
	if run == nil {
		t.Fatal("no Run frame")
	}
	vals := lastValues.Load()
	if vals == nil || vals.secrets["DB_PASSWORD"] != secretValue || vals.secrets["KEY"] != secretValue2 || len(vals.secrets) != 2 {
		t.Errorf("secrets not passed: %+v", vals)
	}
	if vals.vars["REGION"] != "eu" || run.Job.SecretsWithheld {
		t.Errorf("vars %v withheld %v", vals.vars, run.Job.SecretsWithheld)
	}
	// The Run frame itself holds no secret.
	if rb, _ := proto.Marshal(run); strings.Contains(string(rb), "DO-NOT-LOG") {
		t.Error("the Run frame holds a secret")
	}

	// Nothing the host does prints the secrets: its log (debug level) and its
	// state directory. (protobuf-go does not redact String(), so the guarantee
	// is that no code formats the Job, the Run or the JobSpec; see the grep in
	// TestNoCodePrintsTheJobMessages.)
	if strings.Contains(h.logs.String(), "DO-NOT-LOG") {
		t.Errorf("a secret is in the host log:\n%.500s", h.logs.String())
	}
	err := filepath.Walk(state, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		b, _ := os.ReadFile(p)
		if strings.Contains(string(b), "DO-NOT-LOG") {
			t.Errorf("a secret is in the state directory: %s", p)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// The "not available" line only exists when the server has ci.secrets: an old
// server does not send Job.trusted, which then reads false.
func TestSecretsWithheldOnlyWithTheFeature(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
		trusted  bool
		want     bool
	}{
		{"untrusted, server has ci.secrets", []string{FeatureSecrets}, false, true},
		{"trusted, server has ci.secrets", []string{FeatureSecrets}, true, false},
		{"untrusted, old server", []string{FeatureAnnotations}, false, false},
		{"untrusted, features unreachable", nil, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, run, _ := runWithSecrets(t, tc.features, tc.trusted)
			if run == nil || run.Job.SecretsWithheld != tc.want {
				t.Errorf("secrets_withheld = %v, want %v", run != nil && run.Job.SecretsWithheld, tc.want)
			}
		})
	}
}

// protobuf-go prints map values in String(), so nothing may format a message
// that holds secrets: the Job (ermite.Job, with Secrets) or the Run and JobSpec
// that wrap it. This is a heuristic on names, not a proof: printLeaks looks,
// in the sources of the host, the guest, execstream and the executor, for
// formatting, logging, marshalling and String() calls that are given a value
// named like those messages (job, j.job, r.job, spec, run, x.GetJob()), their
// Secrets field or GetSecrets(). It does not follow a message through other
// names or through values it was copied into. The real guarantees are the
// tests that read the host log and the state directory
// (TestSecretsAndVarsReachTheExecutorOnly) and the absence of secrets in the
// Run frame.
func TestNoCodePrintsTheJobMessages(t *testing.T) {
	fset := token.NewFileSet()
	for _, dir := range []string{".", "../guest", "../execstream", "../executor"} {
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			t.Fatal(err)
		}
		for _, file := range files {
			if strings.HasSuffix(file, "_test.go") {
				continue
			}
			src, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, leak := range printLeaks(t, fset, file, src) {
				t.Error(leak)
			}
		}
	}
}

var (
	// printers are the calls that render their arguments: fmt, loggers (zerolog
	// events), the text and JSON encoders of messages.
	printers = map[string]bool{"Errorf": true, "Printf": true, "Sprintf": true, "Fprintf": true, "Println": true, "Fprintln": true,
		"Print": true, "Sprint": true, "Sprintln": true, "Interface": true, "Any": true, "Stringer": true, "Msgf": true, "Msg": true, "Str": true,
		"Fatalf": true, "Logf": true, "Infof": true, "Warnf": true, "Format": true, "Marshal": true, "MarshalToString": true,
		"Err": true, "Object": true, "Dict": true, "Strs": true}
	// the messages by name
	messageName = `(?:(?:j|r|rt)\.job|job|spec|run|j\.run|[A-Za-z_.]+\.GetJob\(\)|[A-Za-z_.]+\.Job)`
	messageRe   = regexp.MustCompile(`^` + messageName + `$`)
	// message.String(), and the secrets of a message or of anything
	stringRe  = regexp.MustCompile(`^` + messageName + `\.String\(\)$`)
	secretsRe = regexp.MustCompile(`\.(?:Secrets|GetSecrets\(\))$`)
)

func leaky(text string) bool {
	return messageRe.MatchString(text) || stringRe.MatchString(text) || secretsRe.MatchString(text)
}

// printLeaks returns the places of src where a message with secrets, or its
// secrets, is given to a printing call, or where String() is called on such a
// message.
func printLeaks(t *testing.T, fset *token.FileSet, name string, src []byte) []string {
	t.Helper()
	f, err := parser.ParseFile(fset, name, src, 0)
	if err != nil {
		t.Fatal(err)
	}
	text := func(e ast.Expr) string {
		var b strings.Builder
		_ = format.Node(&b, fset, e)
		return b.String()
	}
	var out []string
	report := func(e ast.Expr, what string) {
		out = append(out, fmt.Sprintf("%s: %s: %s would print secrets", fset.Position(e.Pos()), what, text(e)))
	}
	// visit looks for leaky expressions inside an argument of a printing call. A
	// field read of a message (j.job.JobId) is a scalar: the message is not
	// descended into.
	var visit func(e ast.Expr, what string)
	visit = func(e ast.Expr, what string) {
		if e == nil {
			return
		}
		if leaky(text(e)) {
			report(e, what)
			return
		}
		switch x := e.(type) {
		case *ast.SelectorExpr:
			if !messageRe.MatchString(text(x.X)) {
				visit(x.X, what)
			}
		case *ast.CallExpr:
			if id, ok := x.Fun.(*ast.Ident); ok && (id.Name == "len" || id.Name == "cap") {
				return // a count is not a secret
			}
			if sel, ok := x.Fun.(*ast.SelectorExpr); ok {
				if !messageRe.MatchString(text(sel.X)) {
					visit(sel.X, what)
				}
			} else {
				visit(x.Fun, what)
			}
			for _, a := range x.Args {
				visit(a, what)
			}
		case *ast.UnaryExpr:
			visit(x.X, what)
		case *ast.StarExpr:
			visit(x.X, what)
		case *ast.ParenExpr:
			visit(x.X, what)
		case *ast.BinaryExpr:
			visit(x.X, what)
			visit(x.Y, what)
		case *ast.IndexExpr:
			visit(x.X, what)
		case *ast.CompositeLit:
			for _, el := range x.Elts {
				if kv, ok := el.(*ast.KeyValueExpr); ok {
					visit(kv.Value, what)
				} else {
					visit(el, what)
				}
			}
		}
	}
	ast.Inspect(f, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		if stringRe.MatchString(text(call)) {
			report(call, "String()")
			return true
		}
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && printers[sel.Sel.Name] {
			for _, a := range call.Args {
				visit(a, sel.Sel.Name+"()")
			}
		}
		return true
	})
	return out
}

// The guard finds what it is meant to find and leaves scalar fields alone.
func TestPrintLeaksHeuristic(t *testing.T) {
	positive := []string{
		`fmt.Sprintf("%v", spec)`,
		`fmt.Errorf("bad %+v", j.job)`,
		`log.Info().Interface("job", j.job).Msg("x")`,
		`log.Info().Any("run", run).Msg("x")`,
		`_ = spec.String()`,
		`_ = j.job.String()`,
		`x := run.GetJob().String()`,
		`log.Debug().Str("s", fmt.Sprint(j.job.Secrets))`,
		`log.Debug().Interface("s", j.job.Secrets).Msg("x")`,
		`log.Debug().Interface("s", job.GetSecrets()).Msg("x")`,
		`fmt.Println(run.GetJob())`,
		`fmt.Println("job", r.job)`,
		`_ = prototext.Format(run)`,
		`_, _ = json.Marshal(j.job)`,
		`fmt.Println(strings.ToUpper(spec.String()))`,
		`fmt.Println(&j.job)`,
		`fmt.Println([]any{j.job})`,
	}
	negative := []string{
		`fmt.Sprintf("%s", j.job.JobId)`,
		`log.Info().Str("job_id", job.JobId).Int64("run_id", job.RunId).Msg("x")`,
		`fmt.Errorf("job %s: %w", j.job.JobKey, err)`,
		`log.Info().Str("k", run.GetJob().GetJobKey()).Msg("x")`,
		`_ = fmt.Sprint(len(j.job.Secrets))`,
		`h := JobHandler{Secrets: j.job.Secrets, Vars: j.job.Vars}`,
		`_ = proto.Marshal(frame)`,
		`fmt.Println(spec.GetRepo())`,
		`_ = other.String()`,
		`secrets := j.job.Secrets; _ = secrets`,
	}
	fset := token.NewFileSet()
	for _, c := range positive {
		if got := printLeaks(t, fset, "p.go", []byte("package p\nfunc f() {\n"+c+"\n}\n")); len(got) == 0 {
			t.Errorf("not detected: %s", c)
		}
	}
	for _, c := range negative {
		if got := printLeaks(t, fset, "n.go", []byte("package p\nfunc f() {\n"+c+"\n}\n")); len(got) != 0 {
			t.Errorf("false positive: %s: %v", c, got)
		}
	}
}

// Job.permissions reaches the executor in the Run frame when the server has
// ci.secrets (it sent them); an old server's absent field stays unset, so that
// the executor works them out from the workflow.
func TestTokenPermissionsReachTheExecutor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		features []string
		perms    *ermite.JobPermissions
		want     map[string]string // nil: unset in the frame
	}{
		{"sent", []string{FeatureSecrets}, &ermite.JobPermissions{Levels: map[string]string{"contents": "write", "metadata": "read"}},
			map[string]string{"contents": "write", "metadata": "read"}},
		{"sent and empty", []string{FeatureSecrets}, &ermite.JobPermissions{}, map[string]string{}},
		{"not sent", []string{FeatureSecrets}, nil, nil},
		{"old server", []string{FeatureAnnotations}, &ermite.JobPermissions{Levels: map[string]string{"contents": "write"}}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seenRun.Store(nil)
			h := newHarness(t, resultsScript(nil))
			h.runner.Cfg.StateDir = t.TempDir()
			h.runner.Features = func(context.Context) ([]string, error) { return tc.features, nil }
			j := testJob("j1")
			j.Permissions = tc.perms
			h.srv.jobs = []*ermite.Job{j}
			stop := h.run(t)
			eventually(t, "CompleteJob", func() bool { return h.srv.completeCount() == 1 })
			if err := stop(); err != nil {
				t.Fatal(err)
			}
			run := seenRun.Load()
			if run == nil {
				t.Fatal("no Run frame")
			}
			got := run.Job.GetTokenPermissions()
			switch {
			case tc.want == nil && got != nil:
				t.Errorf("unexpected permissions %v", got.GetLevels())
			case tc.want != nil && (got == nil || !maps.Equal(got.GetLevels(), tc.want)):
				t.Errorf("permissions %v, want %v", got.GetLevels(), tc.want)
			}
		})
	}
}
