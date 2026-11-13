package sendcutsend

import (
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

func TestToolListMatchesGoldenFile(t *testing.T) {
	f := newFake(t)
	mcptest.AssertToolsGolden(t, connect(t, f.options(t)), filepath.Join("testdata", "tools.golden.json"))
}

func TestCreatePart(t *testing.T) {
	t.Run("uploads inline DXF text, waits for processing and returns the measured part", func(t *testing.T) {
		f := newFake(t)
		s := connect(t, f.options(t))

		var got PartOutput
		mcptest.Structured(t, mcptest.CallTool(t, s, "create_part", map[string]any{
			"filename": "bracket.dxf", "dxf_text": testPlateDXF, "units": "in",
		}), &got)

		assert.Equal(t, PartOutput{
			ID: partID, Name: "bracket.dxf", Processed: true, State: "complete", Quotable: true,
			Units: "in", Width: 2, Height: 3, Area: 6, Contours: 2,
			Format: "sheet", PossibleFormats: []string{"sheet"},
			PreviewURL: "https://cdn.sendcutsend.com/preview/0b5bb_c55fcbc2-bfc0-4de7-8802-9c8879bf11e8-bracket_conv.svg",
			Created:    "2026-11-13T18:03:20Z",
		}, got)

		assert.Equal(t, testPlateDXF, f.uploadedBody("/s3/drawings/abc-bracket.dxf"))

		body := f.lastBody("POST /drawings")
		assert.Equal(t, f.srv.URL+"/s3/drawings/abc-bracket.dxf?X-Amz-Signature=sig", body["filename"], "the whole upload URL is the file name")
		assert.Equal(t, "bracket.dxf", body["customer_filename"])
		assert.InDelta(t, 6, body["processor_version"], 0, "the version the app page names")
		assert.Equal(t, "in", body["units"])
		assert.NotContains(t, body, "user_token", "a signed-in session sends no guest token")
		assert.Equal(t, map[string]any{"filename": "bracket.dxf"}, f.lastBody("POST /drawings/upload"))
	})

	t.Run("returns an unprocessed part when processing outlasts the wait", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-pending.json")

		opts := f.options(t)
		opts.ProcessTimeout = time.Nanosecond
		opts.PollInterval = time.Hour

		var got PartOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, opts), "create_part", map[string]any{
			"filename": "bracket.dxf", "dxf_text": testPlateDXF,
		}), &got)

		assert.False(t, got.Processed)
		assert.False(t, got.Quotable)
		assert.Equal(t, "pending", got.State)
		assert.Empty(t, got.Errors, "errors of an unprocessed part are stale and left out")
	})

	t.Run("reports the problems of a part that can't be priced", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-error.json")

		var got PartOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "create_part", map[string]any{
			"filename": "bracket.dxf", "dxf_text": "0\nEOF\n",
		}), &got)

		assert.True(t, got.Processed)
		assert.False(t, got.Quotable)
		assert.Equal(t, []IssueItem{{Code: "no_quotable_formats", Message: "Part does not match any of our available formats"}}, got.Errors)
	})

	t.Run("creates a part from a file uploaded to an upload URL", func(t *testing.T) {
		f := newFake(t)
		f.mu.Lock()
		f.uploaded["/s3/drawings/abc-bracket.step"] = []byte("step")
		f.mu.Unlock()
		s := connect(t, f.options(t))

		var got PartOutput
		mcptest.Structured(t, mcptest.CallTool(t, s, "create_part", map[string]any{
			"filename": "bracket.step", "upload_url": f.srv.URL + "/s3/drawings/abc-bracket.step?X-Amz-Signature=sig",
		}), &got)

		assert.Equal(t, int64(partID), got.ID)
		assert.Equal(t, f.srv.URL+"/s3/drawings/abc-bracket.step?X-Amz-Signature=sig", f.lastBody("POST /drawings")["filename"])
		assert.Zero(t, f.callCount("POST /drawings/upload"), "no new upload URL is needed")
	})

	t.Run("refuses an upload URL nothing was uploaded to", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "create_part", map[string]any{
			"filename": "bracket.step", "upload_url": f.srv.URL + "/s3/drawings/missing.step?X-Amz-Signature=sig",
		})

		assert.Contains(t, msg, "nothing has been uploaded")
		assert.Zero(t, f.callCount("POST /drawings"))
	})

	invalid := []struct {
		name string
		args map[string]any
		want string
	}{
		{"refuses an upload URL on another host", map[string]any{"filename": "a.step", "upload_url": "https://evil.example/drawings/a.step"}, "get_upload_url returned"},
		{"refuses a plain-HTTP upload URL", map[string]any{"filename": "a.step", "upload_url": "http://sendcutsend.s3.amazonaws.com/drawings/a.step"}, "get_upload_url returned"},
		{"needs one of dxf_text and upload_url", map[string]any{"filename": "a.dxf"}, "exactly one of"},
		{"refuses both dxf_text and upload_url", map[string]any{"filename": "a.dxf", "dxf_text": "x", "upload_url": "https://sendcutsend.s3.amazonaws.com/drawings/a.dxf"}, "exactly one of"},
		{"refuses inline text under a name that isn't .dxf", map[string]any{"filename": "a.step", "dxf_text": "x"}, "must end in one of .dxf"},
		{"refuses a file name with a path", map[string]any{"filename": "../a.dxf", "dxf_text": "x"}, "without a path"},
		{"refuses unknown units", map[string]any{"filename": "a.dxf", "dxf_text": "x", "units": "ft"}, "units must be"},
		{"refuses inline text over the size limit", map[string]any{"filename": "a.dxf", "dxf_text": strings.Repeat("x", maxInlineBytes+1)}, "get_upload_url for large files"},
	}

	for _, tc := range invalid {
		t.Run(tc.name+" without calling SendCutSend", func(t *testing.T) {
			f := newFake(t)

			msg := errorText(t, connect(t, f.options(t)), "create_part", tc.args)

			assert.Contains(t, msg, tc.want)
			assert.Empty(t, f.callList())
		})
	}
}

