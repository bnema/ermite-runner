package executor

import (
	"os"
	"strings"
	"testing"
)

func TestZZDump(t *testing.T) {
	docker := realDocker(t)
	y, _ := os.ReadFile(os.Getenv("DUMP_YAML"))
	rec, _ := runLocal(t, string(y), os.Getenv("DUMP_JOB"), func(c *Config) {
		c.Actions.DockerPath = docker
		c.BaseEnv = append(c.BaseEnv, "XDG_RUNTIME_DIR="+os.Getenv("XDG_RUNTIME_DIR"))
		c.Actions.ExternalsDir = fakeNode(t)
		c.Runner.Environment = "github-hosted"
	})
	var b strings.Builder
	for _, s := range rec.steps {
		b.WriteString("=== STEP " + s.Name + " " + string(s.Conclusion) + "\n" + logOf(s) + "\n")
	}
	os.WriteFile(os.Getenv("DUMP_OUT"), []byte(b.String()), 0o644)
}
