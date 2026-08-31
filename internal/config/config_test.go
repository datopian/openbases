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

// Every field the environment file sets has to reach the struct. This one did
// not: PubSubPushAudience was declared, consumed by cmd/control-api to decide
// whether to register the Pub/Sub push endpoint, and never read — so the
// endpoint was never registered even though the deployed environment file set
// the variable and the consuming code looked right.
//
// The general shape of the bug is worse than the instance: a guard reading
// "register only when configured" behaves as "never register" when the config
// never arrives, and nothing about it looks wrong.
func TestLoadControlAPI_ReadsEveryDeployedVariable(t *testing.T) {
	t.Setenv("WG_ENV", "local")
	t.Setenv("WG_DATABASE_URL", "postgres://localhost/wg")
	t.Setenv("WG_PUBSUB_PUSH_AUDIENCE", "https://work.example/v1/google/events")
	t.Setenv("WG_PUBSUB_PUSH_SERVICE_ACCOUNT", "pusher@example.iam.gserviceaccount.com")

	c, err := LoadControlAPI()
	if err != nil {
		t.Fatal(err)
	}
	if c.PubSubPushAudience != "https://work.example/v1/google/events" {
		t.Errorf("PubSubPushAudience = %q; the push endpoint is not registered without it",
			c.PubSubPushAudience)
	}
	if c.PubSubPushServiceAccount != "pusher@example.iam.gserviceaccount.com" {
		t.Errorf("PubSubPushServiceAccount = %q", c.PubSubPushServiceAccount)
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
