package printables

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	htmltomarkdown "github.com/JohannesKaufmann/html-to-markdown/v2"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Instructions is sent to clients with the tool list.
const Instructions = `Find 3D-printable models on printables.com and get direct download links for their files.
Typical flow: search_models, then get_model for a promising result (description, license, files), then get_download_links for the files you want.
Download links are plain HTTPS URLs that work for 24 hours without signing in. Fetch them yourself (for example curl -fL -o <name> <url>) instead of reading the file into the conversation.`

const (
	defaultLimit      = 10
	maxLimit          = 50
	maxImages         = 5
	maxDescription    = 20000
	maxLinksPerCall   = 25
	searchDescription = "Search printables.com for 3D-printable models. Returns one page of results with IDs to pass to get_model."
	modelDescription  = "Get one Printables model's details: description, license, tags, category, images, and its files and zip packs with the IDs get_download_links takes."
	linksDescription  = "Get direct download URLs for some of a Printables model's files or zip packs. The URLs work for 24 hours without signing in; download them with curl or wget rather than reading the files into the conversation. The file host rejects some HTTP libraries' default User-Agent (Go, python-requests), so set your own if you use one. Store (paid) models refuse."
)

// Tools registers the server's tools on s.
func Tools(c *Client) func(*mcp.Server) {
	return func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "search_models", Description: searchDescription, Annotations: readOnly("Search Printables")}, c.searchTool)
		mcp.AddTool(s, &mcp.Tool{Name: "get_model", Description: modelDescription, Annotations: readOnly("Get a Printables model")}, c.modelTool)
		mcp.AddTool(s, &mcp.Tool{Name: "get_download_links", Description: linksDescription, Annotations: readOnly("Get Printables download links")}, c.linksTool)
	}
}

func readOnly(title string) *mcp.ToolAnnotations {
	openWorld := true

	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &openWorld}
}

// SearchInput is search_models' input.
type SearchInput struct {
	Query  string `json:"query" jsonschema:"what to search for, e.g. raspberry pi 5 case"`
	Sort   string `json:"sort,omitempty" jsonschema:"result order: best_match (default), popular, latest, rating or makes_count"`
	Limit  int    `json:"limit,omitempty" jsonschema:"results per page, 1 to 50 (default 10)"`
	Offset int    `json:"offset,omitempty" jsonschema:"results to skip, for the next page"`
}

// SearchOutput is search_models' output.
type SearchOutput struct {
	Total  int           `json:"total" jsonschema:"how many models match in all"`
	Offset int           `json:"offset"`
	Models []ModelResult `json:"models"`
}

// ModelResult is one search result.
type ModelResult struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	URL          string  `json:"url"`
	Summary      string  `json:"summary,omitempty"`
	Author       string  `json:"author,omitempty"`
	Rating       float64 `json:"rating" jsonschema:"average rating out of 5; 0 when unrated"`
	Ratings      int     `json:"ratings"`
	Likes        int     `json:"likes"`
	Downloads    int     `json:"downloads"`
	Makes        int     `json:"makes" jsonschema:"how many people posted a print of it"`
	Published    string  `json:"published,omitempty"`
	Paid         bool    `json:"paid,omitempty" jsonschema:"a Store model, which can't be downloaded here"`
	ThumbnailURL string  `json:"thumbnail_url,omitempty"`
}

func toResult(m ModelSummary) ModelResult {
	r := ModelResult{
		ID:        m.ID,
		Name:      m.Name,
		URL:       ModelURL(m.ID, m.Slug),
		Summary:   m.Summary,
		Rating:    m.RatingAvg,
		Ratings:   m.RatingCount,
		Likes:     m.LikesCount,
		Downloads: m.DownloadCount,
		Makes:     m.MakesCount,
		Published: m.DatePublished,
		Paid:      m.Premium || (m.Price != nil && *m.Price != "" && *m.Price != "0"),
	}

	if m.User != nil {
		r.Author = m.User.PublicUsername
	}

	if m.Image != nil {
		r.ThumbnailURL = m.Image.URL()
	}

	return r
}

func (c *Client) searchTool(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	query := strings.TrimSpace(in.Query)
	if query == "" {
		return nil, SearchOutput{}, errors.New("query is empty")
	}

	order := SearchOrder(in.Sort)
	if order == "" {
		order = SearchOrders[0]
	}

	if !slices.Contains(SearchOrders, order) {
		return nil, SearchOutput{}, fmt.Errorf("sort must be one of %v, not %q", SearchOrders, in.Sort)
	}

	limit := in.Limit
	if limit == 0 {
		limit = defaultLimit
	}

	if limit < 1 || limit > maxLimit {
		return nil, SearchOutput{}, fmt.Errorf("limit must be between 1 and %d, not %d", maxLimit, in.Limit)
	}

	if in.Offset < 0 {
		return nil, SearchOutput{}, fmt.Errorf("offset must not be negative, not %d", in.Offset)
	}

	res, err := c.Search(ctx, query, order, limit, in.Offset)
	if err != nil {
		return nil, SearchOutput{}, err
	}

	out := SearchOutput{Total: res.TotalCount, Offset: in.Offset, Models: make([]ModelResult, 0, len(res.Items))}
	for _, m := range res.Items {
		out.Models = append(out.Models, toResult(m))
	}

	return nil, out, nil
}