func TestGetUploadURL(t *testing.T) {
	t.Run("returns a pre-signed URL and a command that uploads to it", func(t *testing.T) {
		f := newFake(t)

		var got UploadOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "get_upload_url", map[string]any{"filename": "my part.STEP"}), &got)

		want := f.srv.URL + "/s3/drawings/abc-bracket.dxf?X-Amz-Signature=sig"
		assert.Equal(t, want, got.UploadURL)
		assert.Equal(t, "curl -fsS -T 'my part.STEP' '"+want+"'", got.Example)
		assert.Equal(t, map[string]any{"filename": "my part.STEP"}, f.lastBody("POST /drawings/upload"))
	})

	t.Run("refuses a file type SendCutSend can't quote", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "get_upload_url", map[string]any{"filename": "part.pdf"})

		assert.Contains(t, msg, ".dxf, .dwg, .ai, .eps, .stp, .step")
		assert.Empty(t, f.callList())
	})
}

func TestQuotePart(t *testing.T) {
	t.Run("lists the materials that can cut the part, cheapest first, and why the rest can't", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-complete.json")

		var got QuoteOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "quote_part", map[string]any{"part_id": partID, "quantity": 10}), &got)

		var skus []string
		for _, q := range got.Quotes {
			skus = append(skus, q.MaterialSKU)
		}

		assert.Equal(t, []string{"CRS-030", "ALU-063", "ABSBLACK-125", "SST-500"}, skus)
		assert.Equal(t, 4, got.Quotable)
		assert.Equal(t, []UnavailableCount{
			{Reason: "min_length_short_side_hard", Materials: 1},
			{Reason: "tube_self_order_disabled_hard", Materials: 1},
		}, got.Unavailable)

		first := got.Quotes[0]
		assert.Equal(t, "Steel", first.Category)
		assert.Equal(t, ".030", first.ThicknessIn)
		assert.InDelta(t, 1.71, first.UnitPrice, 0.0001)
		assert.InDelta(t, 17.1, first.TotalPrice, 0.0001)
		assert.Empty(t, first.PriceBreaks, "price breaks come with a single material")
		assert.True(t, got.Quotes[3].OutOfStock)

		body := f.lastBody("POST /drawings/5001/quotes")
		assert.Nil(t, body["material_sku"])
		assert.InDelta(t, 10, body["quantity"], 0)
	})

	t.Run("keeps one category and the limit", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-complete.json")

		var got QuoteOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "quote_part", map[string]any{
			"part_id": partID, "quantity": 10, "category": "aluminum", "limit": 1,
		}), &got)

		require.Len(t, got.Quotes, 1)
		assert.Equal(t, "ALU-063", got.Quotes[0].MaterialSKU)
		assert.Equal(t, 4, got.Quotable, "counted before the filter")
	})

	t.Run("prices one material with price breaks and delivery dates", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-complete.json")

		var got QuoteOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "quote_part", map[string]any{
			"part_id": partID, "quantity": 10, "material_sku": "crs-030",
		}), &got)

		require.Len(t, got.Quotes, 1)
		q := got.Quotes[0]
		assert.Equal(t, "CRS-030", q.MaterialSKU)
		assert.Equal(t, "2026-11-17T08:00:00Z", q.EstShipDate)
		assert.Equal(t, "2026-11-18T08:00:00Z", q.InHandDate)
		require.NotEmpty(t, q.PriceBreaks)
		assert.Equal(t, 1, q.PriceBreaks[0].Quantity)
		assert.Equal(t, "CRS-030", f.lastBody("POST /drawings/5001/quotes")["material_sku"])
	})

	t.Run("rejects an unknown material without asking for prices", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-complete.json")

		msg := errorText(t, connect(t, f.options(t)), "quote_part", map[string]any{"part_id": partID, "quantity": 1, "material_sku": "NOPE-1"})

		assert.Contains(t, msg, `unknown material_sku "NOPE-1"`)
		assert.Zero(t, f.callCount("POST /drawings/5001/quotes"))
	})

	t.Run("refuses a part that is still processing", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-pending.json")

		msg := errorText(t, connect(t, f.options(t)), "quote_part", map[string]any{"part_id": partID, "quantity": 1})

		assert.Contains(t, msg, "still processing")
	})

	t.Run("refuses a part that can't be priced, with the reason", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-error.json")

		msg := errorText(t, connect(t, f.options(t)), "quote_part", map[string]any{"part_id": partID, "quantity": 1})

		assert.Contains(t, msg, "no_quotable_formats")
	})

	t.Run("rejects a quantity out of range without calling SendCutSend", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "quote_part", map[string]any{"part_id": partID, "quantity": 0})

		assert.Contains(t, msg, "quantity must be between 1 and 10000")
		assert.Empty(t, f.callList())
	})
}

