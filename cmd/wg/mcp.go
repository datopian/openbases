package main

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	wgmcp "github.com/datopian/openbases/internal/mcp"
	"github.com/datopian/openbases/internal/version"
)

// The stdio MCP transport (wg-p4h.10, wg-p4h.11).
//
//	wg mcp
//
// A thin adapter now. The tools, their schemas, the tool-to-route table and the
// error mapping all live in internal/mcp, shared with the remote transport that
// control-api serves at /mcp — so a client on a laptop and a client on a phone
// see the same tool set from the same code. Two copies would drift, and a tool
// list that lags by a release is a confidently wrong prompt rather than a
// missing feature.
//
// This file used to carry a hand-rolled JSON-RPC loop over stdin and stdout. It
// worked for stdio and implemented nothing else: no session handling, no
// protocol negotiation beyond a hardcoded version string, no annotations. The
// official SDK supplies all of that and is what makes the remote transport
// possible at all.
//
// Still a subcommand of the CLI rather than a separate binary, so it reads the
// same credentials file and speaks to the same API and cannot drift from it.
func mcpServe() (int, error) {
	server := wgmcp.NewServer(wgmcp.Options{
		Caller:  cliCaller{},
		Version: version.Version,
		// There IS a shell here, which is the one thing the tool set needs to
		// know: an expired credential should tell the model `wg login`, which
		// is a command that exists in this context and does not exist over a
		// connector.
		HasShell: true,
		// No Observe: stdout is the protocol stream, and anything written
		// there that is not a JSON-RPC message corrupts the conversation. The
		// client's own logs record the calls.
	})
	if err := server.Run(context.Background(), &sdk.StdioTransport{}); err != nil {
		return exitError, err
	}
	return exitOK, nil
}

// cliCaller reaches the API over HTTPS with the person's stored token.
type cliCaller struct{}

func (cliCaller) Call(_ context.Context, method, path string, body any) (int, []byte, error) {
	// do() loads the credentials file on each call, which is deliberate: a
	// `wg login` in another terminal takes effect on the next tool call rather
	// than needing the server restarted.
	return do(method, path, body)
}
