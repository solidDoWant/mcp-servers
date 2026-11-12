// Package mcptest connects a real MCP client to a server's HTTP handler, for
// tests that exercise tools the way a client calls them.
package mcptest

import (
	"context"
	"encoding/json"
	"flag"
	"log/slog"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/pmezard/go-difflib/difflib"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
)

var update = flag.Bool("update", false, "rewrite golden files with the current output")

// Connect serves cfg over HTTP and returns a client session connected to it.
// Both are closed when the test ends.
func Connect(t *testing.T, cfg mcpserver.Config) *mcp.ClientSession {
	t.Helper()

	logger := slog.New(slog.DiscardHandler)
	srv := httptest.NewServer(mcpserver.NewHandler(cfg, logger))
	t.Cleanup(srv.Close)

	client := mcp.NewClient(&mcp.Implementation{Name: "mcptest", Version: "0"}, nil)
	session, err := client.Connect(t.Context(), &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp"}, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = session.Close() })

	return session
}

// AssertToolsGolden compares the server's tool list (names, descriptions,
// input and output schemas) with the golden file at path. Run the test with
// -update to rewrite the file after an intended change, then review the diff:
// clients see every word of it.
func AssertToolsGolden(t *testing.T, session *mcp.ClientSession, path string) {
	t.Helper()

	tools, err := session.ListTools(context.WithoutCancel(t.Context()), nil)
	require.NoError(t, err)

	got, err := json.MarshalIndent(tools.Tools, "", "  ")
	require.NoError(t, err)

	got = append(got, '\n')

	if *update {
		require.NoError(t, os.WriteFile(path, got, 0o644))

		return
	}

	want, err := os.ReadFile(path)
	require.NoError(t, err, "missing golden file; run the test with -update to create it")

	if string(want) != string(got) {
		diff, _ := difflib.GetUnifiedDiffString(difflib.UnifiedDiff{
			A: difflib.SplitLines(string(want)), B: difflib.SplitLines(string(got)),
			FromFile: path, ToFile: "current", Context: 2,
		})
		t.Fatalf("tool list changed; run the test with -update if the change is intended:\n%s", diff)
	}
}

// CallTool calls a tool and returns its result. A protocol error fails the
// test; a tool error is returned in the result (IsError) for the test to
// check.
func CallTool(t *testing.T, session *mcp.ClientSession, name string, args any) *mcp.CallToolResult {
	t.Helper()

	res, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: name, Arguments: args})
	require.NoError(t, err)

	return res
}

// Structured decodes a tool result's structured content into out.
func Structured(t *testing.T, res *mcp.CallToolResult, out any) {
	t.Helper()

	require.False(t, res.IsError, "tool returned an error: %s", Text(res))

	raw, err := json.Marshal(res.StructuredContent)
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, out))
}

// Text returns a tool result's text content, joined.
func Text(res *mcp.CallToolResult) string {
	var s string

	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			s += tc.Text
		}
	}

	return s
}
