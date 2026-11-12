package cloudflareprofile

import (
	"fmt"
	"strings"
)

// DataClass is how a name under the container working directory (/data) is
// treated when the Cloudflare profile is on.
type DataClass string

const (
	// DataDisposable may be absent. Losing it does not lose the service's
	// only copy of state. Logs are also written to stdout.
	DataDisposable DataClass = "disposable"
	// DataForbidden is a SQLite database. The profile never opens it. If a
	// file of this name is present, startup fails instead of ignoring a
	// database that might be the only copy.
	DataForbidden DataClass = "forbidden"
	// DataExternalized is configuration that must come from a binding. The
	// file is not required on disk and is not loaded when the profile was
	// injected by the process environment.
	DataExternalized DataClass = "externalized"
	// DataRequired must still be on the disk. The inventory has none: every
	// file found in the tree is one of the classes above. A new dependency
	// that is not proven disposable has to be added here, and a missing file
	// then fails closed.
	DataRequired DataClass = "required"
)

// DataRule is one inventoried path. Evidence is a code location, not a guess
// that an unknown file can be recreated.
type DataRule struct {
	Name     string
	Class    DataClass
	Evidence string
}

// DataInventory is the complete set of data-directory names this process
// can create or read from the working directory. Dockerfile sets
// WORKDIR /data, so relative paths land there.
func DataInventory() []DataRule {
	return []DataRule{
		{
			Name:     ".env",
			Class:    DataExternalized,
			Evidence: "main.go loads .env with godotenv; the profile skips that load when CLOUDFLARE_PROFILE is already set",
		},
		{
			Name:     "one-api.db",
			Class:    DataForbidden,
			Evidence: "common/database.go SQLitePath default one-api.db, opened only when SQL_DSN is empty or starts with local",
		},
		{
			Name:     "logs",
			Class:    DataDisposable,
			Evidence: "common/init.go default --log-dir ./logs and logger/logger.go; the profile clears the log directory and keeps stdout",
		},
		{
			Name:     "pprof",
			Class:    DataDisposable,
			Evidence: "common/pprof.go writes ./pprof only when ENABLE_PPROF=true, which this profile refuses",
		},
		{
			Name:     "new-api-body-cache",
			Class:    DataDisposable,
			Evidence: "common/disk_cache.go request-body temp files; the default path is the OS temp dir, not a database",
		},
	}
}

// EvaluateDataNames fails closed for an unclassified name, a SQLite file, or
// a required file that is not in present. Disposable and externalized names
// may be absent. present empty is an empty disk.
func EvaluateDataNames(present []string) error {
	required := make([]string, 0)
	for _, rule := range DataInventory() {
		if rule.Class == DataRequired {
			required = append(required, rule.Name)
		}
	}
	have := make(map[string]struct{}, len(present))
	for _, name := range present {
		have[name] = struct{}{}
		class, known := classifyDataName(name)
		if !known {
			return fmt.Errorf("cloudflare profile refuses an unclassified data file: %s", name)
		}
		if class == DataForbidden {
			return fmt.Errorf("cloudflare profile refuses a SQLite data file: %s", name)
		}
	}
	for _, name := range required {
		if _, ok := have[name]; !ok {
			return fmt.Errorf("cloudflare profile refuses to start: required data file is missing: %s", name)
		}
	}
	return nil
}

func classifyDataName(name string) (DataClass, bool) {
	if name == "one-api.db" || strings.HasPrefix(name, "one-api.db-") {
		return DataForbidden, true
	}
	for _, rule := range DataInventory() {
		if rule.Name == name {
			return rule.Class, true
		}
	}
	return "", false
}
