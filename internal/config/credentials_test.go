package config

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"testing"
)

// A credential file must win over an environment variable, so that migrating a
// secret is additive: add LoadCredential=, then remove the env line, without a
// window where the old value is still in force.
func TestCredentialPrefersTheFileOverTheEnvironment(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "db_app_password"), []byte("from-file"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	t.Setenv("WG_DB_APP_PASSWORD", "from-env")

	if got := credential("db_app_password", "WG_DB_APP_PASSWORD"); got != "from-file" {
		t.Errorf("credential = %q, want the file's value", got)
	}
	if got := CredentialSource("db_app_password", "WG_DB_APP_PASSWORD"); got != "systemd-credential" {
		t.Errorf("source = %q, want systemd-credential", got)
	}
}

// Without systemd there is no credential directory, and local development still
// has to work.
func TestCredentialFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", "")
	t.Setenv("WG_GITHUB_WEBHOOK_SECRET", "env-secret")
	if got := credential("github_webhook_secret", "WG_GITHUB_WEBHOOK_SECRET"); got != "env-secret" {
		t.Errorf("credential = %q, want env-secret", got)
	}
	if got := CredentialSource("github_webhook_secret", "WG_GITHUB_WEBHOOK_SECRET"); got != "environment" {
		t.Errorf("source = %q, want environment", got)
	}
}

// A credential directory that exists but does not hold this particular secret
// must fall through rather than returning empty. A half-migrated deployment
// should start, not fail in a way that reads like a missing secret.
func TestCredentialFallsThroughWhenTheFileIsAbsent(t *testing.T) {
	t.Setenv("CREDENTIALS_DIRECTORY", t.TempDir())
	t.Setenv("WG_DB_APP_PASSWORD", "still-here")
	if got := credential("db_app_password", "WG_DB_APP_PASSWORD"); got != "still-here" {
		t.Errorf("credential = %q, want the environment value", got)
	}
}

// systemd writes what it is given. A trailing newline from an editor or a shell
// redirect is not part of the secret, and leaving it on produces an
// authentication failure that looks exactly like a wrong password.
func TestCredentialStripsTrailingNewlines(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "s"), []byte("value\r\n\n"), 0o400); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", dir)
	if got := credential("s", "UNSET_KEY"); got != "value" {
		t.Errorf("credential = %q, want %q", got, "value")
	}
}

// A name containing a path separator must not read outside the credential
// directory.
func TestCredentialRefusesToEscapeTheDirectory(t *testing.T) {
	outer := t.TempDir()
	if err := os.WriteFile(filepath.Join(outer, "secret"), []byte("do-not-read"), 0o400); err != nil {
		t.Fatal(err)
	}
	inner := filepath.Join(outer, "creds")
	if err := os.Mkdir(inner, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CREDENTIALS_DIRECTORY", inner)
	t.Setenv("UNSET_KEY", "")
	if got := credential("../secret", "UNSET_KEY"); got != "" {
		t.Errorf("credential = %q; a traversing name must not resolve", got)
	}
}

func TestDatabaseURL(t *testing.T) {
	const tmpl = "postgres://workgraph_app:{password}@127.0.0.1:5432/workgraph?sslmode=disable"

	t.Run("a complete URL wins, for the migrate tool and local development", func(t *testing.T) {
		t.Setenv("CREDENTIALS_DIRECTORY", "")
		t.Setenv("WG_DATABASE_URL", "postgres:///direct")
		t.Setenv("WG_DATABASE_URL_TEMPLATE", tmpl)
		t.Setenv("WG_DB_APP_PASSWORD", "pw")
		if got := databaseURL(); got != "postgres:///direct" {
			t.Errorf("databaseURL = %q", got)
		}
	})

	t.Run("the template is filled from the credential", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "db_app_password"), []byte("s3cret"), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CREDENTIALS_DIRECTORY", dir)
		t.Setenv("WG_DATABASE_URL", "")
		t.Setenv("WG_DATABASE_URL_TEMPLATE", tmpl)
		want := "postgres://workgraph_app:s3cret@127.0.0.1:5432/workgraph?sslmode=disable"
		if got := databaseURL(); got != want {
			t.Errorf("databaseURL = %q, want %q", got, want)
		}
	})

	// The failure this prevents is silent and severe: an unencoded '@' or '/'
	// terminates the authority section early, so the client connects to a
	// DIFFERENT host or database rather than reporting a bad password.
	t.Run("a password with URL metacharacters is encoded", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "db_app_password"), []byte("p@ss/w:rd?x#y"), 0o400); err != nil {
			t.Fatal(err)
		}
		t.Setenv("CREDENTIALS_DIRECTORY", dir)
		t.Setenv("WG_DATABASE_URL", "")
		t.Setenv("WG_DATABASE_URL_TEMPLATE", tmpl)

		got := databaseURL()
		if got != "postgres://workgraph_app:p%40ss%2Fw%3Ard%3Fx%23y@127.0.0.1:5432/workgraph?sslmode=disable" {
			t.Errorf("databaseURL = %q; metacharacters must be percent-encoded", got)
		}
		// And the assembled string must still parse as one URL pointing at the
		// intended host, which is the property that actually matters.
		if err := assertHost(got, "127.0.0.1:5432", "/workgraph"); err != nil {
			t.Error(err)
		}
	})

	t.Run("no password means no URL, rather than a URL with an empty password", func(t *testing.T) {
		t.Setenv("CREDENTIALS_DIRECTORY", "")
		t.Setenv("WG_DATABASE_URL", "")
		t.Setenv("WG_DATABASE_URL_TEMPLATE", tmpl)
		t.Setenv("WG_DB_APP_PASSWORD", "")
		if got := databaseURL(); got != "" {
			t.Errorf("databaseURL = %q, want empty so startup reports a missing setting", got)
		}
	})
}

func assertHost(raw, host, path string) error {
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	if u.Host != host {
		return fmt.Errorf("host = %q, want %q", u.Host, host)
	}
	if u.Path != path {
		return fmt.Errorf("path = %q, want %q", u.Path, path)
	}
	return nil
}
