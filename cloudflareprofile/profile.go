// Package cloudflareprofile is the default-off guard for running New API as a
// Linux container behind Cloudflare. It does not migrate traffic. The process
// behaves as before unless CLOUDFLARE_PROFILE=1 is injected by the process
// environment.
package cloudflareprofile

import (
	_ "embed"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync/atomic"

	"github.com/QuantumNous/new-api/common"
)

const (
	envProfile = "CLOUDFLARE_PROFILE"
	envRuntime = "NEEDTOKEN_RUNTIME"
	envBoot    = "NEEDTOKEN_CONTAINER_BOOT"
	envSQL     = "SQL_DSN"
	envSession = "SESSION_SECRET"
	envRedis   = "REDIS_CONN_STRING"
	envCrypto  = "CRYPTO_SECRET"
	envLogMode = "NEEDTOKEN_LOG_SQL"
	envLogSQL  = "LOG_SQL_DSN"
	envSQLite  = "SQLITE_PATH"
	envPprof   = "ENABLE_PPROF"
	envToken   = "GENERATE_DEFAULT_TOKEN"

	// RuntimeIsolatedCandidate is the only mode that may pass validation.
	// It is not a production cutover.
	RuntimeIsolatedCandidate = "isolated-candidate"
	// BootTestFixture is an extra lock. It does not prove the DSN host is
	// isolated. SQL also has to match isolated_hosts.json. This repository
	// never sets the switch in a deploy workflow.
	BootTestFixture = "test-fixture"
)

const (
	errProfileFromDisk = "cloudflare profile must be injected by the process environment"
	errSQLAbsent       = "cloudflare profile refuses to start: SQL_DSN binding is absent"
	errSessionAbsent   = "cloudflare profile refuses to start: SESSION_SECRET binding is absent"
	errSessionHolder   = "cloudflare profile refuses the placeholder SESSION_SECRET"
	errSQLite          = "cloudflare profile refuses SQLite"
	errRedisAbsent     = "cloudflare profile refuses to start without REDIS_CONN_STRING"
	errSQLitePath      = "cloudflare profile refuses SQLITE_PATH"
	errPprof           = "cloudflare profile refuses ENABLE_PPROF disk profiles"
	errDefaultToken    = "cloudflare profile refuses generating a default token"
	errRuntime         = "cloudflare profile refuses to start a writer"
	errBoot            = "cloudflare profile refuses to open SQL without the isolated test switch"
	errSQLHost         = "cloudflare profile refuses a SQL host outside the isolated allowlist"
	errCryptoAbsent    = "cloudflare profile refuses to start: CRYPTO_SECRET binding is absent"
	errLogImplicit     = "cloudflare profile refuses an implicit log database"
	errLogMainConflict = "cloudflare profile refuses LOG_SQL_DSN when logs stay on the main database"
	errLogSeparate     = "cloudflare profile refuses a separate log database without LOG_SQL_DSN"
	errSQLPermit       = "cloudflare profile refuses SQL before isolation and the writer permit"
	errDefaultAdmin    = "cloudflare profile refuses the default root account"
	errBootstrap       = "cloudflare profile refuses bootstrap"
)

//go:embed isolated_hosts.json
var isolatedHostsJSON []byte

var sqlPermit atomic.Bool

// Enabled is true only when the process environment asks for the profile.
func Enabled(getenv func(string) string) bool {
	return getenv(envProfile) == "1"
}

// RejectDiskProfile fails when a data-directory .env turned the profile on.
// Bindings have to be present before godotenv runs, so an empty disk cannot
// supply or invent them.
func RejectDiskProfile(fromProcessEnvironment, afterDotEnv bool) error {
	if afterDotEnv && !fromProcessEnvironment {
		return errors.New(errProfileFromDisk)
	}
	return nil
}

// Validate checks binding presence. It does not read secret values into the
// error text, generate a session or crypto key, or open a database.
func Validate(getenv func(string) string) error {
	if !Enabled(getenv) {
		return nil
	}
	dsn := strings.TrimSpace(getenv(envSQL))
	if dsn == "" {
		return errors.New(errSQLAbsent)
	}
	if ForbidsSQLite(getenv, dsn) {
		return errors.New(errSQLite)
	}
	session := strings.TrimSpace(getenv(envSession))
	if session == "" {
		return errors.New(errSessionAbsent)
	}
	if session == "random_string" {
		return errors.New(errSessionHolder)
	}
	if strings.TrimSpace(getenv(envRedis)) == "" {
		return errors.New(errRedisAbsent)
	}
	if strings.TrimSpace(getenv(envSQLite)) != "" {
		return errors.New(errSQLitePath)
	}
	if boolEnv(getenv, envPprof) {
		return errors.New(errPprof)
	}
	if boolEnv(getenv, envToken) {
		return errors.New(errDefaultToken)
	}
	if getenv(envRuntime) != RuntimeIsolatedCandidate {
		return errors.New(errRuntime)
	}
	if getenv(envBoot) != BootTestFixture {
		return errors.New(errBoot)
	}
	if strings.TrimSpace(getenv(envCrypto)) == "" {
		return errors.New(errCryptoAbsent)
	}
	if err := validateLogDatabase(getenv); err != nil {
		return err
	}
	if _, err := requireIsolatedHost(dsn); err != nil {
		return err
	}
	return nil
}

