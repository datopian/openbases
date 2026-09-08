// Command wg-init creates a deployment's first organisation and administrator.
//
// `workgraph-migrate -fresh` leaves the schema with nobody in it, which is
// correct and unusable: every request is resolved to a user row and refused
// when there is none, so with no users there is no way in to create the first
// one. This writes that row.
//
// Run once, at install time, on a host with database access — the same place
// and the same way as wg-registry. It connects as the application role and does
// its work through system_bootstrap_organisation, which is SECURITY DEFINER
// because at bootstrap there is no app user for RLS to check by definition.
//
//	wg-init -org acme -org-name "Acme Ltd" \
//	        -admin-email ops@acme.example -admin-name "Dana Ops"
//
// Idempotent: running it again with the same arguments reports
// already_initialised and changes nothing. It refuses to create a second
// organisation, because one company per deployment is the documented stance and
// a bootstrap tool that could quietly add a tenant would contradict it.
//
// Flags rather than prompts, deliberately. This runs inside an install script
// next to the migration step, and a tool that stops to ask cannot be run
// unattended. Everything it needs is on the command line or it refuses and says
// which part is missing.
package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/datopian/openbases/internal/config"
)

// options is everything the bootstrap needs. Separated from main so the
// validation and the reporting can be tested without a database.
type options struct {
	orgSlug    string
	orgName    string
	adminEmail string
	adminName  string
	portfolios bool
}

func main() {
	var (
		dsn = flag.String("dsn", config.DatabaseURL(), "PostgreSQL connection string")
		opt options
	)
	flag.StringVar(&opt.orgSlug, "org", "", "short slug for the organisation, e.g. acme")
	flag.StringVar(&opt.orgName, "org-name", "", "display name, e.g. \"Acme Ltd\"")
	flag.StringVar(&opt.adminEmail, "admin-email", "", "the first administrator's address, as the identity provider will assert it")
	flag.StringVar(&opt.adminName, "admin-name", "", "the first administrator's display name")
	flag.BoolVar(&opt.portfolios, "with-portfolios", false, "also create the four standard portfolios (oss, product, client, internal)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := validate(opt); err != nil {
		log.Error("cannot initialise", "error", err)
		flag.Usage()
		os.Exit(2)
	}
	if *dsn == "" {
		log.Error("no connection string; set WG_DATABASE_URL, or WG_DATABASE_URL_TEMPLATE with the db_app_password credential, or pass -dsn")
		os.Exit(1)
	}

	status, err := run(context.Background(), *dsn, opt)
	if err != nil {
		log.Error("initialisation failed", "error", err)
		os.Exit(1)
	}
	fmt.Print(summary(status, opt))
}

// validate refuses an incomplete invocation before touching the database.
//
// Every field is required. The alternative — defaulting the organisation name
// from the slug, say — produces a deployment named "acme" that somebody has to
// notice and fix later, and this runs once.
func validate(o options) error {
	var missing []string
	if strings.TrimSpace(o.orgSlug) == "" {
		missing = append(missing, "-org")
	}
	if strings.TrimSpace(o.orgName) == "" {
		missing = append(missing, "-org-name")
	}
	if strings.TrimSpace(o.adminEmail) == "" {
		missing = append(missing, "-admin-email")
	}
	if strings.TrimSpace(o.adminName) == "" {
		missing = append(missing, "-admin-name")
	}
	if len(missing) > 0 {
		return fmt.Errorf("missing %s", strings.Join(missing, ", "))
	}

	// The address is checked again, and better, inside the function. This is
	// here so an obvious typo fails before a connection is opened, and because
	// the reason is worth saying once in plain terms.
	if !strings.Contains(o.adminEmail, "@") {
		return fmt.Errorf(
			"-admin-email %q is not an address; it must be the one the identity "+
				"provider asserts, because that is what the first sign-in is matched on",
			o.adminEmail)
	}
	if strings.ContainsAny(o.orgSlug, " \t/") {
		return fmt.Errorf("-org %q should be a short slug with no spaces or slashes", o.orgSlug)
	}
	return nil
}

func run(ctx context.Context, dsn string, o options) (string, error) {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return "", fmt.Errorf("opening database: %w", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	if err := db.PingContext(ctx); err != nil {
		return "", fmt.Errorf("connecting: %w", err)
	}

	var status string
	err = db.QueryRowContext(ctx,
		`SELECT system_bootstrap_organisation($1, $2, $3, $4, $5)`,
		strings.TrimSpace(o.orgSlug), strings.TrimSpace(o.orgName),
		strings.TrimSpace(o.adminEmail), strings.TrimSpace(o.adminName),
		o.portfolios).Scan(&status)
	if err != nil {
		// The function raises for the cases worth distinguishing — a second
		// organisation, a malformed address — and its message is better than
		// anything that could be reconstructed here, so it is passed through.
		return "", err
	}
	if status == "" {
		return "", errors.New("the bootstrap function returned no status; the schema may predate migration 0091")
	}
	return status, nil
}

// summary says what happened and what to do next.
//
// The next step is the part people get stuck on: nothing is created for signing
// in, and nothing can be. A Cloudflare Access subject is issued by the provider
// on first sign-in and is linked to this user by email address then. If the
// address here is not the one the provider asserts, the administrator is
// refused and the deployment has nobody who can fix it.
func summary(status string, o options) string {
	var b strings.Builder
	switch status {
	case "created":
		fmt.Fprintf(&b, "created organisation %q (%s)\n", o.orgName, o.orgSlug)
		fmt.Fprintf(&b, "  administrator: %s <%s>, granted organisation_admin\n",
			o.adminName, o.adminEmail)
		if o.portfolios {
			b.WriteString("  portfolios:    oss, product, client, internal\n")
		}
	case "admin_added":
		fmt.Fprintf(&b, "organisation %q already existed; added %s as organisation_admin\n",
			o.orgSlug, o.adminEmail)
	case "already_initialised":
		fmt.Fprintf(&b, "already initialised: organisation %q with %s as organisation_admin. Nothing to do.\n",
			o.orgSlug, o.adminEmail)
		return b.String()
	default:
		fmt.Fprintf(&b, "bootstrap reported %q\n", status)
	}

	b.WriteString("\nNext: sign in at the deployment's hostname as " + o.adminEmail + ".\n")
	b.WriteString("No login identity was created and none can be — the identity provider\n")
	b.WriteString("issues the subject on first sign-in, and it is linked to this user by\n")
	b.WriteString("address. If that address is not the one your provider asserts, this\n")
	b.WriteString("administrator will be refused, so check it before handing the\n")
	b.WriteString("deployment over.\n")
	return b.String()
}
