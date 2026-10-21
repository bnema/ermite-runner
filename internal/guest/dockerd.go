// SPDX-License-Identifier: MIT

package guest

import (
	"fmt"

	"bnema.dev/ermite-runner/internal/sandbox"
)

func defaultDockerdArgs() []string {
	return []string{"/usr/bin/dockerd", "--data-root", sandbox.GuestScratch + "/docker",
		"--storage-driver", "overlay2", "--host", "unix:///var/run/docker.sock",
		"--registry-mirror", fmt.Sprintf("http://127.0.0.1:%d", sandbox.CacheVsockPort)}
}
