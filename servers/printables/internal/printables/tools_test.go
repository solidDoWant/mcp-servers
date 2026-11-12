package printables

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/internal/mcptest"
)

// call is one request the fake API received.
type call struct {
	operation string
	variables map[string]any
	userAgent string
}

// fakeAPI answers GraphQL requests by operation with canned bodies.
type fakeAPI struct {
	t         *testing.T
	responses map[string]func(vars map[string]any) (int, string)

	mu    sync.Mutex
	calls []call
}

func newFakeAPI(t *testing.T) *fakeAPI {
	t.Helper()

	return &fakeAPI{t: t, responses: map[string]func(map[string]any) (int, string){}}
}

func (f *fakeAPI) on(operation string, status int, body string) {
	f.responses[operation] = func(map[string]any) (int, string) { return status, body }
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, err := io.ReadAll(r.Body)
	require.NoError(f.t, err)

	var req gqlRequest
	require.NoError(f.t, json.Unmarshal(raw, &req))

	// "query Search(...)" or "mutation DownloadLink(...)": the second word.
	fields := strings.FieldsFunc(req.Query, func(r rune) bool { return r == ' ' || r == '(' || r == '\n' })
	require.GreaterOrEqual(f.t, len(fields), 2)
	operation := fields[1]

	f.mu.Lock()
	f.calls = append(f.calls, call{operation: operation, variables: req.Variables, userAgent: r.Header.Get("User-Agent")})
	f.mu.Unlock()

	respond, ok := f.responses[operation]
	if !ok {
		f.t.Errorf("unexpected operation %q", operation)
		w.WriteHeader(http.StatusInternalServerError)

		return
	}

	status, body := respond(req.Variables)
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}

func (f *fakeAPI) callsTo(operation string) []call {
	f.mu.Lock()
	defer f.mu.Unlock()

	var out []call

	for _, c := range f.calls {
		if c.operation == operation {
			out = append(out, c)
		}
	}

	return out
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

	return mcptest.Connect(t, mcpserver.Config{
		Name:         "printables",
		Version:      "test",
		Instructions: Instructions,
		Register:     Tools(NewClient(srv.URL, "test")),
	})
}

func TestToolListMatchesGolden(t *testing.T) {
	mcptest.AssertToolsGolden(t, connect(t, newFakeAPI(t)), "testdata/tools.golden.json")
}

func TestSearchReturnsOnePageOfModels(t *testing.T) {
	api := newFakeAPI(t)
	api.on("Search", http.StatusOK, fixture(t, "search.json"))

	res := mcptest.CallTool(t, connect(t, api), "search_models", map[string]any{"query": "rpi 5 case"})

	var out SearchOutput
	mcptest.Structured(t, res, &out)

	assert.Equal(t, 3866, out.Total)
	require.Len(t, out.Models, 2)
	assert.Equal(t, ModelResult{
		ID:           "770347",
		Name:         "Rpi 5 case dinrail mount",
		URL:          "https://www.printables.com/model/770347-rpi-5-case-dinrail-mount",
		Summary:      "Din rail mount for the Rpi 5 case from Pihut.",
		Author:       "Jercorin",
		Rating:       3,
		Ratings:      1,
		Likes:        7,
		Downloads:    55,
		Makes:        2,
		Published:    "2024-03-30T12:45:43.566887+00:00",
		ThumbnailURL: "https://media.printables.com/media/prints/770347/images/5993155/case1.jpg",
	}, out.Models[0])
	assert.True(t, out.Models[1].Paid, "a Store model is marked paid")
	assert.Zero(t, out.Models[1].Rating, "an unrated model reports 0")

	calls := api.callsTo("Search")
	require.Len(t, calls, 1)
	assert.Equal(t, map[string]any{"query": "rpi 5 case", "limit": float64(10), "offset": float64(0), "ordering": "best_match"}, calls[0].variables)
	assert.True(t, strings.HasPrefix(calls[0].userAgent, "mcp-servers-printables/test "), "names itself, since the API rejects Go's default User-Agent")
}

func TestSearchPassesSortLimitAndOffset(t *testing.T) {
	api := newFakeAPI(t)
	api.on("Search", http.StatusOK, fixture(t, "search.json"))

	res := mcptest.CallTool(t, connect(t, api), "search_models", map[string]any{"query": "benchy", "sort": "popular", "limit": 50, "offset": 20})
	require.False(t, res.IsError, mcptest.Text(res))

	calls := api.callsTo("Search")
	require.Len(t, calls, 1)
	assert.Equal(t, map[string]any{"query": "benchy", "limit": float64(50), "offset": float64(20), "ordering": "popular"}, calls[0].variables)
}

