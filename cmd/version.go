package cmd

import "runtime/debug"

// version is set at build time with -ldflags "-X github.com/johnmuhlestein/skima/cmd.version=v1.2.3" - otherwise it
// comes from the build info: the module version for go install ...@version, or the git commit for a local build
var version string

func versionString() string {
	if version != "" {
		return version
	}
	info, ok := debug.ReadBuildInfo()
	if !ok {
		return "unknown"
	}
	if v := info.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	var revision, modified string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.modified":
			modified = s.Value
		}
	}
	if revision == "" {
		return "devel"
	}
	if len(revision) > 12 {
		revision = revision[:12]
	}
	if modified == "true" {
		revision += "-dirty"
	}
	return "devel+" + revision
}
