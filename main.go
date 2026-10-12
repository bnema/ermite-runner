// SPDX-License-Identifier: MIT

// Command ermite-runner runs CI jobs for an ermite server. Each job runs in
// its own Firecracker micro-VM, or in its own container when the host has no
// KVM, and executes workflows compatible with GitHub Actions.
package main

import (
	"fmt"
	"os"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	if len(os.Args) > 1 && os.Args[1] == "version" {
		fmt.Println("ermite-runner", version)
		return
	}
	fmt.Fprintln(os.Stderr, "usage: ermite-runner version")
	os.Exit(2)
}
