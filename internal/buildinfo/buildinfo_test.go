package buildinfo

import (
	"encoding/json"
	"runtime/debug"
	"strings"
	"testing"
)

func TestModuleInstallIdentity(t *testing.T) {
	build := &debug.BuildInfo{
		GoVersion: "go1.25.0",
		Main:      debug.Module{Path: "github.com/c0ze/tincan/v2", Version: "v2.0.0"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "abcdef"},
			{Key: "vcs.time", Value: "2026-09-08T12:00:00Z"},
			{Key: "vcs.modified", Value: "true"},
		},
	}
	info := fromBuildInfo(build, "dev", "unknown", "unknown")
	if info.Module != build.Main.Path || info.Version != build.Main.Version || info.Commit != "abcdef" || info.Date != "2026-09-08T12:00:00Z" || !info.Modified || info.GoVersion != "go1.25.0" {
		t.Fatalf("module install identity lost: %+v", info)
	}
	data, err := json.Marshal(info)
	if err != nil || !strings.Contains(string(data), `"modified":true`) || !strings.Contains(info.String(), info.Version) {
		t.Fatalf("identity not printable: %s, %v", data, err)
	}
}

func TestReleaseIdentityOverridesModuleMetadata(t *testing.T) {
	build := &debug.BuildInfo{
		Main: debug.Module{Version: "(devel)"},
		Settings: []debug.BuildSetting{
			{Key: "vcs.revision", Value: "old-commit"},
			{Key: "vcs.time", Value: "old-date"},
		},
	}
	info := fromBuildInfo(build, "v2.0.0", "release-commit", "release-date")
	if info.Version != "v2.0.0" || info.Commit != "release-commit" || info.Date != "release-date" {
		t.Fatalf("release identity overwritten: %+v", info)
	}
}

func TestDevelopmentWithoutBuildMetadata(t *testing.T) {
	for _, build := range []*debug.BuildInfo{nil, {Main: debug.Module{Version: "(devel)"}}} {
		info := fromBuildInfo(build, "dev", "unknown", "unknown")
		if info.Version != "dev" || info.Commit != "unknown" || info.GoVersion == "" || info.Module != "github.com/c0ze/tincan/v2" {
			t.Fatalf("invalid development identity: %+v", info)
		}
	}
}
