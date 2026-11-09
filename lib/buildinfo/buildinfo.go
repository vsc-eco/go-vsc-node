// Package buildinfo reports the source commit a binary was built from, so an
// operator can tell from a bot's /health which code it is running.
package buildinfo

import "runtime/debug"

// GitCommit is stamped at build time:
//
//	go build -ldflags "-X vsc-node/lib/buildinfo.GitCommit=$(git rev-parse HEAD)" ...
//
// The Makefile and Dockerfile.mapping-bot do this.
var GitCommit string

// Commit returns the stamped GitCommit. Without a stamp it returns the
// revision Go records for a plain `go build` inside a git checkout (with
// "-dirty" when the tree had uncommitted changes), else "unknown".
func Commit() string {
	if GitCommit != "" {
		return GitCommit
	}
	return fromBuildInfo(debug.ReadBuildInfo())
}

func fromBuildInfo(bi *debug.BuildInfo, ok bool) string {
	if !ok || bi == nil {
		return "unknown"
	}
	rev, dirty := "", false
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}
	if rev == "" {
		return "unknown"
	}
	if dirty {
		return rev + "-dirty"
	}
	return rev
}
