//go:build live

package printables

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestLiveAPI runs against the real printables.com API, to catch changes to
// the undocumented schema. `make test-live` runs it; CI runs it nightly.
func TestLiveAPI(t *testing.T) {
	c := NewClient(DefaultEndpoint, "live-test")

	res, err := c.Search(t.Context(), "benchy", "popular", 3, 0)
	require.NoError(t, err)
	require.NotEmpty(t, res.Items)
	assert.Positive(t, res.TotalCount)

	// A long-lived CC-BY model with a model file and a zip pack.
	m, err := c.Model(t.Context(), "770347")
	require.NoError(t, err)
	require.NotEmpty(t, m.STLs)
	require.NotEmpty(t, m.DownloadPacks)
	assert.NotEmpty(t, m.Description)

	for _, req := range []struct {
		id   string
		kind FileKind
	}{{m.STLs[0].ID, KindSTL}, {m.DownloadPacks[0].ID, KindPack}} {
		link, err := c.DownloadLink(t.Context(), m.ID, req.id, req.kind)
		require.NoError(t, err)

		// The file host rejects Go's default User-Agent, as the API does.
		head, err := http.NewRequestWithContext(t.Context(), http.MethodHead, link.URL, nil)
		require.NoError(t, err)
		head.Header.Set("User-Agent", c.userAgent)

		resp, err := http.DefaultClient.Do(head)
		require.NoError(t, err)

		_ = resp.Body.Close()
		assert.Equal(t, http.StatusOK, resp.StatusCode, "the link downloads without signing in")
	}
}
