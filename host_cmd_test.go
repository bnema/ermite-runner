// SPDX-License-Identifier: MIT

package main

import (
	"os"
	"path/filepath"
	"testing"

	"bnema.dev/ermite-runner/internal/host"
)

func TestFirstRunToken(t *testing.T) {
	env := func(m map[string]string) func(string) string {
		return func(k string) string { return m[k] }
	}
	if tok, err := firstRunToken(env(nil)); err != nil || tok != "" {
		t.Fatalf("no token: %q %v", tok, err)
	}
	if tok, err := firstRunToken(env(map[string]string{host.EnvRegisterToken: " t1 \n"})); err != nil || tok != "t1" {
		t.Fatalf("env: %q %v", tok, err)
	}
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte("t2\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The file wins over the environment.
	if tok, err := firstRunToken(env(map[string]string{envRegisterTokenFile: p, host.EnvRegisterToken: "t1"})); err != nil || tok != "t2" {
		t.Fatalf("file: %q %v", tok, err)
	}
	if _, err := firstRunToken(env(map[string]string{envRegisterTokenFile: filepath.Join(t.TempDir(), "missing")})); err == nil {
		t.Fatal("a missing token file was accepted")
	}
}
