package config

import (
	"strings"
	"testing"
)

func TestLoadControlAPI_FailsClosedInProduction(t *testing.T) {
	t.Setenv("WG_ENV", "production")
	t.Setenv("WG_DATABASE_URL", "postgres://localhost/wg")
	t.Setenv("WG_ACCESS_TEAM_DOMAIN", "")
	t.Setenv("WG_ACCESS_AUD", "")

	_, err := LoadControlAPI()
	if err == nil {
		t.Fatal("production config without Access settings must not load")
	}
	for _, want := range []string{"WG_ACCESS_TEAM_DOMAIN", "WG_ACCESS_AUD"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not mention %s: %v", want, err)
		}
	}
}

func TestLoadControlAPI_RejectsWildcardBindOutsideLocal(t *testing.T) {
	t.Setenv("WG_ENV", "staging")
	t.Setenv("WG_DATABASE_URL", "postgres://localhost/wg")
	t.Setenv("WG_ACCESS_TEAM_DOMAIN", "datopian.cloudflareaccess.com")
	t.Setenv("WG_ACCESS_AUD", "aud-value")
	t.Setenv("WG_LISTEN_ADDR", "0.0.0.0:8080")

	if _, err := LoadControlAPI(); err == nil {
		t.Fatal("binding 0.0.0.0 outside local must be rejected")
	}
}

func TestLoadControlAPI_LocalDefaults(t *testing.T) {
	t.Setenv("WG_ENV", "local")
	t.Setenv("WG_DATABASE_URL", "postgres://localhost/wg")
	t.Setenv("WG_ACCESS_TEAM_DOMAIN", "")
	t.Setenv("WG_ACCESS_AUD", "")
	t.Setenv("WG_LISTEN_ADDR", "")

	c, err := LoadControlAPI()
	if err != nil {
		t.Fatalf("local config must load: %v", err)
	}
	if c.ListenAddr != "127.0.0.1:8080" {
		t.Errorf("unexpected default listen addr %q", c.ListenAddr)
	}
}

func TestEnvironmentValid(t *testing.T) {
	for _, e := range []Environment{EnvLocal, EnvStaging, EnvProduction} {
		if !e.Valid() {
			t.Errorf("%s should be valid", e)
		}
	}
	if Environment("prod").Valid() {
		t.Error(`"prod" must not be accepted; the environment name is exact`)
	}
}
