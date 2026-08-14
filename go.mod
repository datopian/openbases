module github.com/datopian/workgraph

go 1.26

// Pinned to a patched release: 1.26.0 carries known stdlib
// vulnerabilities fixed in 1.26.4 and 1.26.5 (govulncheck GO-2026-*).
// Without this, local and CI resolve different patch levels.
toolchain go1.26.6

require (
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/jackc/pgx/v5 v5.10.0
)

require (
	github.com/golang-jwt/jwt/v5 v5.3.1 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/text v0.39.0 // indirect
)
