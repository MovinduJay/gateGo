package buildinfo

import (
	"encoding/json"
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	Version = "dev"
	Commit  = "unknown"
	Date    = "unknown"
)

type Info struct {
	Version   string `json:"version"`
	Commit    string `json:"commit"`
	BuildDate string `json:"build_date"`
	Dirty     bool   `json:"dirty"`
	GoVersion string `json:"go_version"`
}

func Current() Info {
	info := Info{Version: Version, Commit: Commit, BuildDate: Date, GoVersion: runtime.Version()}
	if build, ok := debug.ReadBuildInfo(); ok {
		if info.Version == "dev" && build.Main.Version != "" && build.Main.Version != "(devel)" {
			info.Version = build.Main.Version
		}
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				if info.Commit == "unknown" {
					info.Commit = setting.Value
				}
			case "vcs.time":
				if info.BuildDate == "unknown" {
					info.BuildDate = setting.Value
				}
			case "vcs.modified":
				info.Dirty = strings.EqualFold(setting.Value, "true")
			}
		}
	}
	return info
}

func Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			w.WriteHeader(http.StatusMethodNotAllowed)
			_ = json.NewEncoder(w).Encode(map[string]string{"error": "GET required"})
			return
		}
		_ = json.NewEncoder(w).Encode(Current())
	})
}
