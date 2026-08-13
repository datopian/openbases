package main

import (
	"strings"
	"testing"
)

// The committed migrations must load, be ordered, and hash stably. A migration
// that cannot be read is a deployment that fails at the worst moment.
func TestLoadMigrations(t *testing.T) {
	all, err := load()
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(all) == 0 {
		t.Fatal("no migrations embedded; the deploy would silently do nothing")
	}

	for i, m := range all {
		if !strings.HasSuffix(m.name, ".sql") {
			t.Errorf("%s is not a .sql file", m.name)
		}
		if len(m.checksum) != 64 {
			t.Errorf("%s has a malformed checksum %q", m.name, m.checksum)
		}
		if m.sql == "" {
			t.Errorf("%s is empty", m.name)
		}
		// Lexical order is what makes the NNNN_ prefix meaningful.
		if i > 0 && all[i-1].name >= m.name {
			t.Errorf("migrations out of order: %s before %s", all[i-1].name, m.name)
		}
	}
}

// The checksum must depend on content, or an edited migration would pass the
// tamper check that exists to catch exactly that.
func TestChecksumIsContentAddressed(t *testing.T) {
	all, err := load()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]string{}
	for _, m := range all {
		if prev, dup := seen[m.checksum]; dup {
			t.Errorf("%s and %s hash identically; content-addressing is broken", prev, m.name)
		}
		seen[m.checksum] = m.name
	}
}
