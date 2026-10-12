// SPDX-License-Identifier: MIT

package executor

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func fakeBin(t *testing.T, names ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, n := range names {
		if err := os.WriteFile(filepath.Join(dir, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestBuiltinFormatsAndExtensions(t *testing.T) {
	// ScriptHandlerHelpers.cs _defaultArguments and _extensions (Linux rows).
	for shell, want := range map[string][2]string{
		"bash":       {"--noprofile --norc -e -o pipefail {0}", ".sh"},
		"sh":         {"-e {0}", ".sh"},
		"python":     {"{0}", ".py"},
		"pwsh":       {`-command ". '{0}'"`, ".ps1"},
		"powershell": {`-command ". '{0}'"`, ".ps1"},
		"cmd":        {`/D /E:ON /V:OFF /S /C "CALL "{0}""`, ".cmd"},
		"BASH":       {"--noprofile --norc -e -o pipefail {0}", ".sh"}, // case-insensitive tables
		"python3":    {"", ""},
		"ruby":       {"", ""},
	} {
		if got := ScriptArgumentsFormat(shell); got != want[0] {
			t.Errorf("format(%s) = %q", shell, got)
		}
		if got := ScriptFileExtension(shell); got != want[1] {
			t.Errorf("extension(%s) = %q", shell, got)
		}
	}
}

func TestFixUpScriptContents(t *testing.T) {
	if got := FixUpScriptContents("cmd", "echo hi"); got != "@echo off\necho hi" {
		t.Fatal(got)
	}
	want := "$ErrorActionPreference = 'stop'\nGet-Date\nif ((Test-Path -LiteralPath variable:\\LASTEXITCODE)) { exit $LASTEXITCODE }"
	for _, s := range []string{"pwsh", "powershell"} {
		if got := FixUpScriptContents(s, "Get-Date"); got != want {
			t.Fatalf("%s: %q", s, got)
		}
	}
	// The switch is case-sensitive: PWSH is left alone.
	for _, s := range []string{"bash", "sh", "python", "PWSH", "Cmd"} {
		if got := FixUpScriptContents(s, "x"); got != "x" {
			t.Fatalf("%s: %q", s, got)
		}
	}
}

func TestParseShellOption(t *testing.T) {
	for in, want := range map[string][2]string{
		"bash":                {"bash", ""},
		"bash -x {0}":         {"bash", "-x {0}"},
		"python3 {0}":         {"python3", "{0}"},
		"perl  -e {0}":        {"perl", " -e {0}"}, // Split(" ", 2) keeps the second space
		"":                    {"", ""},
		"node -e \"x y\" {0}": {"node", "-e \"x y\" {0}"},
	} {
		c, a := ParseShellOption(in)
		if c != want[0] || a != want[1] {
			t.Errorf("%q: %q %q", in, c, a)
		}
	}
}

func TestPlanShell(t *testing.T) {
	both := fakeBin(t, "bash", "sh", "python", "python3", "perl")
	onlySh := fakeBin(t, "sh")
	l := Lookup{Path: both}
	tests := []struct {
		name  string
		shell string
		l     Lookup
		want  ShellPlan
		err   string
	}{
		{"default uses bash with the arguments of sh", "", l, ShellPlan{"sh", filepath.Join(both, "bash"), "-e {0}"}, ""},
		{"default falls back to sh", "", Lookup{Path: onlySh}, ShellPlan{"sh", filepath.Join(onlySh, "sh"), "-e {0}"}, ""},
		{"bash", "bash", l, ShellPlan{"bash", filepath.Join(both, "bash"), "--noprofile --norc -e -o pipefail {0}"}, ""},
		{"sh", "sh", l, ShellPlan{"sh", filepath.Join(both, "sh"), "-e {0}"}, ""},
		{"python", "python", l, ShellPlan{"python", filepath.Join(both, "python"), "{0}"}, ""},
		{"custom with args", "python3 {0}", l, ShellPlan{"python3", filepath.Join(both, "python3"), "{0}"}, ""},
		{"custom args are left-trimmed", "perl   -w {0}", l, ShellPlan{"perl", filepath.Join(both, "perl"), "-w {0}"}, ""},
		{"bash with explicit args", "bash -x {0}", l, ShellPlan{"bash", filepath.Join(both, "bash"), "-x {0}"}, ""},
		{"unknown command", "nope {0}", l, ShellPlan{}, "nope: command not found"},
		{"pwsh not installed", "pwsh", l, ShellPlan{}, "pwsh: command not found"},
		{"no sh at all", "", Lookup{Path: t.TempDir()}, ShellPlan{}, "sh: command not found"},
		{"custom without {0}", "perl -w", l, ShellPlan{}, "Invalid shell option"},
		{"no format for an unknown shell", "perl", l, ShellPlan{}, "Invalid shell option"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := PlanShell(tc.shell, tc.l)
			if tc.err != "" {
				if err == nil || !strings.Contains(err.Error(), tc.err) {
					t.Fatalf("err = %v, want %q", err, tc.err)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %+v, %v; want %+v", got, err, tc.want)
			}
		})
	}
}

func TestPlanShellUsesPrependedPath(t *testing.T) {
	sys, extra := fakeBin(t, "sh"), fakeBin(t, "bash")
	p, err := PlanShell("", Lookup{Path: sys, Prepend: []string{extra}})
	if err != nil || p.CommandPath != filepath.Join(extra, "bash") {
		t.Fatalf("%+v %v", p, err)
	}
}

func TestShellLine(t *testing.T) {
	p := ShellPlan{"sh", "/usr/bin/bash", "-e {0}"}
	if p.ShellLine() != "/usr/bin/bash -e {0}" || p.Extension() != ".sh" {
		t.Fatal(p.ShellLine(), p.Extension())
	}
}

func TestFormatArguments(t *testing.T) {
	for _, tc := range []struct{ format, path, want, err string }{
		{"-e {0}", "/t/a.sh", "-e /t/a.sh", ""},
		{"{0}", "/t/a b.sh", "/t/a b.sh", ""},
		{"-c '{0}' {0}", "/t/x", "-c '/t/x' /t/x", ""},
		{"{{0}} {0}", "/t/x", "{0} /t/x", ""},
		{"x }}", "/t/x", "x }", ""},
		{"{0}", `/t/a"b`, `/t/a\"b`, ""}, // double quotes in the path are escaped
		{"{0:D}", "/t/x", "/t/x", ""},
		{"{1}", "/t/x", "", "Index"},
		{"{0", "/t/x", "", "not in a correct format"},
		{"a } b", "/t/x", "", "not in a correct format"},
	} {
		got, err := FormatArguments(tc.format, tc.path)
		if tc.err != "" {
			if err == nil || !strings.Contains(err.Error(), tc.err) {
				t.Errorf("%q: err = %v", tc.format, err)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("%q: %q %v", tc.format, got, err)
		}
	}
}

func TestParseArguments(t *testing.T) {
	for in, want := range map[string][]string{
		"":           nil,
		"   ":        nil,
		"-e /t/a.sh": {"-e", "/t/a.sh"},
		"--noprofile --norc -e -o pipefail /t/a.sh": {"--noprofile", "--norc", "-e", "-o", "pipefail", "/t/a.sh"},
		`-c "a b" c`:              {"-c", "a b", "c"},
		`a\"b`:                    {`a"b`},
		`a\\"b c"`:                {`a\b c`},
		`a\\b`:                    {`a\\b`},
		`"a""b"`:                  {`a"b`},
		"a\tb":                    {"a", "b"},
		`-command ". '/t/x.ps1'"`: {"-command", ". '/t/x.ps1'"},
		`""`:                      {""},
	} {
		if got := ParseArguments(in); !reflect.DeepEqual(got, want) {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestWriteScript(t *testing.T) {
	dir := t.TempDir()
	bin := fakeBin(t, "bash", "sh", "python")
	l := Lookup{Path: bin}
	for _, tc := range []struct {
		shell, ext string
		args       []string
	}{
		{"", ".sh", []string{"-e"}},
		{"bash", ".sh", []string{"--noprofile", "--norc", "-e", "-o", "pipefail"}},
		{"python", ".py", nil},
	} {
		plan, err := PlanShell(tc.shell, l)
		if err != nil {
			t.Fatal(err)
		}
		s, err := WriteScript(dir, plan, "echo hi\n")
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(s.Path) != dir || filepath.Ext(s.Path) != tc.ext {
			t.Fatalf("%s: path %s", tc.shell, s.Path)
		}
		want := append(append([]string{plan.CommandPath}, tc.args...), s.Path)
		if !reflect.DeepEqual(s.Argv, want) {
			t.Fatalf("%s: argv %q, want %q", tc.shell, s.Argv, want)
		}
		b, _ := os.ReadFile(s.Path)
		if string(b) != "echo hi\n" {
			t.Fatalf("contents %q", b)
		}
	}
	// A custom shell without an extension in the table: no extension.
	plan, _ := PlanShell("python {0}", l)
	if s, _ := WriteScript(dir, plan, "x"); filepath.Ext(s.Path) != ".py" {
		t.Fatalf("python {0}: %s", s.Path)
	}
	bin2 := fakeBin(t, "perl")
	plan, _ = PlanShell("perl {0}", Lookup{Path: bin2})
	if s, _ := WriteScript(dir, plan, "x"); filepath.Ext(s.Path) != "" {
		t.Fatalf("perl: %s", s.Path)
	}
}

func TestResolveWorkingDirectory(t *testing.T) {
	for _, tc := range [][3]string{
		{"/w/r/r", "", "/w/r/r"},
		{"/w/r/r", "sub", "/w/r/r/sub"},
		{"/w/r/r", "a/b", "/w/r/r/a/b"},
		{"/w/r/r", "/tmp", "/tmp"},
		{"/w/r/r", "..", "/w/r/r/.."},
	} {
		if got := ResolveWorkingDirectory(tc[0], tc[1]); got != tc[2] {
			t.Errorf("%v: %q", tc, got)
		}
	}
}