func TestSearchRejectsBadInputWithoutCallingTheAPI(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"blank query", map[string]any{"query": "  "}, "query is empty"},
		{"unknown sort", map[string]any{"query": "x", "sort": "newest"}, "sort must be one of"},
		{"limit above the maximum", map[string]any{"query": "x", "limit": 51}, "limit must be between 1 and 50"},
		{"negative limit", map[string]any{"query": "x", "limit": -1}, "limit must be between 1 and 50"},
		{"negative offset", map[string]any{"query": "x", "offset": -1}, "offset must not be negative"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t)
			res := mcptest.CallTool(t, connect(t, api), "search_models", tt.args)

			assert.True(t, res.IsError)
			assert.Contains(t, mcptest.Text(res), tt.want)
			assert.Empty(t, api.callsTo("Search"))
		})
	}
}

func TestAPIFailuresAreToolErrorsNotEmptyResults(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"GraphQL error", http.StatusBadRequest, `{"errors":[{"message":"Cannot query field 'x'"}]}`, "printables.com API error (HTTP 400): Cannot query field 'x'"},
		{"Cloudflare block page", http.StatusForbidden, "<html>Just a moment...</html>", "printables.com returned HTTP 403 with a body that isn't JSON: <html>Just a moment...</html>"},
		{"server error with JSON", http.StatusBadGateway, `{"data":null}`, "printables.com returned HTTP 502"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on("Search", tt.status, tt.body)

			res := mcptest.CallTool(t, connect(t, api), "search_models", map[string]any{"query": "x"})

			assert.True(t, res.IsError)
			assert.Contains(t, mcptest.Text(res), tt.want)
		})
	}
}

func TestGetModelReturnsDetailsFilesAndPacks(t *testing.T) {
	api := newFakeAPI(t)
	api.on("Model", http.StatusOK, fixture(t, "model.json"))

	res := mcptest.CallTool(t, connect(t, api), "get_model", map[string]any{"model": "770347"})

	var out ModelOutput
	mcptest.Structured(t, res, &out)

	assert.Equal(t, "Rpi 5 case dinrail mount", out.Name)
	assert.Equal(t, "Easy to print Dinrail mount for the Rpi 5 case from [Pihut](https://thepihut.com).\n\nFor use with M2 screws and nuts.", out.Description)
	assert.Equal(t, []string{"rpi", "dinrail"}, out.Tags)
	assert.Equal(t, "CC-BY", out.License)
	assert.Equal(t, "Hobby & Makers > Mechanical Parts", out.Category)
	assert.Equal(t, []string{
		"https://media.printables.com/media/prints/770347/images/5993155/case1.jpg",
		"https://media.printables.com/media/prints/770347/images/5993156/case2.jpg",
	}, out.ImageURLs)

	minutes, layer, nozzle, weight := 62.0, 0.2, 0.4, 21.5
	assert.Equal(t, []FileResult{
		{ID: "3269904", Name: "RpiDinRail_M2.stl", Kind: "model", SizeBytes: 175584},
		{ID: "3496939", Name: "RpiDinRail.step", Kind: "model", SizeBytes: 222099},
		{
			ID: "411", Name: "RpiDinRail_0.2mm_PLA_MK4.bgcode", Kind: "gcode", SizeBytes: 1048576,
			Printer: "Original Prusa MK4", Material: "PLA", PrintMinutes: &minutes, LayerHeightMM: &layer, NozzleMM: &nozzle, WeightGrams: &weight,
		},
	}, out.Files)
	assert.Equal(t, []PackResult{
		{ID: "3621974", Contents: "model_files", SizeBytes: 138649},
		{ID: "3621975", Contents: "other_files", SizeBytes: 138649},
	}, out.Packs)

	calls := api.callsTo("Model")
	require.Len(t, calls, 1)
	assert.Equal(t, map[string]any{"id": "770347"}, calls[0].variables)
}

func TestGetModelReportsAMissingModel(t *testing.T) {
	api := newFakeAPI(t)
	api.on("Model", http.StatusOK, `{"data":{"model":null}}`)

	res := mcptest.CallTool(t, connect(t, api), "get_model", map[string]any{"model": "1"})

	assert.True(t, res.IsError)
	assert.Contains(t, mcptest.Text(res), "model not found: 1")
}

func TestModelIDAcceptsIDsAndPageURLs(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"770347", "770347"},
		{" 770347 ", "770347"},
		{"https://www.printables.com/model/770347-rpi-5-case-dinrail-mount", "770347"},
		{"https://www.printables.com/model/770347-rpi-5-case-dinrail-mount/files", "770347"},
		{"https://printables.com/model/770347", "770347"},
		{"https://www.printables.com/de/model/770347-rpi", "770347"},
		{"https://www.printables.com/model/770347?lang=cs", "770347"},
	}

	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got, err := parseModelID(tt.in)
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}

	for _, bad := range []string{"", "rpi case", "https://example.com/model/770347", "https://www.printables.com/@user/models", "770347abc"} {
		t.Run("rejects "+bad, func(t *testing.T) {
			_, err := parseModelID(bad)
			assert.Error(t, err)
		})
	}
}

