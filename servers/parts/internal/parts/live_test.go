//go:build live

package parts

import (
	"os"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

func liveSession(t *testing.T, cfg Config) *mcp.ClientSession {
	t.Helper()

	cfg.Version = "live-test"

	return mcptest.Connect(t, mcpserver.Config{Name: "parts", Version: "test", Register: Tools(New(cfg))})
}

// jlcsearch needs no key, so this runs in CI without secrets.
func TestLiveJLCSearchFindsAPartByNumberAndKeyword(t *testing.T) {
	s := liveSession(t, Config{})

	var search SearchOutput
	mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "STM32F103C8T6", "limit": 5}), &search)
	require.Empty(t, search.Errors)
	require.NotEmpty(t, search.Parts)
	assert.Equal(t, "STM32F103C8T6", search.Parts[0].MPN)
	require.NotNil(t, search.Parts[0].Best)
	assert.Positive(t, search.Parts[0].Best.UnitPrice)

	var part GetPartOutput
	mcptest.Structured(t, mcptest.CallTool(t, s, "get_part", map[string]any{"part_number": "C8734"}), &part)
	require.Len(t, part.Offers, 1)
	assert.Equal(t, "STM32F103C8T6", part.Offers[0].MPN)
	assert.Equal(t, "C8734", part.Offers[0].DistributorPN)

	var basic SearchOutput
	mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "10k 0603", "jlc_basic_only": true, "limit": 5}), &basic)
	require.NotEmpty(t, basic.Parts)

	for _, g := range basic.Parts {
		for _, o := range g.Offers {
			require.NotNil(t, o.JLCBasic)
			assert.True(t, *o.JLCBasic, "%s is a basic part", o.DistributorPN)
		}
	}
}

func readKey(t *testing.T, env string) string {
	t.Helper()

	path := os.Getenv(env)
	if path == "" {
		t.Skipf("%s is not set", env)
	}

	b, err := os.ReadFile(path)
	require.NoError(t, err)

	return strings.TrimSpace(string(b))
}

func TestLiveDigiKeyFindsAPart(t *testing.T) {
	cfg := Config{JLCSearchDisabled: true}
	cfg.DigiKey.ClientID = readKey(t, "DIGIKEY_CLIENT_ID_FILE")
	cfg.DigiKey.ClientSecret = readKey(t, "DIGIKEY_CLIENT_SECRET_FILE")
	s := liveSession(t, cfg)

	var search SearchOutput
	mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "STM32F103C8T6", "limit": 3}), &search)
	require.Empty(t, search.Errors)
	require.NotEmpty(t, search.Parts)
	assert.NotEmpty(t, search.Parts[0].Offers[0].PriceBreaks)

	var part GetPartOutput
	mcptest.Structured(t, mcptest.CallTool(t, s, "get_part", map[string]any{"part_number": "STM32F103C8T6"}), &part)
	require.NotEmpty(t, part.Offers)
	assert.NotEmpty(t, part.Offers[0].Specs)
}

func TestLiveMouserFindsAPart(t *testing.T) {
	cfg := Config{JLCSearchDisabled: true, MouserAPIKey: readKey(t, "MOUSER_API_KEY_FILE")}
	s := liveSession(t, cfg)

	var search SearchOutput
	mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "STM32F103C8T6", "limit": 3}), &search)
	require.Empty(t, search.Errors)
	require.NotEmpty(t, search.Parts)
	assert.NotEmpty(t, search.Parts[0].Offers[0].PriceBreaks)

	var part GetPartOutput
	mcptest.Structured(t, mcptest.CallTool(t, s, "get_part", map[string]any{"part_number": "STM32F103C8T6"}), &part)
	require.NotEmpty(t, part.Offers)
}
