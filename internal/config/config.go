// Package config loads service configuration from the environment.
//
// Configuration is fail-closed: a missing security-relevant setting is an error,
// never a permissive default. Secrets are read from systemd credentials or the
// environment at runtime and are never logged (plan section 11.4).
package config

import (
	"errors"
	"fmt"
	"os"
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
		DatabaseURL:  os.Getenv("WG_DATABASE_URL"),
		ShutdownWait: getdur("WG_SHUTDOWN_WAIT", 15*time.Second),

		AccessTeamDomain: os.Getenv("WG_ACCESS_TEAM_DOMAIN"),
		AccessAudience:   os.Getenv("WG_ACCESS_AUD"),

		CellAccessAudience:       os.Getenv("WG_CELL_ACCESS_AUD"),
		CellHealthAccessAudience: os.Getenv("WG_CELL_HEALTH_ACCESS_AUD"),

		GitHubWebhookSecret: os.Getenv("WG_GITHUB_WEBHOOK_SECRET"),

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
