// Package buildinfo reports release metadata and Go's embedded build identity.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// These values may be supplied by the release build using -ldflags -X.
var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// Info is the same build identity exposed by the CLI and MCP server.
type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	Date      string `json:"date"`
	Modified  bool   `json:"modified"`
	GoVersion string `json:"go_version"`
	Module    string `json:"module"`
}

// Current prefers release metadata, falling back to Go module/VCS metadata.
// go install module@version embeds the module version even without ldflags.
func Current() Info {
	build, _ := debug.ReadBuildInfo()
	return fromBuildInfo(build, Version, Commit, Date)
}

func fromBuildInfo(build *debug.BuildInfo, version, commit, date string) Info {
	info := Info{Version: version, Commit: commit, Date: date,
		GoVersion: runtime.Version(), Module: "github.com/c0ze/tincan"}
	if build == nil {
		return info
	}
	if build.Main.Path != "" {
		info.Module = build.Main.Path
	}
	if build.GoVersion != "" {
		info.GoVersion = build.GoVersion
	}
	if (version == "dev" || version == "") && build.Main.Version != "" && build.Main.Version != "(devel)" {
		info.Version = build.Main.Version
	}
	for _, setting := range build.Settings {
		switch setting.Key {
		case "vcs.revision":
			if commit == "unknown" || commit == "" {
				info.Commit = setting.Value
			}
		case "vcs.time":
			if date == "unknown" || date == "" {
				info.Date = setting.Value
			}
		case "vcs.modified":
			info.Modified = setting.Value == "true"
		}
	}
	return info
}

func (info Info) String() string {
	return fmt.Sprintf("tincan %s commit=%s date=%s go=%s modified=%t",
		info.Version, info.Commit, info.Date, info.GoVersion, info.Modified)
}
