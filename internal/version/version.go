// Package version reports the build identity of the running binary.
//
// Values are injected at link time by the build (see Taskfile.yml and the
// Dockerfile). When they are not — `go run`, `go test`, `go build` with no
// flags — they are recovered from the Go build info instead, so a locally
// built binary still reports something truthful rather than "dev/unknown".
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"sync"
)

// Injected via -ldflags -X. Do not rename without updating the build.
var (
	Version = "dev"
	Commit  = ""
	Date    = ""
)

var once sync.Once

// resolve backfills Commit and Date from the embedded VCS stamps when the
// linker did not supply them. Go records these automatically for builds from a
// git work tree.
func resolve() {
	once.Do(func() {
		info, ok := debug.ReadBuildInfo()
		if !ok {
			return
		}
		for _, s := range info.Settings {
			switch s.Key {
			case "vcs.revision":
				if Commit == "" {
					Commit = s.Value
				}
			case "vcs.time":
				if Date == "" {
					Date = s.Value
				}
			case "vcs.modified":
				if s.Value == "true" {
					Commit += "-dirty"
				}
			}
		}
	})
}

// Short returns the version alone, for the User-Agent string and metrics labels.
func Short() string {
	resolve()
	return Version
}

// String returns the full build identity, for `tome version` and startup logs.
func String() string {
	resolve()
	s := "tomekeeper " + Version
	if Commit != "" {
		s += " (" + Commit + ")"
	}
	if Date != "" {
		s += " built " + Date
	}
	return s + fmt.Sprintf(" %s %s/%s", runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
