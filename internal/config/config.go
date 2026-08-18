// Package config loads service configuration from the environment.
//
// Configuration is fail-closed: a missing security-relevant setting is an error,
// never a permissive default. Secrets are read from systemd credentials or the
// environment at runtime and are never logged (plan section 11.4).
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Environment identifies the deployment environment. Staging and production
// configuration are always separate (plan section 3.3).
type Environment string

const (
	EnvLocal      Environment = "local"
	EnvStaging    Environment = "staging"
	EnvProduction Environment = "production"
)

// Valid reports whether e is a recognised environment.
func (e Environment) Valid() bool {
	switch e {
	case EnvLocal, EnvStaging, EnvProduction:
		return true
	}
	return false
}

// ControlAPI is the configuration for the control API service.
type ControlAPI struct {
	Environment  Environment
	ListenAddr   string
	DatabaseURL  string
	ShutdownWait time.Duration

	// AccessTeamDomain and AccessAudience are required to validate the
	// Cloudflare Access JWT. The application validates the token itself; it
	// never trusts request headers alone (plan section 8.1).
	AccessTeamDomain string
	AccessAudience   string

	// CellAccessAudience is the AUD of the Access application that fronts the
	// git-credential endpoint. Accepted ONLY on that path.
	CellAccessAudience string

	// CellHealthAccessAudience is the AUD of the Access application that fronts
	// the agent-health endpoint (ADR-0019).
	//
	// A second value rather than a reuse of the one above, because the two
	// endpoints grant different powers to the same token: one mints a git
	// credential, the other writes into people's inboxes. Binding each to its
	// own audience keeps a token minted for one from working on the other.
	CellHealthAccessAudience string

	// GitHubWebhookSecret authenticates inbound webhooks. GitHub cannot pass a
	// Cloudflare Access challenge, so this shared secret is the only thing
	// standing between the endpoint and anyone who learns its URL.
	GitHubWebhookSecret string

	// The GitHub App itself, used to mint short-lived git credentials for
	// execution cells. Held only on the control node: the key can mint tokens
	// for every installed repository, so an execution node must never see it.
	GitHubAppID          string
	GitHubInstallationID string
	GitHubPrivateKeyPath string
}

// ErrMissing reports a required setting that was not supplied.
type ErrMissing struct{ Key string }

func (e ErrMissing) Error() string { return "required configuration missing: " + e.Key }

// LoadControlAPI reads control API configuration from the environment.
func LoadControlAPI() (ControlAPI, error) {
	c := ControlAPI{
		Environment:  Environment(getenv("WG_ENV", string(EnvLocal))),
		ListenAddr:   getenv("WG_LISTEN_ADDR", "127.0.0.1:8080"),
		DatabaseURL:  databaseURL(),
		ShutdownWait: getdur("WG_SHUTDOWN_WAIT", 15*time.Second),

		AccessTeamDomain: os.Getenv("WG_ACCESS_TEAM_DOMAIN"),
		AccessAudience:   os.Getenv("WG_ACCESS_AUD"),

		CellAccessAudience:       os.Getenv("WG_CELL_ACCESS_AUD"),
		CellHealthAccessAudience: os.Getenv("WG_CELL_HEALTH_ACCESS_AUD"),

		GitHubWebhookSecret: credential("github_webhook_secret", "WG_GITHUB_WEBHOOK_SECRET"),

		GitHubAppID:          os.Getenv("WG_GITHUB_APP_ID"),
		GitHubInstallationID: os.Getenv("WG_GITHUB_INSTALLATION_ID"),
		GitHubPrivateKeyPath: os.Getenv("WG_GITHUB_PRIVATE_KEY_PATH"),
	}

	var errs []error
	if !c.Environment.Valid() {
		errs = append(errs, fmt.Errorf("WG_ENV %q is not one of local, staging, production", c.Environment))
	}
	if c.DatabaseURL == "" {
		errs = append(errs, ErrMissing{Key: "WG_DATABASE_URL"})
	}
	// Outside local development, authentication configuration is mandatory.
	// A deployed service must never start in a state where it cannot verify
	// an Access token, because that would leave authorisation to headers.
	if c.Environment != EnvLocal {
		if c.AccessTeamDomain == "" {
			errs = append(errs, ErrMissing{Key: "WG_ACCESS_TEAM_DOMAIN"})
		}
		if c.AccessAudience == "" {
			errs = append(errs, ErrMissing{Key: "WG_ACCESS_AUD"})
		}
		if strings.HasPrefix(c.ListenAddr, "0.0.0.0") {
			errs = append(errs, errors.New("WG_LISTEN_ADDR must not bind 0.0.0.0; the origin is reached only through Cloudflare Tunnel"))
		}
	}
	return c, errors.Join(errs...)
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getdur(key string, def time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return def
	}
	if secs, err := strconv.Atoi(v); err == nil {
		return time.Duration(secs) * time.Second
	}
	if d, err := time.ParseDuration(v); err == nil {
		return d
	}
	return def
}

