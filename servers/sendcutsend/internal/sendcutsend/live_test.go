//go:build live

package sendcutsend

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

func liveClient(t *testing.T, credsFile string) *Client {
	t.Helper()

	c, err := NewClient(Options{CredentialsFile: credsFile, Version: "live-test", PollInterval: 2 * time.Second})
	require.NoError(t, err)

	return c
}

// A guest session needs no account, so this runs in CI without secrets. It
// covers the page scraping, upload, processing and pricing requests.
func TestLiveGuestUploadsAndPricesAPart(t *testing.T) {
	c := liveClient(t, "")
	ctx := t.Context()

	s, err := c.newSession(ctx)
	require.NoError(t, err)
	require.NotEmpty(t, s.csrf)
	require.Positive(t, s.app.DrawingAPIVersion)
	require.NotNil(t, s.app.UserToken, "a guest session has a user token")

	target, err := s.uploadURL(ctx, "live-test-plate.dxf")
	require.NoError(t, err)
	require.NoError(t, s.put(ctx, target, []byte(testPlateDXF)))

	d, err := s.createDrawing(ctx, target, "live-test-plate.dxf", "")
	require.NoError(t, err)

	d, err = s.waitProcessed(ctx, d)
	require.NoError(t, err)

	p := toPart(d)
	require.True(t, p.Quotable, "part: %+v", p)
	assert.InDelta(t, 2.0, p.Width, 0.001)
	assert.InDelta(t, 3.0, p.Height, 0.001)

	quotes, _, err := s.quotes(ctx, d.ID, 5, "", nil)
	require.NoError(t, err)

	priced := 0

	for _, q := range quotes {
		if len(q.Errors) == 0 && q.TotalCost > 0 {
			priced++
		}
	}

	assert.Greater(t, priced, 20, "most materials price a 2x3 inch plate")

	mats, err := s.materials(ctx)
	require.NoError(t, err)
	assert.Greater(t, len(mats), 50)
}

// The account flow needs the owner's sign-in cookie, so it runs only when
// SENDCUTSEND_CREDENTIALS_FILE names one. It cleans up what it creates.
func TestLiveAccountFlowBuildsAndDeletesASavedCart(t *testing.T) {
	creds := os.Getenv("SENDCUTSEND_CREDENTIALS_FILE")
	if creds == "" {
		t.Skip("SENDCUTSEND_CREDENTIALS_FILE is not set")
	}

	c := liveClient(t, creds)
	session := mcptest.Connect(t, mcpserver.Config{Name: "sendcutsend", Version: "test", Register: Tools(c)})

	var part PartOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "create_part", map[string]any{
		"filename": "mcp-live-test-plate.dxf", "dxf_text": testPlateDXF,
	}), &part)
	require.True(t, part.Quotable, "part: %+v", part)

	// A deferred call, not t.Cleanup: the test's context is cancelled before
	// cleanups run, and the call needs it.
	defer func() {
		res := mcptest.CallTool(t, session, "delete_part", map[string]any{"part_id": part.ID})
		assert.False(t, res.IsError, mcptest.Text(res))
	}()

	var all QuoteOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "quote_part", map[string]any{
		"part_id": part.ID, "quantity": 5, "category": "Steel", "limit": 3,
	}), &all)
	require.NotEmpty(t, all.Quotes)

	// Mild steel .074 offers tapping and powder coating.
	const sku = "CRS-074"

	var features FeaturesOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "get_part_features", map[string]any{
		"part_id": part.ID, "material_sku": sku,
	}), &features)
	require.Len(t, features.Holes, 1)
	require.Contains(t, features.Holes[0].TapsThatFit, "M4 x 0.7")

	services := map[string]any{
		"taps":   []map[string]any{{"hole_ids": []string{features.Holes[0].ID}, "tap": "M4 x 0.7"}},
		"powder": "matte_black",
	}

	var one QuoteOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "quote_part", map[string]any{
		"part_id": part.ID, "quantity": 5, "material_sku": sku, "services": services,
	}), &one)
	require.Len(t, one.Quotes, 1)
	assert.NotEmpty(t, one.Quotes[0].EstShipDate)
	assert.NotEmpty(t, one.Quotes[0].PriceBreaks)
	require.NotNil(t, one.Quotes[0].GrandTotal)
	assert.Greater(t, *one.Quotes[0].GrandTotal, one.Quotes[0].TotalPrice, "services cost something")

	var cart CartOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "create_saved_cart", map[string]any{
		"title": "mcp-servers live test (safe to delete)",
		"items": []map[string]any{{"part_id": part.ID, "material_sku": sku, "quantity": 5, "services": services}},
	}), &cart)
	require.Positive(t, cart.ID)
	assert.True(t, strings.HasPrefix(cart.CheckoutURL, DefaultBaseURL+"/customer#/cart?accept="), cart.CheckoutURL)
	require.Len(t, cart.Items, 1)
	assert.InDelta(t, *one.Quotes[0].GrandTotal, *cart.Items[0].TotalPrice, 0.01, "the cart charges what the quote said")

	var got CartOutput
	mcptest.Structured(t, mcptest.CallTool(t, session, "get_saved_cart", map[string]any{"cart_id": cart.ID}), &got)
	require.Len(t, got.Items, 1)
	assert.Equal(t, sku, got.Items[0].MaterialSKU)
	assert.Equal(t, 5, got.Items[0].Quantity)

	res := mcptest.CallTool(t, session, "delete_saved_cart", map[string]any{"cart_id": cart.ID})
	require.False(t, res.IsError, mcptest.Text(res))
}
