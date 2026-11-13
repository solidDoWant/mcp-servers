package electroniccomponents

import (
	"io"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/cache"
	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

func TestToolListMatchesGoldenFile(t *testing.T) {
	mcptest.AssertToolsGolden(t, connect(t, newFake(t).config()), filepath.Join("testdata", "tools.golden.json"))
}

func search(t *testing.T, cfg Config, args map[string]any) SearchOutput {
	t.Helper()

	var out SearchOutput
	mcptest.Structured(t, mcptest.CallTool(t, connect(t, cfg), "search_parts", args), &out)

	return out
}

func mpns(groups []PartGroup) []string {
	var out []string
	for _, g := range groups {
		out = append(out, g.MPN)
	}

	return out
}

func TestSearchParts(t *testing.T) {
	t.Run("groups every source's offers by part number, interleaving their rankings", func(t *testing.T) {
		f := newFake(t)

		got := search(t, f.config(), map[string]any{"query": "STM32F103C8T6"})

		assert.Equal(t, []string{"digikey", "mouser", "jlcpcb"}, got.SourcesSearched)
		assert.Empty(t, got.Errors)
		assert.Equal(t, []string{"STM32F103C8T6", "STM32F103C8T6TR", "LCKFB-DKX-STM32F103C8T6"}, mpns(got.Parts))

		g := got.Parts[0]
		require.Len(t, g.Offers, 3)
		assert.Equal(t, "STMicroelectronics", g.Manufacturer)
		assert.Equal(t, "48-LQFP", g.Package)
		assert.Equal(t, &BestOffer{Source: "jlcpcb", UnitPrice: 1.4415, TotalPrice: 1.44, Currency: "USD"}, g.Best)
	})

	t.Run("maps each source's fields", func(t *testing.T) {
		f := newFake(t)

		got := search(t, f.config(), map[string]any{"query": "STM32F103C8T6", "quantity": 10})
		offers := map[string]OfferOut{}

		for _, o := range got.Parts[0].Offers {
			offers[o.Source] = o
		}

		dk := offers["digikey"]
		assert.Equal(t, "497-6063-ND", dk.DistributorPN, "the priced, stocked variation, not the marketplace one")
		assert.Equal(t, 3120, *dk.Stock)
		assert.Equal(t, "Active", dk.Lifecycle)
		assert.Equal(t, "https://www.st.com/resource/en/datasheet/stm32f103c8t6.pdf", dk.DatasheetURL)
		assert.InDelta(t, 6.05, *dk.PriceAtQuantity, 1e-9)
		assert.Nil(t, dk.Specs, "searches leave specifications out")

		ms := offers["mouser"]
		assert.Equal(t, "511-STM32F103C8T6", ms.DistributorPN)
		assert.Equal(t, 1890, *ms.Stock)
		assert.Equal(t, "LQFP-48", ms.Package)
		assert.InDelta(t, 5.81, *ms.PriceAtQuantity, 1e-9)

		jlc := offers["jlcpcb"]
		assert.Equal(t, "C8734", jlc.DistributorPN)
		assert.Equal(t, "https://jlcpcb.com/partdetail/C8734", jlc.ProductURL)
		assert.False(t, *jlc.JLCBasic)
		assert.True(t, *jlc.JLCPreferred)
		assert.InDelta(t, 1.4415, *jlc.PriceAtQuantity, 1e-9, "a single price applies to any quantity")
	})

	t.Run("picks the cheapest offer with enough stock for the quantity", func(t *testing.T) {
		f := newFake(t)

		got := search(t, f.config(), map[string]any{"query": "STM32F103C8T6", "quantity": 210000})

		assert.Nil(t, got.Parts[0].Best, "no source has 210000 in stock")

		got = search(t, f.config(), map[string]any{"query": "STM32F103C8T6", "quantity": 2000, "sources": []string{"digikey", "mouser"}})
		assert.Equal(t, "digikey", got.Parts[0].Best.Source, "Mouser has only 1890")
		assert.InDelta(t, 9900.0, got.Parts[0].Best.TotalPrice, 1e-9)
	})

	t.Run("labels a part number made by several companies with every maker", func(t *testing.T) {
		f := newFake(t)
		f.setOverride("POST /products/v4/search/keyword", f.digikey(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"Products": [
				{"ManufacturerProductNumber": "AMS1117-3.3", "Manufacturer": {"Name": "UMW"}, "QuantityAvailable": 100,
				 "ProductVariations": [{"DigiKeyProductNumber": "4518-AMS1117-3.3TR-ND", "StandardPricing": [{"BreakQuantity": 1, "UnitPrice": 0.20}]}]},
				{"ManufacturerProductNumber": "AMS1117-3.3", "Manufacturer": {"Name": "EVVO"}, "QuantityAvailable": 100,
				 "ProductVariations": [{"DigiKeyProductNumber": "5272-AMS1117-3.3TR-ND", "StandardPricing": [{"BreakQuantity": 1, "UnitPrice": 0.10}]}]}]}`)
		}))
		f.setOverride("POST /api/v1/search/keyword", f.mouser(func(w http.ResponseWriter, _ *http.Request) {
			_, _ = io.WriteString(w, `{"Errors": [], "SearchResults": {"Parts": [
				{"ManufacturerPartNumber": "AMS1117-3.3", "Manufacturer": "umw", "MouserPartNumber": "1-AMS", "AvailabilityInStock": "5",
				 "PriceBreaks": [{"Quantity": 1, "RawPrice": 0.30, "Currency": "USD"}]}]}}`)
		}))

		got := search(t, f.config(), map[string]any{"query": "AMS1117-3.3", "sources": []string{"digikey", "mouser"}})

		require.Len(t, got.Parts, 1, "clones stay in one group")
		g := got.Parts[0]
		assert.Empty(t, g.Manufacturer)
		assert.Equal(t, []string{"UMW", "EVVO"}, g.Manufacturers, "the same maker spelt differently counts once")
		assert.Equal(t, "EVVO", g.Best.Manufacturer, "the best offer says whose part it is")
	})

	t.Run("names a single maker when jlcsearch's offers name none", func(t *testing.T) {
		f := newFake(t)

		got := search(t, f.config(), map[string]any{"query": "STM32F103C8T6", "sources": []string{"mouser", "jlcpcb"}})

		assert.Equal(t, "STMicroelectronics", got.Parts[0].Manufacturer)
		assert.Empty(t, got.Parts[0].Manufacturers)
	})

	t.Run("passes the filters to each source", func(t *testing.T) {
		f := newFake(t)

		search(t, f.config(), map[string]any{"query": "STM32F103C8T6", "limit": 7, "in_stock_only": true, "jlc_basic_only": true})

		dk := f.body("POST /products/v4/search/keyword")
		assert.InDelta(t, 7, dk["Limit"], 0)
		assert.Equal(t, map[string]any{"MinimumQuantityAvailable": 1.0}, dk["FilterOptionsRequest"])

		ms := f.body("POST /api/v1/search/keyword")["SearchByKeywordRequest"].(map[string]any)
		assert.InDelta(t, 7, ms["records"], 0)
		assert.Equal(t, "InStock", ms["searchOptions"])

		assert.Contains(t, f.query("GET /api/search"), "is_basic=true")
		assert.Contains(t, f.query("GET /api/search"), "limit=7")
	})

	t.Run("answers a repeated search from the cache without calling the sources", func(t *testing.T) {
		f := newFake(t)
		s := connect(t, f.config())
		args := map[string]any{"query": "STM32F103C8T6"}

		var first, second SearchOutput
		mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", args), &first)
		mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "  stm32f103c8t6 "}), &second)

		assert.Empty(t, first.FromCache)

		var cached []string
		for _, n := range second.FromCache {
			cached = append(cached, n.Source)
			assert.False(t, n.Stale)
		}

		assert.Equal(t, []string{"digikey", "mouser", "jlcpcb"}, cached, "case and spacing don't make a new search")
		assert.Equal(t, mpns(first.Parts), mpns(second.Parts))
		assert.Equal(t, 1, f.count("POST /products/v4/search/keyword"))
		assert.Equal(t, 1, f.count("POST /api/v1/search/keyword"))
		assert.Equal(t, 1, f.count("GET /api/search"))
	})

	t.Run("signs in to DigiKey once and reuses the token", func(t *testing.T) {
		f := newFake(t)
		s := connect(t, f.config())

		mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "STM32F103C8T6", "sources": []string{"digikey"}})
		mcptest.CallTool(t, s, "get_part", map[string]any{"part_number": "STM32F103C8T6", "sources": []string{"digikey"}})

		assert.Equal(t, 1, f.count("POST /v1/oauth2/token"))
	})

	t.Run("lists a failing source under errors and still answers from the others", func(t *testing.T) {
		f := newFake(t)
		cfg := f.config()
		cfg.MouserAPIKey = "wrong"

		got := search(t, cfg, map[string]any{"query": "STM32F103C8T6"})

		assert.Equal(t, []SourceError{{Source: "mouser", Error: "Mouser refused the request: API Key: Invalid unique identifier."}}, got.Errors)
		assert.NotEmpty(t, got.Parts)
	})

	t.Run("fails when every source fails", func(t *testing.T) {
		f := newFake(t)
		cfg := f.config()
		cfg.DigiKey.ClientSecret = "wrong"

		msg := errorText(t, connect(t, cfg), "search_parts", map[string]any{"query": "x", "sources": []string{"digikey"}})

		assert.Contains(t, msg, "DigiKey: request failed")
		assert.Contains(t, msg, "invalid_client")
	})

	t.Run("explains DigiKey's rate limiting", func(t *testing.T) {
		f := newFake(t)
		f.setOverride("POST /products/v4/search/keyword", func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Retry-After", "42")
			http.Error(w, `{"title":"Too Many Requests"}`, http.StatusTooManyRequests)
		})

		msg := errorText(t, connect(t, f.config()), "search_parts", map[string]any{"query": "x", "sources": []string{"digikey"}})

		assert.Contains(t, msg, "DigiKey returned HTTP 429 (rate limited; retry in 42s)")
	})

	t.Run("retries jlcsearch after server errors", func(t *testing.T) {
		f := newFake(t)
		failures := 2

		f.setOverride("GET /api/search", func(w http.ResponseWriter, _ *http.Request) {
			if failures > 0 {
				failures--

				http.Error(w, "oops", http.StatusInternalServerError)

				return
			}

			f.json(w, "jlc-search.json")
		})

		got := search(t, f.config(), map[string]any{"query": "STM32F103C8T6", "sources": []string{"jlcpcb"}})

		assert.Empty(t, got.Errors)
		assert.Equal(t, 3, f.count("GET /api/search"))
		assert.Len(t, got.Parts, 3)
	})

	t.Run("gives up on jlcsearch after three server errors", func(t *testing.T) {
		f := newFake(t)
		f.setOverride("GET /api/search", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "D1 DB is overloaded", http.StatusInternalServerError)
		})

		msg := errorText(t, connect(t, f.config()), "search_parts", map[string]any{"query": "x", "sources": []string{"jlcpcb"}})

		assert.Contains(t, msg, "jlcsearch returned HTTP 500: D1 DB is overloaded")
		assert.Equal(t, 3, f.count("GET /api/search"))
	})

	invalid := []struct {
		name string
		args map[string]any
		want string
	}{
		{"an empty query", map[string]any{"query": "  "}, "query is empty"},
		{"a limit over 50", map[string]any{"query": "x", "limit": 51}, "limit must be between 1 and 50"},
		{"a negative quantity", map[string]any{"query": "x", "quantity": -1}, "quantity must be between 1"},
		{"an unknown source", map[string]any{"query": "x", "sources": []string{"arrow"}}, `unknown source "arrow"`},
		{"control characters", map[string]any{"query": "a\nb"}, "control characters"},
	}

	for _, tc := range invalid {
		t.Run("rejects "+tc.name+" without calling a source", func(t *testing.T) {
			f := newFake(t)

			assert.Contains(t, errorText(t, connect(t, f.config()), "search_parts", tc.args), tc.want)
			assert.Zero(t, f.count("GET /api/search")+f.count("POST /api/v1/search/keyword")+f.count("POST /products/v4/search/keyword"))
		})
	}
}

// Mouser takes its key in the URL, and Go's HTTP errors quote the URL.
func TestMouserKeyNeverAppearsInErrors(t *testing.T) {
	t.Run("when the connection fails", func(t *testing.T) {
		f := newFake(t)
		cfg := f.config()
		cfg.MouserURL = "https://127.0.0.1:1" // nothing listens here

		msg := errorText(t, connect(t, cfg), "search_parts", map[string]any{"query": "x", "sources": []string{"mouser"}})

		assert.Contains(t, msg, "Mouser: request failed")
		assert.NotContains(t, msg, testMouserKey)
		assert.NotContains(t, msg, "apiKey")
	})

	t.Run("when Mouser answers with an error status", func(t *testing.T) {
		f := newFake(t)
		f.setOverride("POST /api/v1/search/keyword", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "upstream trouble", http.StatusBadGateway)
		})

		msg := errorText(t, connect(t, f.config()), "search_parts", map[string]any{"query": "x", "sources": []string{"mouser"}})

		assert.Contains(t, msg, "Mouser returned HTTP 502: upstream trouble")
		assert.NotContains(t, msg, testMouserKey)
	})
}

func TestGetPart(t *testing.T) {
	t.Run("returns every source's offer with specifications", func(t *testing.T) {
		f := newFake(t)

		var got GetPartOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.config()), "get_part", map[string]any{"part_number": "STM32F103C8T6"}), &got)

		require.Len(t, got.Offers, 3)

		dk := got.Offers[0]
		assert.Equal(t, "digikey", dk.Source)
		assert.Equal(t, "72MHz", dk.Specs["Speed"])
		assert.NotContains(t, dk.Specs, "Grade", "empty values are left out")

		assert.Equal(t, "mouser", got.Offers[1].Source)
		assert.Equal(t, "ARM Microcontrollers - MCU", got.Offers[1].Specs["Category"])
		assert.Equal(t, "Exact", f.body("POST /api/v1/search/partnumber")["SearchByPartRequest"].(map[string]any)["partSearchOptions"])

		assert.Equal(t, "jlcpcb", got.Offers[2].Source)
		assert.Equal(t, "STM32F103C8T6", got.Offers[2].MPN, "jlcsearch's longer matches are left out")
	})

	t.Run("finds a part by its LCSC number", func(t *testing.T) {
		f := newFake(t)

		var got GetPartOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.config()), "get_part", map[string]any{"part_number": "C8734", "sources": []string{"jlcpcb"}}), &got)

		require.Len(t, got.Offers, 1)
		assert.Equal(t, "STM32F103C8T6", got.Offers[0].MPN)
		assert.Nil(t, got.Offers[0].Stock, "jlcsearch doesn't know the stock of an exact lookup")
	})

	t.Run("answers no offers when DigiKey doesn't carry the part", func(t *testing.T) {
		f := newFake(t)

		var got GetPartOutput
		mcptest.Structured(t, mcptest.CallTool(t, connect(t, f.config()), "get_part", map[string]any{"part_number": "NOPE-123", "sources": []string{"digikey"}}), &got)

		assert.Empty(t, got.Offers)
		assert.Empty(t, got.Errors)
	})

	t.Run("escapes a part number with a slash for DigiKey", func(t *testing.T) {
		f := newFake(t)
		seen := ""

		f.setOverride("GET /products/v4/search/{pn}/productdetails", func(w http.ResponseWriter, r *http.Request) {
			seen = r.PathValue("pn")
			http.NotFound(w, r)
		})

		mcptest.CallTool(t, connect(t, f.config()), "get_part", map[string]any{"part_number": "AB/CD#1", "sources": []string{"digikey"}})

		assert.Equal(t, "AB/CD#1", seen)
	})

	t.Run("refuses several part numbers at once", func(t *testing.T) {
		f := newFake(t)

		assert.Contains(t, errorText(t, connect(t, f.config()), "get_part", map[string]any{"part_number": "A|B"}), "without '|'")
	})
}

func TestSources(t *testing.T) {
	t.Run("enables only the sources with credentials", func(t *testing.T) {
		f := newFake(t)
		cfg := f.config()
		cfg.DigiKey.ClientSecret = ""
		cfg.MouserAPIKey = ""
		s := connect(t, cfg)

		var got SourcesOutput
		mcptest.Structured(t, mcptest.CallTool(t, s, "list_sources", map[string]any{}), &got)

		assert.Equal(t, []SourceInfo{
			{Source: "digikey", Reason: "no DigiKey client ID and secret are configured"},
			{Source: "mouser", Reason: "no Mouser Search API key is configured"},
			{Source: "jlcpcb", Enabled: true},
		}, got.Sources)

		var out SearchOutput
		mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "STM32F103C8T6"}), &out)
		assert.Equal(t, []string{"jlcpcb"}, out.SourcesSearched)

		assert.Contains(t, errorText(t, s, "search_parts", map[string]any{"query": "x", "sources": []string{"mouser"}}),
			"source mouser isn't available: no Mouser Search API key is configured")
	})

	t.Run("fails clearly when no source is enabled", func(t *testing.T) {
		msg := errorText(t, connect(t, Config{JLCSearchDisabled: true}), "search_parts", map[string]any{"query": "x"})

		assert.Contains(t, msg, "no source is enabled")
	})

	t.Run("counts calls against the daily budget, but not cache hits", func(t *testing.T) {
		f := newFake(t)
		cfg := f.config()
		cfg.MouserBudget = BudgetConfig{Daily: 1}
		s := connect(t, cfg)

		mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "STM32F103C8T6", "sources": []string{"mouser"}})
		mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "STM32F103C8T6", "sources": []string{"mouser"}})

		var got SourcesOutput
		mcptest.Structured(t, mcptest.CallTool(t, s, "list_sources", map[string]any{}), &got)
		assert.Equal(t, SourceInfo{Source: "mouser", Enabled: true, CallsToday: 1, DailyLimit: 1}, got.Sources[1])

		var out SearchOutput
		mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", map[string]any{"query": "another part"}), &out)
		require.Len(t, out.Errors, 1)
		assert.Equal(t, "mouser", out.Errors[0].Source)
		assert.Contains(t, out.Errors[0].Error, "Mouser's daily limit of 1 calls is spent; it resets at 00:00 UTC")
	})
}

func TestMakerKeyIgnoresCaseSpacingAndPunctuation(t *testing.T) {
	assert.Equal(t, makerKey("STMicroelectronics"), makerKey("ST Microelectronics"))
	assert.Equal(t, makerKey("Texas Instruments"), makerKey("TEXAS-INSTRUMENTS"))
	assert.NotEqual(t, makerKey("Texas Instruments"), makerKey("Microchip"))
	assert.Empty(t, makerKey(" - "))
}

func TestStockCount(t *testing.T) {
	for in, want := range map[string]*int{"1234": intPtr(1234), "1,234 In Stock": intPtr(1234), "None": nil, "": nil, " 7 On Order": intPtr(7)} {
		assert.Equal(t, want, stockCount(in), "%q", in)
	}
}

// testClock is a controllable time source for cache lifetimes.
type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.t = c.t.Add(d)
}

func TestCachedAnswers(t *testing.T) {
	t.Run("answers with an older result, saying so, when a source fails", func(t *testing.T) {
		f := newFake(t)
		clk := &testClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
		cfg := f.config()
		cfg.Now, cfg.CacheStaleFor = clk.now, 24*time.Hour
		s := connect(t, cfg)
		args := map[string]any{"query": "STM32F103C8T6", "sources": []string{"jlcpcb"}}

		mcptest.CallTool(t, s, "search_parts", args)

		clk.advance(2 * time.Hour)
		f.setOverride("GET /api/search", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "D1 DB is overloaded", http.StatusInternalServerError)
		})

		var got SearchOutput
		mcptest.Structured(t, mcptest.CallTool(t, s, "search_parts", args), &got)

		assert.Empty(t, got.Errors, "the stale answer stands in for the failure")
		assert.Len(t, got.Parts, 3)
		require.Len(t, got.FromCache, 1)
		assert.Equal(t, "jlcpcb", got.FromCache[0].Source)
		assert.True(t, got.FromCache[0].Stale)
		assert.Equal(t, 7200, got.FromCache[0].AgeSeconds)
		assert.Contains(t, got.FromCache[0].Error, "jlcsearch returned HTTP 500")
	})

	t.Run("fails once the older result is past its stale window", func(t *testing.T) {
		f := newFake(t)
		clk := &testClock{t: time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)}
		cfg := f.config()
		cfg.Now, cfg.CacheStaleFor = clk.now, time.Hour
		s := connect(t, cfg)
		args := map[string]any{"query": "STM32F103C8T6", "sources": []string{"jlcpcb"}}

		mcptest.CallTool(t, s, "search_parts", args)

		clk.advance(3 * time.Hour)
		f.setOverride("GET /api/search", func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "down", http.StatusInternalServerError)
		})

		assert.Contains(t, errorText(t, s, "search_parts", args), "jlcsearch returned HTTP 500")
	})

	t.Run("shares answers and the day's count between servers over one Redis store", func(t *testing.T) {
		f := newFake(t)
		m := miniredis.RunT(t)
		open := func() Config {
			store, err := cache.NewRedisStore("redis://"+m.Addr(), cache.TLSFiles{})
			require.NoError(t, err)
			t.Cleanup(func() { _ = store.Close() })

			cfg := f.config()
			cfg.CacheStore = store

			return cfg
		}
		first, second := connect(t, open()), connect(t, open())
		args := map[string]any{"query": "STM32F103C8T6", "sources": []string{"mouser"}}

		mcptest.CallTool(t, first, "search_parts", args)

		var got SearchOutput
		mcptest.Structured(t, mcptest.CallTool(t, second, "search_parts", args), &got)
		require.Len(t, got.FromCache, 1)
		assert.Equal(t, 1, f.count("POST /api/v1/search/keyword"), "the second server didn't ask Mouser again")

		var sources SourcesOutput
		mcptest.Structured(t, mcptest.CallTool(t, second, "list_sources", map[string]any{}), &sources)
		assert.Equal(t, "redis", sources.CacheBackend)
		assert.Equal(t, 1, sources.Sources[1].CallsToday, "the first server's call counts for both")
	})
}