func TestGetDownloadLinksAsksForEachFileByItsKind(t *testing.T) {
	api := newFakeAPI(t)
	api.on("Model", http.StatusOK, fixture(t, "model.json"))
	api.on("DownloadLink", http.StatusOK, fixture(t, "link.json"))

	before := time.Now().UTC()
	res := mcptest.CallTool(t, connect(t, api), "get_download_links", map[string]any{
		"model":    "https://www.printables.com/model/770347-rpi-5-case-dinrail-mount",
		"file_ids": []string{"3496939", "411"},
		"pack_ids": []string{"3621974"},
	})

	var out LinksOutput
	mcptest.Structured(t, res, &out)

	require.Len(t, out.Links, 3)
	assert.Equal(t, []string{"RpiDinRail.step", "RpiDinRail_0.2mm_PLA_MK4.bgcode", "rpi-5-case-dinrail-mount-model_files.zip"},
		[]string{out.Links[0].Name, out.Links[1].Name, out.Links[2].Name})
	assert.Equal(t, int64(138649), out.Links[2].SizeBytes)
	assert.Equal(t, "https://files.printables.com/media/prints/770347/stls/5993094/rpidinrail_m2.stl", out.Links[0].URL)

	expires, err := time.Parse(time.RFC3339, out.Links[0].ExpiresAt)
	require.NoError(t, err)
	assert.WithinDuration(t, before.Add(24*time.Hour), expires, time.Minute)

	calls := api.callsTo("DownloadLink")
	require.Len(t, calls, 3)
	assert.Equal(t, map[string]any{"id": "3496939", "modelId": "770347", "fileType": "stl"}, calls[0].variables)
	assert.Equal(t, map[string]any{"id": "411", "modelId": "770347", "fileType": "gcode"}, calls[1].variables)
	assert.Equal(t, map[string]any{"id": "3621974", "modelId": "770347", "fileType": "pack"}, calls[2].variables)
}

func TestGetDownloadLinksRejectsIDsFromOtherModels(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"unknown file", map[string]any{"model": "770347", "file_ids": []string{"999"}}, "model 770347 has no file with ID 999"},
		{"pack ID given as a file", map[string]any{"model": "770347", "file_ids": []string{"3621974"}}, "has no file with ID 3621974"},
		{"unknown pack", map[string]any{"model": "770347", "pack_ids": []string{"3269904"}}, "has no pack with ID 3269904"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t)
			api.on("Model", http.StatusOK, fixture(t, "model.json"))

			res := mcptest.CallTool(t, connect(t, api), "get_download_links", tt.args)

			assert.True(t, res.IsError)
			assert.Contains(t, mcptest.Text(res), tt.want)
			assert.Empty(t, api.callsTo("DownloadLink"), "no link is requested when any ID is wrong")
		})
	}
}

func TestGetDownloadLinksLimitsTheRequest(t *testing.T) {
	tests := []struct {
		name string
		args map[string]any
		want string
	}{
		{"no IDs", map[string]any{"model": "770347"}, "give at least one ID"},
		{"too many IDs", map[string]any{"model": "770347", "file_ids": make([]string, 26)}, "at most 25 links per call, not 26"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			api := newFakeAPI(t)
			res := mcptest.CallTool(t, connect(t, api), "get_download_links", tt.args)

			assert.True(t, res.IsError)
			assert.Contains(t, mcptest.Text(res), tt.want)
			assert.Empty(t, api.callsTo("Model"))
		})
	}
}

func TestGetDownloadLinksReportsARefusal(t *testing.T) {
	api := newFakeAPI(t)
	api.on("Model", http.StatusOK, fixture(t, "model.json"))
	api.on("DownloadLink", http.StatusOK, fixture(t, "link-refused.json"))

	res := mcptest.CallTool(t, connect(t, api), "get_download_links", map[string]any{"model": "770347", "file_ids": []string{"3269904"}})

	assert.True(t, res.IsError)
	assert.Contains(t, mcptest.Text(res), "RpiDinRail_M2.stl (3269904): printables.com refused the download link: files_cannot_be_downloaded")
}

func TestDescriptionIsCutToALimit(t *testing.T) {
	md, err := descriptionMarkdown("<p>" + strings.Repeat("é", maxDescription+10) + "</p>")
	require.NoError(t, err)

	assert.True(t, strings.HasSuffix(md, "[description cut at 20000 characters]"))
	assert.Equal(t, maxDescription, strings.Count(md, "é"), "cuts on characters, not bytes")
}