// GrantSQLPermit runs only after isolation has succeeded and the Redis lease
// is held. InitDB, migrations, and the default admin path check PermitSQL
// and do nothing before that. The lease is not read by Cloud Run.
func GrantSQLPermit() {
	sqlPermit.Store(true)
}

// ResetSQLPermitForTest clears the in-process permit. Production startup does
// not call it.
func ResetSQLPermitForTest() {
	sqlPermit.Store(false)
}

// PermitSQL fails closed while the profile is on and the writer permit has
// not been granted. The normal process is allowed through.
func PermitSQL() error {
	if !Enabled(os.Getenv) {
		return nil
	}
	if !sqlPermit.Load() {
		return errors.New(errSQLPermit)
	}
	return nil
}

// RefuseDefaultAdmin rejects the built-in root/123456 account. The profile
// does not create one before or after the writer permit.
func RefuseDefaultAdmin() error {
	if !Enabled(os.Getenv) {
		return nil
	}
	return errors.New(errDefaultAdmin)
}

func validateLogDatabase(getenv func(string) string) error {
	mode := strings.TrimSpace(getenv(envLogMode))
	dsn := strings.TrimSpace(getenv(envLogSQL))
	switch mode {
	case "main":
		if dsn != "" {
			return errors.New(errLogMainConflict)
		}
		return nil
	case "separate":
		if dsn == "" {
			return errors.New(errLogSeparate)
		}
		if _, err := requireIsolatedHost(dsn); err != nil {
			return err
		}
		return nil
	default:
		return errors.New(errLogImplicit)
	}
}

func requireIsolatedHost(dsn string) (string, error) {
	host, err := sqlHost(dsn)
	if err != nil || !isolatedHostAllowed(host) {
		return "", errors.New(errSQLHost)
	}
	return host, nil
}

func sqlHost(dsn string) (string, error) {
	parsed, err := url.Parse(strings.TrimSpace(dsn))
	if err != nil || parsed.Hostname() == "" {
		return "", errors.New(errSQLHost)
	}
	return strings.ToLower(parsed.Hostname()), nil
}

func isolatedHostAllowed(host string) bool {
	var document struct {
		Hosts []string `json:"hosts"`
	}
	if err := common.Unmarshal(isolatedHostsJSON, &document); err != nil {
		return false
	}
	for _, allowed := range document.Hosts {
		if strings.EqualFold(strings.TrimSpace(allowed), host) {
			return true
		}
	}
	return false
}

// ForbidsSQLite reports the SQLite fallback that chooseDB would otherwise
// take. A Postgres or MySQL DSN is allowed through. The returned decision
// does not include the DSN.
func ForbidsSQLite(getenv func(string) string, dsn string) bool {
	if !Enabled(getenv) {
		return false
	}
	dsn = strings.TrimSpace(dsn)
	return dsn == "" || strings.HasPrefix(dsn, "local")
}

// CryptoSecretSource reports the explicit CRYPTO_SECRET binding. The profile
// does not alias a missing value to SESSION_SECRET and does not generate one.
func CryptoSecretSource(getenv func(string) string) string {
	if strings.TrimSpace(getenv(envCrypto)) != "" {
		return "binding"
	}
	return "absent"
}

// RefuseBootstrap rejects an uninitialized database. The setup handler must
// not create a root user or a new secret for this profile.
func RefuseBootstrap(initialized bool) error {
	if !initialized {
		return errors.New(errBootstrap)
	}
	return nil
}

// ScanDataDir classifies the container data directory. A missing directory is
// an empty disk and is allowed only because no inventoried file is required
// to live there. An unclassified name fails closed.
func ScanDataDir(getenv func(string) string) error {
	if !Enabled(getenv) {
		return nil
	}
	dir := strings.TrimSpace(getenv("CLOUDFLARE_DATA_DIR"))
	if dir == "" {
		dir = "/data"
	}
	return ScanPath(dir)
}

// ScanPath reads one directory level. It does not read file contents.
func ScanPath(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return EvaluateDataNames(nil)
		}
		return fmt.Errorf("cloudflare profile cannot read the data directory: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	return EvaluateDataNames(names)
}

func boolEnv(getenv func(string) string, name string) bool {
	switch strings.ToLower(strings.TrimSpace(getenv(name))) {
	case "1", "t", "true", "y", "yes":
		return true
	default:
		return false
	}
}