func cartArgs(sku string) map[string]any {
	return map[string]any{
		"title": "Brackets",
		"items": []map[string]any{{"part_id": partID, "material_sku": sku, "quantity": 10}},
	}
}

func TestCreateSavedCart(t *testing.T) {
	t.Run("adds the parts, saves the cart and takes its parts out of the cart again", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-complete.json")

		var got CartOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "create_saved_cart", cartArgs("CRS-030")), &got)

		unit, total, sub, ship := 1.71, 17.1, 17.1, 15.11
		assert.Equal(t, CartOutput{
			ID: cartID, Title: "Brackets", Reference: "CKTEST01",
			CheckoutURL: f.srv.URL + "/customer#/cart?accept=dGVzdA%3D%3D%0A",
			Created:     "2026-11-13T18:04:19Z", ItemCount: 1,
			Items: []CartItemResult{{
				PartID: partID, PartName: "bracket.dxf", MaterialSKU: "CRS-030", Quantity: 10,
				UnitPrice: &unit, TotalPrice: &total,
			}},
			Subtotal: &sub, Shipping: &ship,
			EstShipDate: "2026-11-17T08:00:00.000Z", InHandDate: "2026-11-19T08:00:00.000Z",
		}, got)

		assert.Equal(t, map[string]any{"title": "Brackets", "request_approval": false}, f.lastBody("POST /saved_carts"))

		item := f.lastBody("POST /cart")
		assert.InDelta(t, 102, item["material_id"], 0, "the SKU's material ID")
		assert.InDelta(t, partID, item["drawing_id"], 0)
		assert.Equal(t, []any{}, item["operations"], "cut only")
		assert.Zero(t, f.cartLen())
	})

	t.Run("refuses when the account's cart already holds items, and leaves them alone", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-complete.json")
		f.cart = []map[string]any{{"id": float64(1)}}

		msg := errorText(t, connect(t, f.options(t)), "create_saved_cart", cartArgs("CRS-030"))

		assert.Contains(t, msg, "already holds 1 items")
		assert.Zero(t, f.callCount("POST /cart"))
		assert.Zero(t, f.callCount("DELETE /cart/1"))
		assert.Equal(t, 1, f.cartLen())
	})

	t.Run("saves nothing when a part can't be ordered in its material", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-complete.json")
		f.setOverride("POST /cart", f.signed(func(w http.ResponseWriter, _ *http.Request) {
			f.addCartItem(w, "cart-item-unorderable.json")
		}))

		msg := errorText(t, connect(t, f.options(t)), "create_saved_cart", cartArgs("CRS-030"))

		assert.Contains(t, msg, "no cart was saved")
		assert.Contains(t, msg, "min_length_short_side_hard")
		assert.Zero(t, f.callCount("POST /saved_carts"))
		assert.Zero(t, f.cartLen(), "its item is taken out again")
	})

	t.Run("saves nothing when someone else adds to the cart meanwhile, and keeps their item", func(t *testing.T) {
		f := newFake(t)
		f.setStates("drawing-complete.json")
		f.setOverride("POST /cart", f.signed(func(w http.ResponseWriter, _ *http.Request) {
			f.mu.Lock()
			f.cart = append(f.cart, map[string]any{"id": float64(1)})
			f.mu.Unlock()
			f.addCartItem(w, "cart-item.json")
		}))

		msg := errorText(t, connect(t, f.options(t)), "create_saved_cart", cartArgs("CRS-030"))

		assert.Contains(t, msg, "someone else")
		assert.Zero(t, f.callCount("POST /saved_carts"))
		assert.Equal(t, 1, f.cartLen(), "only this call's item is taken out")
	})

	t.Run("rejects an unknown material before touching the cart", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "create_saved_cart", cartArgs("NOPE-1"))

		assert.Contains(t, msg, `unknown material_sku "NOPE-1"`)
		assert.Zero(t, f.callCount("GET /cart"))
	})

	t.Run("rejects an out-of-stock material before touching the cart", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "create_saved_cart", cartArgs("SST-500"))

		assert.Contains(t, msg, "SST-500 is out of stock")
		assert.Zero(t, f.callCount("GET /cart"))
	})

	t.Run("refuses without an account", func(t *testing.T) {
		f := newFake(t)
		opts := f.options(t)
		opts.CredentialsFile = ""

		msg := errorText(t, connect(t, opts), "create_saved_cart", cartArgs("CRS-030"))

		assert.Contains(t, msg, "need a signed-in account")
	})

	invalid := []struct {
		name string
		args map[string]any
		want string
	}{
		{"an empty title", map[string]any{"title": " ", "items": []map[string]any{{"part_id": 1, "material_sku": "A", "quantity": 1}}}, "title must be"},
		{"no items", map[string]any{"title": "x", "items": []map[string]any{}}, "items must hold 1 to 25"},
		{"a zero quantity", map[string]any{"title": "x", "items": []map[string]any{{"part_id": 1, "material_sku": "A", "quantity": 0}}}, "items[0].quantity"},
		{"a missing SKU", map[string]any{"title": "x", "items": []map[string]any{{"part_id": 1, "material_sku": "", "quantity": 1}}}, "items[0].material_sku is empty"},
	}

	for _, tc := range invalid {
		t.Run("rejects "+tc.name+" without calling SendCutSend", func(t *testing.T) {
			f := newFake(t)

			msg := errorText(t, connect(t, f.options(t)), "create_saved_cart", tc.args)

			assert.Contains(t, msg, tc.want)
			assert.Empty(t, f.callList())
		})
	}
}

