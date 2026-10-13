// SPDX-License-Identifier: MIT

package executor

import "testing"

// Recorded on github.com (matrix-include-exclude): "m two 2", not "m two 2 ".
func TestJobNameIsTrimmed(t *testing.T) {
	needTools(t, "bash")
	yaml := `on: push
jobs:
  main:
    name: m ${{ matrix.os }} ${{ matrix.extra }}
    runs-on: ubuntu-24.04
    strategy:
      matrix:
        os: [one]
        include:
          - os: one
            extra: ''
    steps:
      - run: echo hi
`
	rec, res := runLocal(t, yaml, "main", func(c *Config) { c.MatrixJSON = `{"os":"one","extra":""}` })
	if res.Conclusion != ConclusionSuccess {
		t.Fatalf("%+v", res)
	}
	log := rec.steps[0].Log
	if last := log[len(log)-1]; last != "Complete job name: m one" {
		t.Errorf("%q", last)
	}
}
