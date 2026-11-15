package tekton

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/require"
	"k8s.io/client-go/rest"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

const namespace = "ci-infra-mk3"

// request is one request the fake API server received.
type request struct {
	path      string
	query     url.Values
	userAgent string
}

// fakeAPI answers Kubernetes API requests by path with canned bodies, and
// with a Kubernetes NotFound status for any other path.
type fakeAPI struct {
	t         *testing.T
	responses map[string]func() (int, string)

	mu       sync.Mutex
	requests []request
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	return &fakeAPI{t: t, responses: map[string]func() (int, string){}}
}

func (f *fakeAPI) on(path string, status int, body string) {
	f.responses[path] = func() (int, string) { return status, body }
}

// onFixture serves the recorded response in testdata/name at path.
func (f *fakeAPI) onFixture(path, name string) {
	f.on(path, http.StatusOK, fixture(f.t, name))
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	f.requests = append(f.requests, request{path: r.URL.Path, query: r.URL.Query(), userAgent: r.Header.Get("User-Agent")})
	f.mu.Unlock()

	respond, ok := f.responses[r.URL.Path]
	if !ok {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = fmt.Fprintf(w, `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"%s not found","reason":"NotFound","code":404}`, r.URL.Path)

		return
	}

	status, body := respond()
	if body != "" && body[0] == '{' {
		w.Header().Set("Content-Type", "application/json")
	}

	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeAPI) requestsTo(path string) []request {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []request

	for _, r := range f.requests {
		if r.path == path {
			out = append(out, r)
		}
	}

	return out
}

func (f *fakeAPI) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()

	return len(f.requests)
}

func fixture(t *testing.T, name string) string {
	t.Helper()

	b, err := os.ReadFile("testdata/" + name)
	require.NoError(t, err)

	return string(b)
}

func connect(t *testing.T, api *fakeAPI) *mcp.ClientSession {
	t.Helper()

	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)

	client, err := NewClient(&rest.Config{Host: srv.URL, UserAgent: userAgent("test")})
	require.NoError(t, err)

	return mcptest.Connect(t, mcpserver.Config{
		Name:         "tekton",
		Version:      "test",
		Instructions: Instructions,
		Register:     Tools(client),
	})
}

func pipelineRunsPath(name string) string {
	p := "/apis/tekton.dev/v1/namespaces/" + namespace + "/pipelineruns"
	if name != "" {
		p += "/" + name
	}

	return p
}

func taskRunPath(name string) string {
	return "/apis/tekton.dev/v1/namespaces/" + namespace + "/taskruns/" + name
}

func logPath(pod string) string {
	return "/api/v1/namespaces/" + namespace + "/pods/" + pod + "/log"
}

// forbidden is the API server's answer to a request RBAC refuses.
const forbidden = `{"kind":"Status","apiVersion":"v1","status":"Failure","message":"pipelineruns.tekton.dev is forbidden: User \"authentik:someone\" cannot list resource \"pipelineruns\" in API group \"tekton.dev\" in the namespace \"ci-infra-mk3\"","reason":"Forbidden","details":{"group":"tekton.dev","kind":"pipelineruns"},"code":403}`