func TestSavedCarts(t *testing.T) {
	t.Run("lists saved carts with checkout URLs", func(t *testing.T) {
		f := newFake(t)

		var got ListCartsOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "list_saved_carts", map[string]any{}), &got)

		require.Len(t, got.Carts, 1)
		assert.Equal(t, 1, got.Total)
		assert.Equal(t, f.srv.URL+"/customer#/cart?accept=dGVzdA%3D%3D%0A", got.Carts[0].CheckoutURL)
		assert.Equal(t, 1, got.Carts[0].ItemCount)
	})

	t.Run("gets a saved cart's parts without prices", func(t *testing.T) {
		f := newFake(t)

		var got CartOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "get_saved_cart", map[string]any{"cart_id": cartID}), &got)

		assert.Equal(t, []CartItemResult{{PartID: partID, PartName: "bracket.dxf", MaterialSKU: "CRS-030", Quantity: 10}}, got.Items)
	})

	t.Run("deletes a saved cart", func(t *testing.T) {
		f := newFake(t)

		var got DeletedOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "delete_saved_cart", map[string]any{"cart_id": cartID}), &got)

		assert.Equal(t, int64(cartID), got.Deleted)
		assert.Equal(t, 1, f.callCount("DELETE /saved_carts/7001"))
	})

	t.Run("says a missing saved cart wasn't found", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "get_saved_cart", map[string]any{"cart_id": 1})

		assert.Contains(t, msg, "saved cart 1: not found")
	})
}

