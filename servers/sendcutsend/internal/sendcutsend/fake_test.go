package sendcutsend

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	testCreds = "0123abcd%3A%3A1001"
	testCSRF  = "test-csrf-token"
	partID    = 5001
	bracketID = 6001
	cartID    = 7001
)

// fake is a stand-in for the SendCutSend app and its upload host, serving
// recorded responses from testdata/. Like the real site, it wants the
// sign-in cookie on every request and the page's CSRF token on every write.
type fake struct {
	t   *testing.T
	srv *httptest.Server

	mu sync.Mutex
	// calls lists "METHOD /path" for every request, in order.
	calls []string
	// bodies holds the last JSON body sent to each "METHOD /path".
	bodies map[string]map[string]any
	// states are the fixtures GET /drawings/5001 serves in turn; the last
	// one repeats.
	states []string
	// uploaded holds the bodies PUT to the upload host, by path.
	uploaded map[string][]byte
	// cart holds the account's cart items.
	cart []map[string]any
	// nextCartID numbers new cart items.
	nextCartID int64
	// override replaces the handler for one "METHOD /path pattern".
	override map[string]http.HandlerFunc
}

func newFake(t *testing.T) *fake {
	t.Helper()

	f := &fake{
		t:          t,
		bodies:     map[string]map[string]any{},
		states:     []string{"drawing-pending.json", "drawing-complete.json"},
		uploaded:   map[string][]byte{},
		nextCartID: 9000,
		override:   map[string]http.HandlerFunc{},
	}

	mux := http.NewServeMux()
	route := func(pattern string, h http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			f.mu.Lock()
			o := f.override[pattern]
			f.mu.Unlock()

			if o != nil {
				o(w, r)

				return
			}

			h(w, r)
		})
	}

	route("GET /customer", f.page)
	route("POST /drawings/upload", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("upload.json"))
	}))
	route("PUT /s3/drawings/{key}", func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)

		f.mu.Lock()
		f.uploaded[r.URL.Path] = b
		f.mu.Unlock()
	})
	route("HEAD /s3/drawings/{key}", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		_, ok := f.uploaded[r.URL.Path]
		f.mu.Unlock()

		if !ok {
			w.WriteHeader(http.StatusForbidden)
		}
	})
	route("POST /drawings", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("drawing-pending.json"))
	}))
	route("GET /drawings", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("drawings-list.json"))
	}))
	route("GET /drawings/{id}", f.signed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") == strconv.Itoa(bracketID) {
			f.write(w, f.fixture("drawing-bracket.json"))

			return
		}

		if r.PathValue("id") != strconv.Itoa(partID) {
			http.Error(w, "Forbidden", http.StatusForbidden)

			return
		}

		f.mu.Lock()
		name := f.states[0]

		if len(f.states) > 1 {
			f.states = f.states[1:]
		}
		f.mu.Unlock()

		f.write(w, f.fixture(name))
	}))
	route("DELETE /drawings/{id}", f.signed(func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != strconv.Itoa(partID) {
			http.Error(w, "Forbidden", http.StatusForbidden)

			return
		}

		f.write(w, `{"all":8,"uncategorized":8}`)
	}))
	route("GET /materials", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("materials.json"))
	}))
	route("POST /drawings/{id}/quotes", f.signed(func(w http.ResponseWriter, r *http.Request) {
		body := f.body(r)
		sku, _ := body["material_sku"].(string)

		if ops, _ := body["operations"].([]any); len(ops) > 0 {
			f.write(w, f.fixture("quote-services.json"))

			return
		}

		switch sku {
		case "":
			f.write(w, f.fixture("quotes-all.json"))
		case "CRS-030":
			f.write(w, f.fixture("quote-one.json"))
		default:
			f.write(w, `{"data":[],"meta":{"est_ship_date":null,"in_hand_date":null}}`)
		}
	}))
	route("GET /cart", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		b, _ := json.Marshal(map[string]any{"data": f.cart})
		f.mu.Unlock()

		f.write(w, string(b))
	}))
	route("POST /cart", f.signed(func(w http.ResponseWriter, r *http.Request) {
		if ops, _ := f.body(r)["operations"].([]any); len(ops) > 0 {
			f.addCartItem(w, "cart-item-services.json")

			return
		}

		f.addCartItem(w, "cart-item.json")
	}))
	route("GET /tap_sizes", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("tap-sizes.json"))
	}))
	route("GET /sendcutsend-catalog.json", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("catalog.json"))
	}))
	// The CDN that serves parts' geometry: it must never see the app's
	// cookies.
	route("GET /cdn/json/{name}", func(w http.ResponseWriter, r *http.Request) {
		if len(r.Cookies()) > 0 {
			f.t.Errorf("the geometry request carried cookies: %v", r.Cookies())
		}

		f.write(w, f.fixture("model-bracket.json"))
	})
	route("DELETE /cart/{id}", f.signed(func(w http.ResponseWriter, r *http.Request) {
		id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)

		f.mu.Lock()
		for i, it := range f.cart {
			if itemID(it) == id {
				f.cart = append(f.cart[:i], f.cart[i+1:]...)

				break
			}
		}
		f.mu.Unlock()

		f.write(w, `{"meta":{"totals":{}}}`)
	}))
	route("POST /saved_carts", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.mu.Lock()
		empty := len(f.cart) == 0
		f.mu.Unlock()

		if empty {
			http.Error(w, "Saved cart not created. No eligible items found", http.StatusBadRequest)

			return
		}

		f.write(w, f.fixture("saved-cart.json"))
	}))
	route("GET /saved_carts", f.signed(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("saved-carts-list.json"))
	}))
	route("GET /saved_carts/{id}", f.signed(f.savedCartOnly(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("saved-cart.json"))
	})))
	route("GET /saved_carts/{id}/cart_items", f.signed(f.savedCartOnly(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, f.fixture("saved-cart-items.json"))
	})))
	route("DELETE /saved_carts/{id}", f.signed(f.savedCartOnly(func(w http.ResponseWriter, _ *http.Request) {
		f.write(w, `{"stats":{"all":0}}`)
	})))

	f.srv = httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.calls = append(f.calls, r.Method+" "+r.URL.Path)
		f.mu.Unlock()

		mux.ServeHTTP(w, r)
	}))
	t.Cleanup(f.srv.Close)

	return f
}

