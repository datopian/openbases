module github.com/datopian/workgraph

go 1.26

// Pinned to a patched release: 1.26.0 carries known stdlib
// vulnerabilities fixed in 1.26.4 and 1.26.5 (govulncheck GO-2026-*).
// Without this, local and CI resolve different patch levels.
toolchain go1.26.6

require (
	github.com/go-jose/go-jose/v4 v4.1.4
	github.com/golang-jwt/jwt/v5 v5.3.1
	github.com/jackc/pgx/v5 v5.10.0
	// v1.7.0 is the newest release that is not a pre-release: v1.7.0-pre.1
	// through -pre.3 precede it and v1.7.0 is the first stable tag of that
	// line. Exact rather than a range because it supplies the Streamable HTTP
	// transport that /mcp IS -- session handling, protocol negotiation and
	// cross-origin protection -- and a transport that changes shape under a
	// minor bump changes what remote clients can reach us. The hand-rolled
	// JSON-RPC loop this replaced implemented none of it.
	github.com/modelcontextprotocol/go-sdk v1.7.0
)

require (
	github.com/google/jsonschema-go v0.4.3 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.2 // indirect
	github.com/segmentio/asm v1.1.3 // indirect
	github.com/segmentio/encoding v0.5.4 // indirect
	github.com/yosida95/uritemplate/v3 v3.0.2 // indirect
	golang.org/x/oauth2 v0.35.0 // indirect
	golang.org/x/sync v0.21.0 // indirect
	golang.org/x/sys v0.41.0 // indirect
	golang.org/x/text v0.39.0 // indirect
	golang.org/x/time v0.15.0 // indirect
)
