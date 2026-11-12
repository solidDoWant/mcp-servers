package mcpserver_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

type echoIn struct {
	Text string `json:"text"`
}

type echoOut struct {
	Text string `json:"text"`
}

func echoConfig() mcpserver.Config {
	return mcpserver.Config{
		Name:         "echo",
		Version:      "1.2.3",
		Instructions: "Echoes text.",
		Register: func(s *mcp.Server) {
			mcp.AddTool(s, &mcp.Tool{Name: "echo", Description: "Echo text back."},
				func(_ context.Context, _ *mcp.CallToolRequest, in echoIn) (*mcp.CallToolResult, echoOut, error) {
					return nil, echoOut(in), nil
				})
		},
	}
}

func TestHealthCheckAnswersWithoutMCP(t *testing.T) {
	srv := httptest.NewServer(mcpserver.NewHandler(echoConfig(), slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz") //nolint:noctx // a test
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck // a test

	body, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, "ok\n", string(body))
}

func TestClientsSeeTheServersNameVersionAndInstructions(t *testing.T) {
	session := mcptest.Connect(t, echoConfig())

	init := session.InitializeResult()
	assert.Equal(t, "echo", init.ServerInfo.Name)
	assert.Equal(t, "1.2.3", init.ServerInfo.Version)
	assert.Equal(t, "Echoes text.", init.Instructions)

	var out echoOut
	mcptest.Structured(t, mcptest.CallTool(t, session, "echo", map[string]any{"text": "hi"}), &out)
	assert.Equal(t, "hi", out.Text)
}

// The handler keeps no session state, so a request carrying a session ID
// that no replica issued (after a restart, or from another replica) still
// works.
func TestRequestsWorkWithoutASessionTheServerKnows(t *testing.T) {
	srv := httptest.NewServer(mcpserver.NewHandler(echoConfig(), slog.New(slog.DiscardHandler)))
	t.Cleanup(srv.Close)

	body := `{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"echo","arguments":{"text":"hi"}}}`
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, srv.URL+"/mcp", strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Mcp-Session-Id", "issued-by-another-replica")
	req.Header.Set("Mcp-Protocol-Version", "2025-06-18")

	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)

	defer resp.Body.Close() //nolint:errcheck // a test

	got, err := io.ReadAll(resp.Body)
	require.NoError(t, err)
	assert.Equal(t, http.StatusOK, resp.StatusCode, string(got))
	assert.Contains(t, string(got), `"structuredContent":{"text":"hi"}`)
}