// ModelInput names one model.
type ModelInput struct {
	Model string `json:"model" jsonschema:"the model's ID (e.g. 770347) or its printables.com URL"`
}

// ModelOutput is get_model's output.
type ModelOutput struct {
	ModelResult

	Description string       `json:"description" jsonschema:"the author's description, as Markdown"`
	Tags        []string     `json:"tags,omitempty"`
	License     string       `json:"license,omitempty"`
	Category    string       `json:"category,omitempty"`
	NSFW        bool         `json:"nsfw,omitempty"`
	ImageURLs   []string     `json:"image_urls,omitempty" jsonschema:"up to 5 pictures of the model"`
	Files       []FileResult `json:"files"`
	Packs       []PackResult `json:"packs,omitempty" jsonschema:"zips of all of one kind of the model's files"`
}

// FileResult is one downloadable file.
type FileResult struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind" jsonschema:"model (STL, 3MF, STEP and similar), gcode, sla or other"`
	SizeBytes int64  `json:"size_bytes"`
	Folder    string `json:"folder,omitempty"`
	Note      string `json:"note,omitempty"`
	// Print settings, for G-code and SLA files.
	Printer       string   `json:"printer,omitempty"`
	Material      string   `json:"material,omitempty"`
	PrintMinutes  *float64 `json:"print_minutes,omitempty"`
	LayerHeightMM *float64 `json:"layer_height_mm,omitempty"`
	NozzleMM      *float64 `json:"nozzle_mm,omitempty"`
	WeightGrams   *float64 `json:"weight_grams,omitempty"`
}

// PackResult is one zip pack.
type PackResult struct {
	ID        string `json:"id"`
	Contents  string `json:"contents" jsonschema:"which files the zip holds, e.g. model_files"`
	SizeBytes int64  `json:"size_bytes"`
}

var modelIDPattern = regexp.MustCompile(`^(?:https?://(?:www\.)?printables\.com/(?:[a-z]{2}/)?model/)?(\d+)(?:[-/?#].*)?$`)

// parseModelID accepts a model ID or a model page URL.
func parseModelID(s string) (string, error) {
	m := modelIDPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", fmt.Errorf("%q is neither a Printables model ID nor a printables.com/model/... URL", s)
	}

	return m[1], nil
}

func (c *Client) modelTool(ctx context.Context, _ *mcp.CallToolRequest, in ModelInput) (*mcp.CallToolResult, ModelOutput, error) {
	id, err := parseModelID(in.Model)
	if err != nil {
		return nil, ModelOutput{}, err
	}

	m, err := c.Model(ctx, id)
	if err != nil {
		return nil, ModelOutput{}, err
	}

	out := ModelOutput{ModelResult: toResult(m.ModelSummary), NSFW: m.NSFW, Files: []FileResult{}}

	out.Description, err = descriptionMarkdown(m.Description)
	if err != nil {
		return nil, ModelOutput{}, err
	}

	for _, t := range m.Tags {
		out.Tags = append(out.Tags, t.Name)
	}

	if m.License != nil {
		out.License = m.License.Abbreviation
		if out.License == "" {
			out.License = m.License.Name
		}
	}

	if m.Category != nil {
		var parts []string
		for _, p := range m.Category.Path {
			parts = append(parts, p.Name)
		}

		if len(parts) == 0 {
			parts = []string{m.Category.Name}
		}

		out.Category = strings.Join(parts, " > ")
	}

	for _, img := range m.Images {
		if len(out.ImageURLs) == maxImages {
			break
		}

		if u := img.URL(); u != "" {
			out.ImageURLs = append(out.ImageURLs, u)
		}
	}

	for _, group := range fileGroups(m) {
		for _, f := range group.files {
			out.Files = append(out.Files, toFileResult(f, group.kind))
		}
	}

	for _, p := range m.DownloadPacks {
		out.Packs = append(out.Packs, PackResult{ID: p.ID, Contents: strings.ToLower(p.FileType), SizeBytes: p.FileSize})
	}

	return nil, out, nil
}

// descriptionMarkdown turns the description's HTML into Markdown, cut to a
// size the conversation can afford.
func descriptionMarkdown(html string) (string, error) {
	md, err := htmltomarkdown.ConvertString(html)
	if err != nil {
		return "", fmt.Errorf("convert the description to Markdown: %w", err)
	}

	// Authors' editors leave non-breaking spaces at line ends.
	lines := strings.Split(strings.ReplaceAll(md, " ", " "), "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, " \t")
	}

	md = strings.TrimSpace(strings.Join(lines, "\n"))
	if r := []rune(md); len(r) > maxDescription {
		md = string(r[:maxDescription]) + "\n\n[description cut at 20000 characters]"
	}

	return md, nil
}

