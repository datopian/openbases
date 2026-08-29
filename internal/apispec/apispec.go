// Package apispec holds the API contract and serves it to the binary that
// implements it (wg-p4h.5).
//
// The document lives HERE rather than under docs/ for one mechanical reason:
// go:embed cannot reach outside its own package, and a copy under docs/ would be
// a second version of the contract that drifts from the served one. A contract
// with two copies is not a contract.
//
// It is embedded rather than read from disk at startup, so a deployment cannot
// serve a spec that disagrees with the code it is running — that is the same
// drift the contract test prevents in the repository, moved to the node.
package apispec

import _ "embed"

// OpenAPI is the checked-in OpenAPI 3.1 document.
//
//go:embed openapi.json
var OpenAPI []byte