func TestParts(t *testing.T) {
	t.Run("lists the account's parts", func(t *testing.T) {
		f := newFake(t)

		var got ListPartsOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "list_parts", map[string]any{"limit": 2}), &got)

		assert.Equal(t, 9, got.Total)
		require.Len(t, got.Parts, 2)
		assert.Equal(t, "Optional_lacing_bar.STEP", got.Parts[0].Name, "the stored name when no customer name was given")
	})

	t.Run("says a part of another account wasn't found", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "get_part", map[string]any{"part_id": 1})

		assert.Contains(t, msg, "part 1: not found")
	})

	t.Run("deletes a part", func(t *testing.T) {
		f := newFake(t)

		var got DeletedOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "delete_part", map[string]any{"part_id": partID}), &got)

		assert.Equal(t, int64(partID), got.Deleted)
		assert.Equal(t, 1, f.callCount("DELETE /drawings/5001"))
	})

	t.Run("rejects a page before the first", func(t *testing.T) {
		f := newFake(t)

		msg := errorText(t, connect(t, f.options(t)), "list_parts", map[string]any{"page": -1})

		assert.Contains(t, msg, "page must be 1 or more")
		assert.Empty(t, f.callList())
	})
}

func TestListMaterials(t *testing.T) {
	cases := []struct {
		name string
		args map[string]any
		want []string
	}{
		{"lists sheet materials by default", map[string]any{}, []string{"CRS-030", "ALU-063", "ABSBLACK-125", "SST-500"}},
		{"keeps one category, ignoring case", map[string]any{"category": "stainless steel"}, []string{"SST-500"}},
		{"keeps materials offering a service", map[string]any{"service": "bending"}, []string{"CRS-030", "ALU-063"}},
		{"leaves out materials that are out of stock", map[string]any{"in_stock_only": true}, []string{"CRS-030", "ALU-063", "ABSBLACK-125"}},
		{"lists tubes on request", map[string]any{"form": "tube"}, []string{"DCOL-RND-125-065"}},
		{"matches text in the name or SKU", map[string]any{"query": "abs"}, []string{"ABSBLACK-125"}},
		{"stops at the limit", map[string]any{"limit": 2}, []string{"CRS-030", "ALU-063"}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)

			var got MaterialsOutput
			mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "list_materials", tc.args), &got)

			var skus []string
			for _, m := range got.Materials {
				skus = append(skus, m.SKU)
			}

			assert.Equal(t, tc.want, skus)
			assert.Equal(t, []string{"ABS", "Aluminum", "Stainless Steel", "Steel"}, got.Categories)
		})
	}

	t.Run("gives a material's services without repeats", func(t *testing.T) {
		f := newFake(t)

		var got MaterialsOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.options(t)), "list_materials", map[string]any{"query": "CRS-030"}), &got)

		require.Len(t, got.Materials, 1)

		seen := map[string]bool{}
		for _, s := range got.Materials[0].Services {
			assert.False(t, seen[s], "service %s repeated", s)
			seen[s] = true
		}

		assert.True(t, seen["bending"])
	})
}

