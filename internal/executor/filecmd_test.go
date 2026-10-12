// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// Cases ported from SetEnvFileCommandL0, SetOutputFileCommandL0 and
// SaveStateFileCommandL0 (actions/runner, src/Test/L0/Worker). The C# tests
// write their files with a UTF-8 BOM and join lines with the platform newline;
// "\n" is used here and the BOM is added by writeEnvFile.

func TestParseEnvFile(t *testing.T) {
	type kv = Pair
	tests := []struct {
		name    string
		in      string
		want    []kv
		wantErr string
	}{
		{"empty file", "", nil, ""},
		{"simple", "MY_ENV=MY VALUE", []kv{{"MY_ENV", "MY VALUE"}}, ""},
		{"skip empty lines", "\nMY_ENV=my value\n\nMY_ENV_2=my second value\n", []kv{{"MY_ENV", "my value"}, {"MY_ENV_2", "my second value"}}, ""},
		{"empty value", "MY_ENV=", []kv{{"MY_ENV", ""}}, ""},
		{"multiple values", "MY_ENV=my value\nMY_ENV_2=\nMY_ENV_3=my third value", []kv{{"MY_ENV", "my value"}, {"MY_ENV_2", ""}, {"MY_ENV_3", "my third value"}}, ""},
		{"special characters", "MY_ENV==abc\nMY_ENV_2=def=ghi\nMY_ENV_3=jkl=", []kv{{"MY_ENV", "=abc"}, {"MY_ENV_2", "def=ghi"}, {"MY_ENV_3", "jkl="}}, ""},
		{"heredoc", "MY_ENV<<EOF\nline one\nline two\nline three\nEOF", []kv{{"MY_ENV", "line one\nline two\nline three"}}, ""},
		{"heredoc trailing newline", "MY_ENV<<EOF\nhello\nworld\nEOF\n", []kv{{"MY_ENV", "hello\nworld"}}, ""},
		{"heredoc empty value", "MY_ENV<<EOF\nEOF", []kv{{"MY_ENV", ""}}, ""},
		{"heredoc skip empty lines", "\nMY_ENV<<EOF\nhello\nworld\nEOF\n\nMY_ENV_2<<EOF\nHELLO\nAGAIN\nEOF\n", []kv{{"MY_ENV", "hello\nworld"}, {"MY_ENV_2", "HELLO\nAGAIN"}}, ""},
		{"heredoc special characters",
			"MY_ENV<<=EOF\nhello\none\n=EOF\nMY_ENV_2<<<EOF\nhello\ntwo\n<EOF\nMY_ENV_3<<EOF\nhello\n\nthree\n\nEOF\nMY_ENV_4<<EOF\nhello=four\nEOF\nMY_ENV_5<<EOF\n EOF\nEOF",
			[]kv{{"MY_ENV", "hello\none"}, {"MY_ENV_2", "hello\ntwo"}, {"MY_ENV_3", "hello\n\nthree\n"}, {"MY_ENV_4", "hello=four"}, {"MY_ENV_5", " EOF"}}, ""},
		{"heredoc delimiter missing", "MY_ENV<<EOF line one line two line three EOF", nil, "Matching delimiter not found"},
		{"heredoc missing delimiter at end of text", "MY_ENV<<EOF\nline one\nline two", nil, "EOF marker missing new line"},
		{"heredoc multi-line missing newline", "MY_ENV<<EOF line one\n line two\n line three EOF", nil, "EOF marker missing new line"},
		{"equals first wins over heredoc", "A=b<<C", []kv{{"A", "b<<C"}}, ""},
		{"heredoc first wins over equals", "A<<B=C\nx\nB=C", []kv{{"A", "x"}}, ""},
		{"no separator", "just text", nil, "Invalid format 'just text'"},
		{"empty name", "=value", nil, "Name must not be empty"},
		{"empty heredoc name", "<<EOF\nx\nEOF", nil, "Name must not be empty and delimiter must not be empty"},
		{"empty delimiter", "A<<\nx", nil, "delimiter must not be empty"},
		{"pairs before the error are kept", "A=1\nbroken", []kv{{"A", "1"}}, "Invalid format 'broken'"},
		{"CR is data", "A=1\r\n", []kv{{"A", "1\r"}}, ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEnvFile(tc.in)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %v, want %q", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// fakeSink records the effects of file commands.
type fakeSink struct {
	env, outputs, state map[string]string
	path                []string
	errors, debugs      []string
	failed              bool
}

func newSink() *fakeSink {
	return &fakeSink{env: map[string]string{}, outputs: map[string]string{}, state: map[string]string{}}
}
func (s *fakeSink) SetEnv(k, v string)    { s.env[k] = v }
func (s *fakeSink) SetOutput(k, v string) { s.outputs[k] = v }
func (s *fakeSink) SaveState(k, v string) { s.state[k] = v }
func (s *fakeSink) AddPath(p string)      { s.path = append(s.path, p) }
func (s *fakeSink) Error(m string)        { s.errors = append(s.errors, m) }
func (s *fakeSink) Debug(m string)        { s.debugs = append(s.debugs, m) }
func (s *fakeSink) CommandFailed()        { s.failed = true }

func writeEnvFile(t *testing.T, path, content string) {
	t.Helper()
	// The C# tests write a BOM; ReadAllText drops it.
	if err := os.WriteFile(path, []byte("\xef\xbb\xbf"+content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestFileCommandsInit(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "_runner_file_commands")
	fc, err := NewFileCommands(dir)
	if err != nil {
		t.Fatal(err)
	}
	env, err := fc.Init()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"GITHUB_PATH", "GITHUB_ENV", "GITHUB_STEP_SUMMARY", "GITHUB_STATE", "GITHUB_OUTPUT"} {
		p := env[name]
		if p == "" || filepath.Dir(p) != dir {
			t.Fatalf("%s = %q", name, p)
		}
		if st, err := os.Stat(p); err != nil || st.Size() != 0 {
			t.Fatalf("%s: %v %v", name, st, err)
		}
	}
	if !strings.HasPrefix(filepath.Base(env["GITHUB_ENV"]), "set_env_") || !strings.HasPrefix(filepath.Base(env["GITHUB_PATH"]), "add_path_") {
		t.Fatalf("prefixes: %v", env)
	}
	// A second Init deletes the previous files and uses new names.
	env2, _ := fc.Init()
	if env2["GITHUB_ENV"] == env["GITHUB_ENV"] {
		t.Fatal("same file for two steps")
	}
	if _, err := os.Stat(env["GITHUB_ENV"]); !os.IsNotExist(err) {
		t.Fatalf("old file kept: %v", err)
	}
}

func TestProcessAllCommands(t *testing.T) {
	fc, _ := NewFileCommands(t.TempDir())
	env, _ := fc.Init()
	writeEnvFile(t, env["GITHUB_ENV"], "A=1\nM<<X\nl1\nl2\nX\n")
	writeEnvFile(t, env["GITHUB_OUTPUT"], "o=2\n")
	writeEnvFile(t, env["GITHUB_STATE"], "s=3\n")
	writeEnvFile(t, env["GITHUB_PATH"], "/a\n\n/b\n/a\n")
	writeEnvFile(t, env["GITHUB_STEP_SUMMARY"], "# hi")
	s := newSink()
	fc.Process(s)
	if !reflect.DeepEqual(s.env, map[string]string{"A": "1", "M": "l1\nl2"}) || s.outputs["o"] != "2" || s.state["s"] != "3" {
		t.Fatalf("%+v", s)
	}
	if !reflect.DeepEqual(s.path, []string{"/a", "/b", "/a"}) {
		t.Fatalf("path %v", s.path)
	}
	if s.failed || len(s.errors) != 0 {
		t.Fatalf("errors %v", s.errors)
	}
}

func TestProcessBlockedEnvNames(t *testing.T) {
	// SetEnvFileCommand_BlockListItemsFiltered and _Heredoc.
	for _, content := range []string{"NODE_OPTIONS=asdf", "NODE_OPTIONS<<EOF\nasdf\nEOF", "node_options=asdf"} {
		fc, _ := NewFileCommands(t.TempDir())
		env, _ := fc.Init()
		writeEnvFile(t, env["GITHUB_ENV"], content)
		s := newSink()
		fc.Process(s)
		if len(s.env) != 0 || len(s.errors) != 1 || s.errors[0] != "Can't store NODE_OPTIONS output parameter using '$GITHUB_ENV' command." || s.failed {
			t.Fatalf("%q: env=%v errors=%v failed=%v", content, s.env, s.errors, s.failed)
		}
	}
}

func TestProcessFormatErrorFailsTheStep(t *testing.T) {
	fc, _ := NewFileCommands(t.TempDir())
	env, _ := fc.Init()
	writeEnvFile(t, env["GITHUB_OUTPUT"], "good=1\nbad line\n")
	writeEnvFile(t, env["GITHUB_ENV"], "E=1\n")
	s := newSink()
	fc.Process(s)
	// Pairs before the error are applied; other commands still run.
	if s.outputs["good"] != "1" || s.env["E"] != "1" || !s.failed {
		t.Fatalf("%+v", s)
	}
	want := []string{"Unable to process file command 'output' successfully.", "Invalid format 'bad line'"}
	if !reflect.DeepEqual(s.errors, want) {
		t.Fatalf("errors %q", s.errors)
	}
}

func TestProcessMissingFilesAreSilent(t *testing.T) {
	// *_DirectoryNotFound and *_NotFound: nothing is set, no issue.
	fc, _ := NewFileCommands(t.TempDir())
	env, _ := fc.Init()
	for _, p := range env {
		_ = os.Remove(p)
	}
	s := newSink()
	fc.Process(s)
	if len(s.env)+len(s.outputs)+len(s.state)+len(s.path) != 0 || len(s.errors) != 0 || s.failed {
		t.Fatalf("%+v", s)
	}
}

func TestStepSummarySize(t *testing.T) {
	fc, _ := NewFileCommands(t.TempDir())
	env, _ := fc.Init()
	if err := os.WriteFile(env["GITHUB_STEP_SUMMARY"], make([]byte, 1024*1024+1), 0o644); err != nil {
		t.Fatal(err)
	}
	s := newSink()
	fc.Process(s)
	if len(s.errors) != 1 || !strings.HasPrefix(s.errors[0], "$GITHUB_STEP_SUMMARY upload aborted, supports content up to a size of 1024k, got 1024k.") {
		t.Fatalf("%q", s.errors)
	}
	if s.failed {
		t.Fatal("an oversized summary only logs an error")
	}
}

func TestSplitLines(t *testing.T) {
	got := splitLines("a\r\nb\rc\n\nd")
	if !reflect.DeepEqual(got, []string{"a", "b", "c", "", "d"}) {
		t.Fatalf("%q", got)
	}
}

func TestPrependedPath(t *testing.T) {
	// PathUtil.PrependPath: the last added entry comes first; no trailing
	// separator on an empty PATH; nothing changes when already first.
	if got := PrependedPath([]string{"/a", "/b"}, "/usr/bin"); got != "/b:/a:/usr/bin" {
		t.Fatal(got)
	}
	if got := PrependedPath([]string{"/a"}, ""); got != "/a" {
		t.Fatal(got)
	}
	if got := PrependedPath([]string{"/a"}, "/a:/usr/bin"); got != "/a:/usr/bin" {
		t.Fatal(got)
	}
	if got := PrependedPath(nil, "/usr/bin"); got != "/usr/bin" {
		t.Fatal(got)
	}
}