func itemID(it map[string]any) int64 {
	n, _ := it["id"].(float64)

	return int64(n)
}

// page serves the app's page: signed in when the request carries the test
// cookie, a guest session otherwise.
func (f *fake) page(w http.ResponseWriter, r *http.Request) {
	token := `"guest-token"`
	if c, err := r.Cookie(credentialsCookie); err == nil && c.Value == testCreds {
		token = "null"
	}

	w.Header().Set("Content-Type", "text/html")
	_, _ = fmt.Fprintf(w, `<!DOCTYPE html><html><head>
<meta name="csrf-param" content="authenticity_token" />
<meta name="csrf-token" content="%s" />
<script>
  window.appData = {"version":"0.0.4","drawing_api_host":"https://drawing.example","drawing_api_version":6,"user_token":%s,"sample_part":{"id":1}};
  window.other = {"a":1};
</script></head><body></body></html>`, testCSRF, token)
}

// signed wraps a handler with the site's checks: the sign-in cookie on
// every request and the CSRF token on writes.
func (f *fake) signed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if c, err := r.Cookie(credentialsCookie); err != nil || c.Value != testCreds {
			w.WriteHeader(http.StatusUnauthorized)

			return
		}

		if r.Method != http.MethodGet && r.Header.Get("X-CSRF-Token") != testCSRF {
			w.WriteHeader(http.StatusUnprocessableEntity)
			_, _ = io.WriteString(w, `{"csrf":"Something went wrong."}`)

			return
		}

		if ua := r.Header.Get("User-Agent"); !strings.HasPrefix(ua, "mcp-servers-sendcutsend/") {
			f.t.Errorf("User-Agent %q doesn't name the server", ua)
		}

		if r.Method != http.MethodGet {
			b, _ := io.ReadAll(r.Body)
			r.Body = io.NopCloser(bytes.NewReader(b))

			var m map[string]any

			_ = json.Unmarshal(b, &m)

			f.mu.Lock()
			f.bodies[r.Method+" "+r.URL.Path] = m
			f.mu.Unlock()
		}

		h(w, r)
	}
}