func TestSignIn(t *testing.T) {
	t.Run("explains how to renew a cookie the site no longer accepts", func(t *testing.T) {
		f := newFake(t)
		opts := f.options(t)
		opts.CredentialsFile = filepath.Join(t.TempDir(), "creds")
		require.NoError(t, writeFile(opts.CredentialsFile, "stale%3A%3A1001"))

		msg := errorText(t, connect(t, opts), "list_parts", map[string]any{})

		assert.Contains(t, msg, "did not accept the saved sign-in cookie")
		assert.Contains(t, msg, "replace the server's secret")
		assert.Equal(t, []string{"GET /customer"}, f.callList(), "nothing else is tried once the page shows a guest session")
	})

	t.Run("explains a sign-in the API rejects partway", func(t *testing.T) {
		f := newFake(t)
		f.setOverride("GET /drawings", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusUnauthorized) })

		msg := errorText(t, connect(t, f.options(t)), "list_parts", map[string]any{})

		assert.Contains(t, msg, "did not accept the saved sign-in cookie")
	})

	t.Run("reports a missing credentials file", func(t *testing.T) {
		f := newFake(t)
		opts := f.options(t)
		opts.CredentialsFile = filepath.Join(t.TempDir(), "missing")

		msg := errorText(t, connect(t, opts), "list_parts", map[string]any{})

		assert.Contains(t, msg, "read the SendCutSend credentials file")
		assert.Empty(t, f.callList())
	})

	t.Run("reports an empty credentials file", func(t *testing.T) {
		f := newFake(t)
		opts := f.options(t)
		opts.CredentialsFile = filepath.Join(t.TempDir(), "creds")
		require.NoError(t, writeFile(opts.CredentialsFile, "\n"))

		msg := errorText(t, connect(t, opts), "list_parts", map[string]any{})

		assert.Contains(t, msg, "credentials file is empty")
	})

	t.Run("reads a replaced credentials file on the next call", func(t *testing.T) {
		f := newFake(t)
		opts := f.options(t)
		s := connect(t, opts)

		require.NoError(t, writeFile(opts.CredentialsFile, "stale"))
		assert.Contains(t, errorText(t, s, "list_parts", map[string]any{}), "did not accept")

		require.NoError(t, writeFile(opts.CredentialsFile, testCreds))

		res := mcptest.CallTool(t, s, "list_parts", map[string]any{})
		assert.False(t, res.IsError, mcptest.Text(res))
	})
}

func TestUpstreamFailures(t *testing.T) {
	cases := []struct {
		name    string
		handler http.HandlerFunc
		want    string
	}{
		{"reports a server error with its status and message", func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(w, `{"error":"database is down"}`)
		}, "HTTP 500 for GET /drawings: error: database is down"},
		{"reports a body that isn't JSON", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html>maintenance</html>")
		}, "decode SendCutSend response to GET /drawings"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake(t)
			f.setOverride("GET /drawings", tc.handler)

			msg := errorText(t, connect(t, f.options(t)), "list_parts", map[string]any{})

			assert.Contains(t, msg, tc.want)
		})
	}

	t.Run("reports an app page without a CSRF token as a site change", func(t *testing.T) {
		f := newFake(t)
		f.setOverride("GET /customer", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, "<html><head></head></html>")
		})

		msg := errorText(t, connect(t, f.options(t)), "list_parts", map[string]any{})

		assert.Contains(t, msg, "no CSRF token; the site may have changed")
	})
}

func TestDecimalAcceptsStringsNumbersAndNull(t *testing.T) {
	var got struct {
		A, B, C, D Decimal
	}

	require.NoError(t, json.Unmarshal([]byte(`{"A":"25.1","B":0,"C":null,"D":""}`), &got))

	assert.InDelta(t, 25.1, float64(got.A), 1e-9)
	assert.Zero(t, got.B)
	assert.Zero(t, got.C)
	assert.Zero(t, got.D)
	assert.Error(t, json.Unmarshal([]byte(`{"A":"abc"}`), &got))
}