// ---------------------------------------------------------------------------
// systemd credentials
// ---------------------------------------------------------------------------

// credential reads a secret from the systemd credential store, falling back to
// an environment variable.
//
// systemd's LoadCredential= places each secret in a file under
// $CREDENTIALS_DIRECTORY: a per-service tmpfs, mode 0400, owned by the service
// user, unmounted when the unit stops. An environment variable is worse in three
// specific ways, none of them theoretical:
//
//	it is readable from /proc/<pid>/environ for anyone who can read the process,
//	which on the control node includes anything running as root;
//
//	it is inherited by every child process, so a shell-out leaks the database
//	password to whatever it runs;
//
//	it is trivially captured whole — `systemctl show -p Environment` prints it,
//	and so does a crash reporter dumping the environment.
//
// The environment fallback is deliberate rather than lazy. It keeps local
// development working without systemd, and it means a partially migrated
// deployment starts rather than failing in a way that reads like a missing
// secret. The credential path wins when both are present, so migrating a value
// is additive: add the LoadCredential line, remove the env line afterwards.
func credential(name, envKey string) string {
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		// filepath.Base defends against a name containing a separator, which
		// would otherwise read outside the credential directory.
		b, err := os.ReadFile(filepath.Join(dir, filepath.Base(name)))
		if err == nil {
			// Trailing newlines are an artefact of however the value was
			// written, not part of the secret. A password with a stray newline
			// fails authentication in a way that looks like a wrong password.
			return strings.TrimRight(string(b), "\r\n")
		}
	}
	return os.Getenv(envKey)
}

// CredentialSource reports where each secret was actually read from, so a
// deployment can prove the migration happened rather than assuming it.
func CredentialSource(name, envKey string) string {
	if dir := os.Getenv("CREDENTIALS_DIRECTORY"); dir != "" {
		if _, err := os.Stat(filepath.Join(dir, filepath.Base(name))); err == nil {
			return "systemd-credential"
		}
	}
	if os.Getenv(envKey) != "" {
		return "environment"
	}
	return "unset"
}

// databaseURL assembles the connection string, taking the password from the
// systemd credential store when one is present.
//
// The password is the part worth protecting, and it is the part that would
// otherwise sit in a DSN in the environment where every child process inherits
// it. WG_DATABASE_URL_TEMPLATE carries everything else and contains no secret,
// so it can stay in the unit file and be read by anyone.
//
// A complete WG_DATABASE_URL still wins if it is set, because local development
// and the migrate tool both pass one directly.
func databaseURL() string {
	if url := os.Getenv("WG_DATABASE_URL"); url != "" {
		return url
	}
	tmpl := os.Getenv("WG_DATABASE_URL_TEMPLATE")
	password := credential("db_app_password", "WG_DB_APP_PASSWORD")
	if tmpl == "" || password == "" {
		return ""
	}
	// A password is percent-encoded because it goes into a URL and generated
	// passwords contain characters that terminate one early. This was not
	// hypothetical: the current password contains '-' and '_' safely, but the
	// next rotation could produce '@' or '/' and would silently connect to the
	// wrong host or database rather than failing.
	return strings.Replace(tmpl, "{password}", url.QueryEscape(password), 1)
}
