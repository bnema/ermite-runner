// SPDX-License-Identifier: MIT

package externals

import "runtime"

// Artifact is one pinned external tool: where it comes from and the SHA-256 of
// the archive. The pins mirror actions/runner's Misc/externals.sh (node20
// 20.20.2, node24 24.21.0), so a workflow sees the Node.js it would see on a
// GitHub-hosted runner. The checksums of the nodejs.org builds come from the
// official SHASUMS256.txt of each release, and the archives are the .tar.gz
// ones (the Go standard library has no xz).
type Artifact struct {
	// Name is the directory of the GitHub layout, below /home/runner/externals:
	// node20, node24, node20_alpine, node24_alpine.
	Name string
	// Version is the Node.js version without the leading "v".
	Version string
	// Arch is GitHub's architecture name: x64 or arm64.
	Arch string
	// URL is the https location of the .tar.gz.
	URL string
	// SHA256 is the lower-case hex SHA-256 of the archive. Empty means that the
	// checksum has not been recorded yet: the artifact is then reported as
	// unavailable (Pending says why) and nothing is downloaded.
	SHA256 string
	// Pending is the explanation shown to a job that needs an artifact without
	// a recorded checksum.
	Pending string
}

// dirName is the directory of the artifact below the externals directory.
func (a Artifact) dirName() string { return a.Name + "-" + a.Version + "-" + a.Arch }

// The versions of actions/runner's externals.sh.
const (
	node20Version = "20.20.2"
	node24Version = "24.21.0"
)

const (
	nodeDist   = "https://nodejs.org/dist/"
	alpineDist = "https://github.com/actions/alpine_nodejs/releases/download/"
)

func nodejs(name, version, arch, sum string) Artifact {
	return Artifact{
		Name: name, Version: version, Arch: arch, SHA256: sum,
		URL: nodeDist + "v" + version + "/node-v" + version + "-linux-" + arch + ".tar.gz",
	}
}

func alpine(name, version, sum string) Artifact {
	a := Artifact{
		Name: name, Version: version, Arch: "x64", SHA256: sum,
		URL: alpineDist + "v" + version + "/node-v" + version + "-alpine-x64.tar.gz",
	}
	if sum == "" {
		a.Pending = "the checksum of the actions/alpine_nodejs build has not been recorded in this runner yet"
	}
	return a
}

// Pinned is every artifact the runner knows, for all architectures.
var Pinned = []Artifact{
	nodejs("node20", node20Version, "x64", "19e56f0825510207dd904f087fe52faa0a4eb6b2aab5f0ea7a33830d04888b8b"),
	nodejs("node20", node20Version, "arm64", "47ef73d543ecf6eb19435f6c03a0ac4809b3bf0dd6b26c7c571efc2a6572a74d"),
	nodejs("node24", node24Version, "x64", "6e1db87ef58b8819e5d5402eff1536491b18edd8eb7bee5ef7897876e88dc5ff"),
	nodejs("node24", node24Version, "arm64", "724282c3b43aec998aa9527380465b45d229e021b58035f5f4f63095eabfe5d5"),
	// actions/runner ships the alpine builds for x64 only.
	alpine("node20_alpine", node20Version, ""),
	alpine("node24_alpine", node24Version, ""),
}

// HostArch is GitHub's name for the architecture of this host ("" when the
// runner has no build for it).
func HostArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "arm64":
		return "arm64"
	}
	return ""
}

// ForArch selects the artifacts of one architecture, in table order.
func ForArch(all []Artifact, arch string) []Artifact {
	var out []Artifact
	for _, a := range all {
		if a.Arch == arch {
			out = append(out, a)
		}
	}
	return out
}