func (f *fake) savedCartOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("id") != strconv.Itoa(cartID) {
			http.Error(w, "404 Not Found", http.StatusNotFound)

			return
		}

		h(w, r)
	}
}

func (f *fake) addCartItem(w http.ResponseWriter, fixture string) {
	var resp map[string]any
	require.NoError(f.t, json.Unmarshal([]byte(f.fixture(fixture)), &resp))

	f.mu.Lock()
	f.nextCartID++
	item := resp["data"].(map[string]any)
	item["id"] = float64(f.nextCartID)
	f.cart = append(f.cart, item)
	f.mu.Unlock()

	b, _ := json.Marshal(resp)
	f.write(w, string(b))
}

// body decodes a request's JSON body.
func (f *fake) body(r *http.Request) map[string]any {
	var m map[string]any

	b, _ := io.ReadAll(r.Body)
	_ = json.Unmarshal(b, &m)

	return m
}

// lastBody returns the last JSON body sent to "METHOD /path".
func (f *fake) lastBody(key string) map[string]any {
	f.mu.Lock()
	defer f.mu.Unlock()

	return f.bodies[key]
}

func (f *fake) fixture(name string) string {
	b, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(f.t, err)

	out := strings.ReplaceAll(string(b), "https://{{UPLOAD_HOST}}/drawings/", f.srv.URL+"/s3/drawings/")

	return strings.ReplaceAll(out, "https://{{MODEL_HOST}}/", f.srv.URL+"/cdn/")
}

func (f *fake) write(w http.ResponseWriter, body string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, body)
}

func (f *fake) setOverride(pattern string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.override[pattern] = h
}

func (f *fake) setStates(names ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.states = names
}

func (f *fake) callCount(call string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := 0

	for _, c := range f.calls {
		if c == call {
			n++
		}
	}

	return n
}

func (f *fake) cartLen() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.cart)
}

// options are the client options for this fake. Polling is immediate and
// the processing wait never runs out, so tests don't depend on timing.
func (f *fake) options(t *testing.T) Options {
	t.Helper()

	creds := filepath.Join(t.TempDir(), "creds")
	require.NoError(t, os.WriteFile(creds, []byte(credentialsCookie+"="+testCreds+"\n"), 0o600))

	host := strings.TrimPrefix(f.srv.URL, "https://")

	return Options{
		BaseURL:         f.srv.URL,
		CredentialsFile: creds,
		Version:         "test",
		UploadHosts:     []string{host},
		ModelHosts:      []string{host},
		PollInterval:    time.Nanosecond,
		ProcessTimeout:  time.Duration(math.MaxInt64),
		HTTPClient:      f.srv.Client(),
	}
}

// connect returns an MCP client session for a server built with opts.
func connect(t *testing.T, opts Options) *mcp.ClientSession {
	t.Helper()

	c, err := NewClient(opts)
	require.NoError(t, err)

	return mcptest.Connect(t, mcpserver.Config{Name: "sendcutsend", Version: "test", Instructions: Instructions, Register: Tools(c)})
}

// errorText calls a tool that must fail and returns its message.
func errorText(t *testing.T, s *mcp.ClientSession, tool string, args any) string {
	t.Helper()

	res := mcptest.CallTool(t, s, tool, args)
	require.True(t, res.IsError, "expected %s to fail, got %s", tool, mcptest.Text(res))

	return mcptest.Text(res)
}

func (f *fake) callList() []string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]string(nil), f.calls...)
}

func (f *fake) uploadedBody(path string) string {
	f.mu.Lock()
	defer f.mu.Unlock()

	return string(f.uploaded[path])
}

func writeFile(path, content string) error {
	return os.WriteFile(path, []byte(content), 0o600)
}
