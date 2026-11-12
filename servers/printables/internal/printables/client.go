// Package printables reads models from printables.com through the GraphQL
// API its website uses. The API is undocumented: the queries here are the
// ones the site sends, and the client's tests pin the response shapes it
// relies on.
package printables

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// DefaultEndpoint is Printables' GraphQL API.
const DefaultEndpoint = "https://api.printables.com/graphql/"

// mediaBase prefixes the image paths the API returns.
const mediaBase = "https://media.printables.com/"

// Client calls the Printables API.
type Client struct {
	endpoint  string
	userAgent string
	http      *http.Client
}

// NewClient returns a client for endpoint. The API rejects Go's default
// User-Agent, so every request names this server instead.
func NewClient(endpoint, version string) *Client {
	return &Client{
		endpoint:  endpoint,
		userAgent: "mcp-servers-printables/" + version + " (+https://github.com/solidDoWant/mcp-servers)",
		http:      &http.Client{Timeout: 30 * time.Second},
	}
}

// ErrNotFound means the model doesn't exist or isn't public.
var ErrNotFound = errors.New("model not found")

type gqlRequest struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

type gqlError struct {
	Message string `json:"message"`
}

// do sends one GraphQL request and decodes its data into out.
func (c *Client) do(ctx context.Context, query string, variables map[string]any, out any) error {
	body, err := json.Marshal(gqlRequest{Query: query, Variables: variables})
	if err != nil {
		return fmt.Errorf("encode request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("call Printables: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return fmt.Errorf("read Printables response: %w", err)
	}

	var envelope struct {
		Data   json.RawMessage `json:"data"`
		Errors []gqlError      `json:"errors"`
	}

	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("printables.com returned HTTP %d with a body that isn't JSON: %s", resp.StatusCode, snippet(raw))
	}

	if len(envelope.Errors) > 0 {
		msgs := make([]string, len(envelope.Errors))
		for i, e := range envelope.Errors {
			msgs[i] = e.Message
		}

		return fmt.Errorf("printables.com API error (HTTP %d): %s", resp.StatusCode, strings.Join(msgs, "; "))
	}

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("printables.com returned HTTP %d: %s", resp.StatusCode, snippet(raw))
	}

	if err := json.Unmarshal(envelope.Data, out); err != nil {
		return fmt.Errorf("decode Printables response: %w", err)
	}

	return nil
}

func snippet(b []byte) string {
	const limit = 200

	s := strings.TrimSpace(string(b))
	if len(s) > limit {
		s = s[:limit] + "…"
	}

	return s
}

// SearchOrder is a search result order the API accepts.
type SearchOrder string

// SearchOrders lists the orders the API accepts, best first.
var SearchOrders = []SearchOrder{"best_match", "popular", "latest", "rating", "makes_count"}

// User is a model's author.
type User struct {
	PublicUsername string `json:"publicUsername"`
	Handle         string `json:"handle"`
}

// Image is a picture of a model.
type Image struct {
	FilePath string `json:"filePath"`
}

// URL returns the image's full address.
func (i Image) URL() string {
	if i.FilePath == "" {
		return ""
	}

	return mediaBase + i.FilePath
}

// ModelSummary is a model as search results list it.
type ModelSummary struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	Slug          string  `json:"slug"`
	Summary       string  `json:"summary"`
	RatingAvg     float64 `json:"ratingAvg,string"`
	RatingCount   int     `json:"ratingCount"`
	LikesCount    int     `json:"likesCount"`
	DownloadCount int     `json:"downloadCount"`
	MakesCount    int     `json:"makesCount"`
	DatePublished string  `json:"datePublished"`
	Premium       bool    `json:"premium"`
	Price         *string `json:"price"`
	User          *User   `json:"user"`
	Image         *Image  `json:"image"`
}

// URL returns the model's page on printables.com.
func ModelURL(id, slug string) string {
	if slug == "" {
		return "https://www.printables.com/model/" + id
	}

	return "https://www.printables.com/model/" + id + "-" + slug
}

const modelSummaryFields = `
	id name slug summary ratingAvg ratingCount likesCount downloadCount makesCount
	datePublished premium price
	user { publicUsername handle }
	image { filePath }
`

const searchQuery = `query Search($query: String!, $limit: Int!, $offset: Int!, $ordering: SearchChoicesEnum!) {
  result: searchPrints2(query: $query, printType: print, limit: $limit, offset: $offset, ordering: $ordering) {
    totalCount
    items {` + modelSummaryFields + `}
  }
}`

// SearchResult is one page of search results.
type SearchResult struct {
	TotalCount int            `json:"totalCount"`
	Items      []ModelSummary `json:"items"`
}

// Search returns one page of models matching query.
func (c *Client) Search(ctx context.Context, query string, order SearchOrder, limit, offset int) (*SearchResult, error) {
	var data struct {
		Result *SearchResult `json:"result"`
	}

	err := c.do(ctx, searchQuery, map[string]any{
		"query": query, "limit": limit, "offset": offset, "ordering": string(order),
	}, &data)
	if err != nil {
		return nil, err
	}

	if data.Result == nil {
		return &SearchResult{}, nil
	}

	return data.Result, nil
}

