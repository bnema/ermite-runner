// SPDX-License-Identifier: MIT

package guest

import (
	"reflect"
	"testing"
)

func TestBootMs(t *testing.T) {
	want := map[string]int64{"setup": 40, "dockerd": 2300, "kernel": 812}
	encoded := formatBootMs(want)
	if encoded != "dockerd=2300,kernel=812,setup=40" {
		t.Fatal(encoded)
	}
	if got := ParseBootMs(encoded); !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if got := ParseBootMs("a=x,b,=3,c=-1,d=5"); !reflect.DeepEqual(got, map[string]int64{"d": 5}) {
		t.Fatal(got)
	}
}

func TestHelloBootMs(t *testing.T) {
	want := map[string]int64{"dockerd": 23}
	_, _, _, hello, _, err := host(t, "true", 0, func(cfg *ExecConfig) { cfg.BootMs = want })
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(hello.BootMs, want) {
		t.Fatalf("Hello boot_ms = %v", hello.BootMs)
	}
}
