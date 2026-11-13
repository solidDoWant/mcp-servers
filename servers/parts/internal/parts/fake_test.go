package parts

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

const (
	testClientID     = "test-client-id"
	testClientSecret = "test-client-secret"
	testMouserKey    = "MOUSER-SECRET-KEY-1234"
)

// fake plays DigiKey, Mouser and jlcsearch with recorded responses. Like
// the real APIs, DigiKey wants its OAuth token and client ID header, and
// Mouser its key.
type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu       sync.Mutex
	calls    map[string]int
	bodies   map[string]map[string]any
	queries  map[string]string
	override map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fake {
	t.Helper()

	f := &fake{
		t: t, calls: map[string]int{}, bodies: map[string]map[string]any{},
		queries: map[string]string{}, override: map[string]http.HandlerFunc{},
	}

	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))

			var body map[string]any

			_ = json.Unmarshal(b, &body)

			f.mu.Lock()
			f.calls[pattern]++
			f.bodies[pattern] = body
			f.queries[pattern] = r.URL.RawQuery
			o := f.override[pattern]
			f.mu.Unlock()

			if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "mcp-servers-parts/") {
				f.t.Errorf("User-Agent %q doesn't name the server", ua)
			}

			if o != nil {
				o(w, r)

				return
			}

			h(w, r)
		})
	}

	route("POST /v1/oauth2/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()

		if r.PostForm.Get("client_id") != testClientID || r.PostForm.Get("client_secret") != testClientSecret ||
			r.PostForm.Get("grant_type") != "client_credentials" {
			http.Error(w, `{"error":"invalid_client"}`, http.StatusUnauthorized)

			return
		}

		f.json(w, "digikey-token.json")
	})
	route("POST /products/v4/search/keyword", f.digikey(func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, "digikey-keyword.json")
	}))
	route("GET /products/v4/search/{pn}/productdetails", f.digikey(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("pn") != "STM32F103C8T6" {
			http.Error(w, `{"title":"Not Found","status":404}`, http.StatusNotFound)

			return
		}

		f.json(w, "digikey-details.json")
	}))
	route("POST /api/v1/search/keyword", f.mouser(func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, "mouser-keyword.json")
	}))
	route("POST /api/v1/search/partnumber", f.mouser(func(w http.ResponseWriter, _ *http.Request) {
		f.json(w, "mouser-partnumber.json")
	}))
	route("GET /api/search", func(w http.ResponseWriter, r *http.Request) {
		switch q := r.URL.Query().Get("q"); {
		case q == "C8734":
			f.json(w, "jlc-lookup.json")
		case r.URL.Query().Get("is_basic") == "true":
			f.json(w, "jlc-basic.json")
		case strings.Contains(strings.ToUpper(q), "STM32F103C8T6"):
			f.json(w, "jlc-search.json")
		default:
			f.json(w, "jlc-empty.json")
		}
	})

	f.srv = httptest.NewTLSServer(mux)
	t.Cleanup(f.srv.Close)

	return f
}

func (f *fake) digikey(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-access-token" || r.Header.Get("X-DIGIKEY-Client-Id") != testClientID {
			http.Error(w, `{"title":"Unauthorized","status":401}`, http.StatusUnauthorized)

			return
		}

		h(w, r)
	}
}

func (f *fake) mouser(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("apiKey") != testMouserKey {
			f.json(w, "mouser-bad-key.json")

			return
		}

		h(w, r)
	}
}

func (f *fake) json(w http.ResponseWriter, name string) {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(f.t, err)

	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (f *fake) setOverride(pattern string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.override[pattern] = h
}

func (f *fake) count(pattern string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.calls[pattern]
}

func (f *fake) body(pattern string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.bodies[pattern]
}

func (f *fake) query(pattern string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.queries[pattern]
}

// config enables every source against the fake, with budgets that never
// make a test wait.
func (f *fake) config() Config {
	return Config{
		Version:      "test",
		DigiKey:      DigiKeyConfig{BaseURL: f.srv.URL, ClientID: testClientID, ClientSecret: testClientSecret},
		MouserURL:    f.srv.URL,
		MouserAPIKey: testMouserKey,
		JLCSearchURL: f.srv.URL,
		HTTPClient:   f.srv.Client(),
		Wait:         func(context.Context, time.Duration) error { return nil },
	}
}

func connect(t *testing.T, cfg Config) *mcp.ClientSession {
	t.Helper()

	return mcptest.Connect(t, mcpserver.Config{Name: "parts", Version: "test", Instructions: Instructions, Register: Tools(New(cfg))})
}

func errorText(t *testing.T, s *mcp.ClientSession, tool string, args any) string {
	t.Helper()

	res := mcptest.CallTool(t, s, tool, args)
	require.True(t, res.IsError, "expected %s to fail, got %s", tool, mcptest.Text(res))

	return mcptest.Text(res)
}