// Tag is a model's tag.
type Tag struct {
	Name string `json:"name"`
}

// License is a model's license.
type License struct {
	Name         string `json:"name"`
	Abbreviation string `json:"abbreviation"`
}

// Category is a model's category, with its parents.
type Category struct {
	Name string `json:"name"`
	Path []struct {
		Name string `json:"name"`
	} `json:"path"`
}

// File is one downloadable file. Which fields are set depends on its kind:
// G-code and SLA files carry print settings.
type File struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Folder   string `json:"folder"`
	Note     string `json:"note"`
	FileSize int64  `json:"fileSize"`

	Printer *struct {
		Name string `json:"name"`
	} `json:"printer"`
	Material *struct {
		Name string `json:"name"`
	} `json:"material"`
	PrintDuration  *float64 `json:"printDuration"`
	LayerHeight    *float64 `json:"layerHeight"`
	NozzleDiameter *float64 `json:"nozzleDiameter"`
	Weight         *float64 `json:"weight"`
}

// DownloadPack is a zip of all of one kind of a model's files.
type DownloadPack struct {
	ID       string `json:"id"`
	FileSize int64  `json:"fileSize"`
	FileType string `json:"fileType"`
}

// Model is a model with its details and files.
type Model struct {
	ModelSummary

	Description   string         `json:"description"`
	Tags          []Tag          `json:"tags"`
	License       *License       `json:"license"`
	Category      *Category      `json:"category"`
	NSFW          bool           `json:"nsfw"`
	Images        []Image        `json:"images"`
	STLs          []File         `json:"stls"`
	GCodes        []File         `json:"gcodes"`
	SLAs          []File         `json:"slas"`
	OtherFiles    []File         `json:"otherFiles"`
	DownloadPacks []DownloadPack `json:"downloadPacks"`
}

const modelQuery = `query Model($id: ID!) {
  model: print(id: $id) {` + modelSummaryFields + `
    description nsfw
    tags { name }
    license { name abbreviation }
    category { name path { name } }
    images { filePath }
    stls { id name folder note fileSize }
    gcodes { id name folder note fileSize printer { name } material { name } printDuration layerHeight nozzleDiameter weight }
    slas { id name folder note fileSize printer { name } printDuration layerHeight }
    otherFiles { id name folder note fileSize }
    downloadPacks { id fileSize fileType }
  }
}`

// Model returns one model's details and file list.
func (c *Client) Model(ctx context.Context, id string) (*Model, error) {
	var data struct {
		Model *Model `json:"model"`
	}

	if err := c.do(ctx, modelQuery, map[string]any{"id": id}, &data); err != nil {
		return nil, err
	}

	if data.Model == nil {
		return nil, fmt.Errorf("%w: %s", ErrNotFound, id)
	}

	return data.Model, nil
}

// FileKind is the API's name for a kind of download.
type FileKind string

// The kinds of download the API hands out links for.
const (
	KindSTL   FileKind = "stl"
	KindGCode FileKind = "gcode"
	KindSLA   FileKind = "sla"
	KindOther FileKind = "other"
	KindPack  FileKind = "pack"
)

const downloadLinkMutation = `mutation DownloadLink($id: ID!, $modelId: ID!, $fileType: DownloadFileTypeEnum!) {
  getDownloadLink(id: $id, printId: $modelId, fileType: $fileType, source: model_detail) {
    ok
    errors { field messages }
    output { link ttl }
  }
}`

// DownloadLink is a direct, time-limited link to one file.
type DownloadLink struct {
	URL       string
	ExpiresAt time.Time
}

// DownloadLink asks for a direct link to one file or pack of modelID.
func (c *Client) DownloadLink(ctx context.Context, modelID, fileID string, kind FileKind) (*DownloadLink, error) {
	var data struct {
		Result *struct {
			OK     bool `json:"ok"`
			Errors []struct {
				Field    string   `json:"field"`
				Messages []string `json:"messages"`
			} `json:"errors"`
			Output *struct {
				Link string `json:"link"`
				TTL  int    `json:"ttl"`
			} `json:"output"`
		} `json:"getDownloadLink"`
	}

	issued := time.Now()

	err := c.do(ctx, downloadLinkMutation, map[string]any{
		"id": fileID, "modelId": modelID, "fileType": string(kind),
	}, &data)
	if err != nil {
		return nil, err
	}

	r := data.Result
	if r == nil {
		return nil, errors.New("printables.com returned no download link")
	}

	if !r.OK || r.Output == nil || r.Output.Link == "" {
		var msgs []string
		for _, e := range r.Errors {
			msgs = append(msgs, e.Messages...)
		}

		if len(msgs) == 0 {
			msgs = []string{"no reason given"}
		}

		return nil, fmt.Errorf("printables.com refused the download link: %s", strings.Join(msgs, "; "))
	}

	return &DownloadLink{
		URL:       r.Output.Link,
		ExpiresAt: issued.Add(time.Duration(r.Output.TTL) * time.Second).UTC().Truncate(time.Second),
	}, nil
}