type fileGroup struct {
	kind  FileKind
	files []File
}

func fileGroups(m *Model) []fileGroup {
	return []fileGroup{{KindSTL, m.STLs}, {KindGCode, m.GCodes}, {KindSLA, m.SLAs}, {KindOther, m.OtherFiles}}
}

// kindName is the name a file kind has in tool output. The API calls every
// model file (3MF and STEP included) "stl".
func kindName(k FileKind) string {
	if k == KindSTL {
		return "model"
	}

	return string(k)
}

func toFileResult(f File, kind FileKind) FileResult {
	r := FileResult{
		ID:            f.ID,
		Name:          f.Name,
		Kind:          kindName(kind),
		SizeBytes:     f.FileSize,
		Folder:        f.Folder,
		Note:          f.Note,
		LayerHeightMM: f.LayerHeight,
		NozzleMM:      f.NozzleDiameter,
		WeightGrams:   f.Weight,
	}

	if f.Printer != nil {
		r.Printer = f.Printer.Name
	}

	if f.Material != nil {
		r.Material = f.Material.Name
	}

	if f.PrintDuration != nil {
		// The API reports seconds.
		minutes := *f.PrintDuration / 60
		r.PrintMinutes = &minutes
	}

	return r
}

// LinksInput is get_download_links' input.
type LinksInput struct {
	Model   string   `json:"model" jsonschema:"the model's ID or its printables.com URL"`
	FileIDs []string `json:"file_ids,omitempty" jsonschema:"IDs from get_model's files"`
	PackIDs []string `json:"pack_ids,omitempty" jsonschema:"IDs from get_model's packs"`
}

// LinksOutput is get_download_links' output.
type LinksOutput struct {
	Links []Link `json:"links"`
}

// Link is a direct download link for one file.
type Link struct {
	ID        string `json:"id"`
	Name      string `json:"name" jsonschema:"a file name to save it as"`
	SizeBytes int64  `json:"size_bytes"`
	URL       string `json:"url"`
	ExpiresAt string `json:"expires_at" jsonschema:"when the URL stops working (RFC 3339)"`
}

type wanted struct {
	id, name string
	kind     FileKind
	size     int64
}

func (c *Client) linksTool(ctx context.Context, _ *mcp.CallToolRequest, in LinksInput) (*mcp.CallToolResult, LinksOutput, error) {
	id, err := parseModelID(in.Model)
	if err != nil {
		return nil, LinksOutput{}, err
	}

	if len(in.FileIDs)+len(in.PackIDs) == 0 {
		return nil, LinksOutput{}, errors.New("give at least one ID in file_ids or pack_ids (see get_model)")
	}

	if n := len(in.FileIDs) + len(in.PackIDs); n > maxLinksPerCall {
		return nil, LinksOutput{}, fmt.Errorf("at most %d links per call, not %d; use a pack for a whole model", maxLinksPerCall, n)
	}

	// The file list says which kind each ID is, which the link request needs,
	// and confirms the IDs belong to this model.
	m, err := c.Model(ctx, id)
	if err != nil {
		return nil, LinksOutput{}, err
	}

	want, err := resolve(m, in.FileIDs, in.PackIDs)
	if err != nil {
		return nil, LinksOutput{}, err
	}

	out := LinksOutput{Links: make([]Link, 0, len(want))}

	for _, w := range want {
		link, err := c.DownloadLink(ctx, m.ID, w.id, w.kind)
		if err != nil {
			return nil, LinksOutput{}, fmt.Errorf("%s (%s): %w", w.name, w.id, err)
		}

		out.Links = append(out.Links, Link{
			ID: w.id, Name: w.name, SizeBytes: w.size, URL: link.URL, ExpiresAt: link.ExpiresAt.Format(time.RFC3339),
		})
	}

	return nil, out, nil
}

func resolve(m *Model, fileIDs, packIDs []string) ([]wanted, error) {
	files := map[string]wanted{}

	for _, group := range fileGroups(m) {
		for _, f := range group.files {
			files[f.ID] = wanted{id: f.ID, name: f.Name, kind: group.kind, size: f.FileSize}
		}
	}

	packs := map[string]wanted{}

	for _, p := range m.DownloadPacks {
		name := fmt.Sprintf("%s-%s.zip", m.Slug, strings.ToLower(p.FileType))
		packs[p.ID] = wanted{id: p.ID, name: name, kind: KindPack, size: p.FileSize}
	}

	var want []wanted

	for _, id := range fileIDs {
		w, ok := files[id]
		if !ok {
			return nil, fmt.Errorf("model %s has no file with ID %s (see get_model's files)", m.ID, id)
		}

		want = append(want, w)
	}

	for _, id := range packIDs {
		w, ok := packs[id]
		if !ok {
			return nil, fmt.Errorf("model %s has no pack with ID %s (see get_model's packs)", m.ID, id)
		}

		want = append(want, w)
	}

	return want, nil
}
