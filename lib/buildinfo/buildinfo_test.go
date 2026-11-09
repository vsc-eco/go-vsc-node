package buildinfo

import (
	"runtime/debug"
	"testing"
)

func TestCommitPrefersTheBuildStamp(t *testing.T) {
	old := GitCommit
	defer func() { GitCommit = old }()
	GitCommit = "0123abcd"
	if got := Commit(); got != "0123abcd" {
		t.Fatalf("Commit() = %q, want the stamped value", got)
	}
	GitCommit = ""
	if got := Commit(); got == "" {
		t.Fatal("Commit() without a stamp returned empty, want a revision or \"unknown\"")
	}
}

func TestFromBuildInfo(t *testing.T) {
	set := func(kv ...string) *debug.BuildInfo {
		bi := &debug.BuildInfo{}
		for i := 0; i+1 < len(kv); i += 2 {
			bi.Settings = append(bi.Settings, debug.BuildSetting{Key: kv[i], Value: kv[i+1]})
		}
		return bi
	}
	for _, c := range []struct {
		name string
		bi   *debug.BuildInfo
		ok   bool
		want string
	}{
		{"no build info", nil, false, "unknown"},
		{"built with -buildvcs=false", set("-buildvcs", "false"), true, "unknown"},
		{"clean checkout", set("vcs.revision", "d79c657a", "vcs.modified", "false"), true, "d79c657a"},
		{"local changes", set("vcs.revision", "d79c657a", "vcs.modified", "true"), true, "d79c657a-dirty"},
	} {
		if got := fromBuildInfo(c.bi, c.ok); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
