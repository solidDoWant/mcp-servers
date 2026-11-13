package sendcutsend

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/url"
	"path"
	"slices"
	"sort"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Instructions is sent to clients with the tool list.
const Instructions = `Upload flat parts to SendCutSend (laser, waterjet and router cutting of sheet stock), price them, and build saved carts the account owner checks out in their browser.
Typical flow: create_part (DXF text inline, or a file uploaded with get_upload_url) → quote_part without a material to compare materials → quote_part with a material_sku for delivery dates → create_saved_cart → give the user its checkout_url.
list_materials gives SKUs, thicknesses and the services each material offers. Prices here are for cutting only; services such as bending, tapping and finishing are not supported by this server and must be added on the website.
Nothing here places an order or pays: the user reviews and checks out the saved cart on sendcutsend.com.`

const (
	maxQuantity      = 10000
	maxInlineBytes   = 1 << 20
	maxFilename      = 200
	maxTitle         = 100
	maxCartItems     = 25
	defaultListLimit = 20
	maxListLimit     = 100
	defaultQuotes    = 15
	maxQuotes        = 100
	defaultMaterials = 50
	maxMaterials     = 250
)

// uploadExtensions are the file types the site's instant quoting accepts.
var uploadExtensions = []string{".dxf", ".dwg", ".ai", ".eps", ".stp", ".step"}

// Tools registers the server's tools on s.
func Tools(c *Client) func(*mcp.Server) {
	return func(s *mcp.Server) {
		mcp.AddTool(s, &mcp.Tool{Name: "list_materials", Description: materialsDescription, Annotations: readOnly("List SendCutSend materials")}, c.materialsTool)
		mcp.AddTool(s, &mcp.Tool{Name: "get_upload_url", Description: uploadDescription, Annotations: write("Get a SendCutSend upload URL", false)}, c.uploadTool)
		mcp.AddTool(s, &mcp.Tool{Name: "create_part", Description: createPartDescription, Annotations: write("Create a SendCutSend part", false)}, c.createPartTool)
		mcp.AddTool(s, &mcp.Tool{Name: "get_part", Description: getPartDescription, Annotations: readOnly("Get a SendCutSend part")}, c.getPartTool)
		mcp.AddTool(s, &mcp.Tool{Name: "list_parts", Description: listPartsDescription, Annotations: readOnly("List SendCutSend parts")}, c.listPartsTool)
		mcp.AddTool(s, &mcp.Tool{Name: "delete_part", Description: deletePartDescription, Annotations: write("Delete a SendCutSend part", true)}, c.deletePartTool)
		mcp.AddTool(s, &mcp.Tool{Name: "quote_part", Description: quoteDescription, Annotations: readOnly("Price a SendCutSend part")}, c.quoteTool)
		mcp.AddTool(s, &mcp.Tool{Name: "create_saved_cart", Description: createCartDescription, Annotations: write("Create a SendCutSend saved cart", false)}, c.createCartTool)
		mcp.AddTool(s, &mcp.Tool{Name: "list_saved_carts", Description: listCartsDescription, Annotations: readOnly("List SendCutSend saved carts")}, c.listCartsTool)
		mcp.AddTool(s, &mcp.Tool{Name: "get_saved_cart", Description: getCartDescription, Annotations: readOnly("Get a SendCutSend saved cart")}, c.getCartTool)
		mcp.AddTool(s, &mcp.Tool{Name: "delete_saved_cart", Description: deleteCartDescription, Annotations: write("Delete a SendCutSend saved cart", true)}, c.deleteCartTool)
	}
}

const (
	materialsDescription  = "List the materials SendCutSend cuts, with the SKUs quote_part and create_saved_cart take. Filter by category (e.g. Aluminum, Steel, Acrylic), form, service or text. Thickness is in inches."
	uploadDescription     = "Get a pre-signed URL to upload a part file (DXF, DWG, AI, EPS or STEP) to, for files you can't pass to create_part inline. Upload with an HTTP PUT of the raw file, e.g. curl -fsS -T part.step '<upload_url>', within 15 minutes, then call create_part with the same upload_url. Uploaded files are publicly readable by anyone who has the URL."
	createPartDescription = "Add a part to the SendCutSend account from a file and wait for the site to process it. Pass either dxf_text (the content of a DXF file, up to 1 MiB) or upload_url (from get_upload_url, after uploading). Returns the part's ID for quote_part and create_saved_cart, its measured size, and any problems that stop it being priced. If processing takes longer than the wait, the part comes back with processed=false: call get_part later."
	getPartDescription    = "Get one part's details and processing result: size, units, whether it can be priced and the problems found."
	listPartsDescription  = "List the parts on the SendCutSend account, newest first."
	deletePartDescription = "Delete a part from the SendCutSend account. Saved carts that already hold it are not changed."
	quoteDescription      = "Price a part for a quantity. Without material_sku, price it in every material that can cut it and list the cheapest first (optionally only one category). With material_sku, price that material only, with price breaks and estimated ship and delivery dates. Prices are in USD for cutting only, before shipping and tax."
	createCartDescription = "Create a saved cart on the SendCutSend account holding cut-only parts, and return a checkout_url. Give the user the checkout_url: opening it while signed in loads the parts into their cart to review and pay. Nothing is ordered or paid here. The account's own cart must be empty, because SendCutSend builds a saved cart from everything in it; this tool adds its parts, saves, and takes them out again. A saved cart can't be changed afterwards; create another and delete the old one instead."
	listCartsDescription  = "List the account's saved carts, newest first, with their checkout URLs."
	getCartDescription    = "Get one saved cart's parts, materials and quantities. Saved carts don't keep prices; use quote_part for current prices."
	deleteCartDescription = "Delete a saved cart from the SendCutSend account."
)

func readOnly(title string) *mcp.ToolAnnotations {
	openWorld := true

	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &openWorld}
}

func write(title string, destructive bool) *mcp.ToolAnnotations {
	openWorld := true

	return &mcp.ToolAnnotations{Title: title, DestructiveHint: &destructive, OpenWorldHint: &openWorld}
}

// money rounds a price to cents for output.
func money(d Decimal) float64 {
	return math.Round(float64(d)*100) / 100
}

func deref(s *string) string {
	if s == nil {
		return ""
	}

	return *s
}

// --- list_materials

// MaterialsInput is list_materials' input.
type MaterialsInput struct {
	Category    string `json:"category,omitempty" jsonschema:"only this category, e.g. Aluminum, Stainless Steel, Acrylic (case-insensitive)"`
	Form        string `json:"form,omitempty" jsonschema:"only this form: sheet (default) or tube; all for both"`
	Service     string `json:"service,omitempty" jsonschema:"only materials offering this service, e.g. bending, tapping, powder"`
	Query       string `json:"query,omitempty" jsonschema:"only materials whose name or SKU contains this text"`
	InStockOnly bool   `json:"in_stock_only,omitempty" jsonschema:"leave out materials that are out of stock"`
	Limit       int    `json:"limit,omitempty" jsonschema:"at most this many, 1 to 250 (default 50)"`
}

// MaterialsOutput is list_materials' output.
type MaterialsOutput struct {
	Total      int              `json:"total" jsonschema:"how many materials match before the limit"`
	Categories []string         `json:"categories" jsonschema:"every category, for the category filter"`
	Materials  []MaterialResult `json:"materials"`
}

// MaterialResult is one material.
type MaterialResult struct {
	SKU         string   `json:"sku"`
	Name        string   `json:"name"`
	Category    string   `json:"category"`
	Form        string   `json:"form"`
	ThicknessIn string   `json:"thickness_in" jsonschema:"thickness in inches"`
	OutOfStock  bool     `json:"out_of_stock,omitempty"`
	Services    []string `json:"services,omitempty"`
}

func (c *Client) materialsTool(ctx context.Context, _ *mcp.CallToolRequest, in MaterialsInput) (*mcp.CallToolResult, MaterialsOutput, error) {
	limit, err := checkLimit(in.Limit, defaultMaterials, maxMaterials)
	if err != nil {
		return nil, MaterialsOutput{}, err
	}

	form := strings.ToLower(strings.TrimSpace(in.Form))
	if form == "" {
		form = "sheet"
	}

	if !slices.Contains([]string{"sheet", "tube", "all"}, form) {
		return nil, MaterialsOutput{}, fmt.Errorf("form must be sheet, tube or all, not %q", in.Form)
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, MaterialsOutput{}, err
	}

	all, err := s.materials(ctx)
	if err != nil {
		return nil, MaterialsOutput{}, err
	}

	cats := map[string]bool{}
	out := MaterialsOutput{Materials: []MaterialResult{}}
	query := strings.ToLower(strings.TrimSpace(in.Query))

	for _, m := range all {
		cats[m.Category] = true

		switch {
		case form != "all" && !strings.EqualFold(m.Form, form),
			in.Category != "" && !strings.EqualFold(m.Category, strings.TrimSpace(in.Category)),
			in.Service != "" && !slices.Contains(m.Services(), strings.ToLower(strings.TrimSpace(in.Service))),
			query != "" && !strings.Contains(strings.ToLower(m.FullName+" "+m.SKU), query),
			in.InStockOnly && m.OutOfStock:
			continue
		}

		out.Total++

		if len(out.Materials) < limit {
			out.Materials = append(out.Materials, toMaterial(m))
		}
	}

	for k := range cats {
		if k != "" {
			out.Categories = append(out.Categories, k)
		}
	}

	sort.Strings(out.Categories)

	return nil, out, nil
}

func toMaterial(m Material) MaterialResult {
	name := m.FullName
	if name == "" {
		name = m.Name
	}

	return MaterialResult{
		SKU: m.SKU, Name: name, Category: m.Category, Form: m.Form,
		ThicknessIn: m.Thickness, OutOfStock: m.OutOfStock, Services: m.Services(),
	}
}

func checkLimit(v, def, maxV int) (int, error) {
	if v == 0 {
		return def, nil
	}

	if v < 1 || v > maxV {
		return 0, fmt.Errorf("limit must be between 1 and %d, not %d", maxV, v)
	}

	return v, nil
}

// --- get_upload_url

// UploadInput is get_upload_url's input.
type UploadInput struct {
	Filename string `json:"filename" jsonschema:"the file's name with its extension, e.g. bracket.step"`
}

// UploadOutput is get_upload_url's output.
type UploadOutput struct {
	UploadURL string `json:"upload_url" jsonschema:"PUT the file here, then pass this URL to create_part"`
	ExpiresIn string `json:"expires_in"`
	Example   string `json:"example" jsonschema:"a command that uploads the file"`
}

func checkFilename(name string, allowed []string) (string, error) {
	name = strings.TrimSpace(name)

	switch {
	case name == "":
		return "", errors.New("filename is empty")
	case len(name) > maxFilename:
		return "", fmt.Errorf("filename is longer than %d characters", maxFilename)
	case strings.ContainsAny(name, `/\`) || strings.ContainsFunc(name, func(r rune) bool { return r < 0x20 }):
		return "", fmt.Errorf("filename %q must be a plain file name, without a path", name)
	}

	if !slices.Contains(allowed, strings.ToLower(path.Ext(name))) {
		return "", fmt.Errorf("filename %q must end in one of %s", name, strings.Join(allowed, ", "))
	}

	return name, nil
}

func (c *Client) uploadTool(ctx context.Context, _ *mcp.CallToolRequest, in UploadInput) (*mcp.CallToolResult, UploadOutput, error) {
	name, err := checkFilename(in.Filename, uploadExtensions)
	if err != nil {
		return nil, UploadOutput{}, err
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, UploadOutput{}, err
	}

	u, err := s.uploadURL(ctx, name)
	if err != nil {
		return nil, UploadOutput{}, err
	}

	return nil, UploadOutput{
		UploadURL: u,
		ExpiresIn: "15 minutes",
		Example:   fmt.Sprintf("curl -fsS -T %s '%s'", shellQuote(name), u),
	}, nil
}

// shellQuote quotes a file name for the example command. Names are already
// free of control characters and slashes.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// --- create_part / get_part / list_parts / delete_part

// CreatePartInput is create_part's input.
type CreatePartInput struct {
	Filename  string `json:"filename" jsonschema:"the part's file name, shown on the website; with dxf_text it must end in .dxf"`
	DXFText   string `json:"dxf_text,omitempty" jsonschema:"the whole content of a DXF file"`
	UploadURL string `json:"upload_url,omitempty" jsonschema:"the upload_url from get_upload_url, after the file was PUT to it"`
	Units     string `json:"units,omitempty" jsonschema:"the drawing's units: in or mm; leave empty to let SendCutSend detect them"`
}

// PartOutput is a part's details.
type PartOutput struct {
	ID              int64       `json:"id" jsonschema:"the part ID quote_part and create_saved_cart take"`
	Name            string      `json:"name"`
	Processed       bool        `json:"processed" jsonschema:"false while SendCutSend is still processing the file; call get_part again later"`
	State           string      `json:"state"`
	Quotable        bool        `json:"quotable" jsonschema:"whether the part can be priced: processed and without errors"`
	Units           string      `json:"units,omitempty"`
	Width           float64     `json:"width" jsonschema:"in the part's units"`
	Height          float64     `json:"height" jsonschema:"in the part's units"`
	Area            float64     `json:"area" jsonschema:"in the part's units, squared"`
	Contours        int         `json:"contours" jsonschema:"closed outlines found, the outer edge included"`
	Format          string      `json:"format,omitempty" jsonschema:"what SendCutSend thinks it is: sheet, tube and so on"`
	PossibleFormats []string    `json:"possible_formats,omitempty"`
	Errors          []IssueItem `json:"errors,omitempty" jsonschema:"problems that stop the part being priced"`
	Warnings        []IssueItem `json:"warnings,omitempty"`
	PreviewURL      string      `json:"preview_url,omitempty" jsonschema:"an image of the part as SendCutSend sees it"`
	Created         string      `json:"created,omitempty"`
}

// IssueItem is one problem.
type IssueItem struct {
	Code    string `json:"code"`
	Message string `json:"message,omitempty"`
}

func toIssues(in []Issue) []IssueItem {
	var out []IssueItem
	for _, i := range in {
		out = append(out, IssueItem(i))
	}

	return out
}

func toPart(d *Drawing) PartOutput {
	p := PartOutput{
		ID:              d.ID,
		Name:            d.Name(),
		Processed:       d.Processed(),
		State:           d.State,
		Units:           d.Units,
		Width:           float64(d.Width),
		Height:          float64(d.Height),
		Area:            float64(d.Area),
		Contours:        d.Contours,
		Format:          deref(d.LikelyFormat),
		PossibleFormats: d.PossibleFormats,
		PreviewURL:      d.PreviewURL,
		Created:         d.Created,
	}

	// While processing, the site reports stale errors from the empty record.
	if p.Processed {
		p.Errors = toIssues(d.Errors)
		p.Warnings = toIssues(d.Warnings)
		p.Quotable = d.State == "complete" && len(d.Errors) == 0
	}

	return p
}

func (c *Client) createPartTool(ctx context.Context, _ *mcp.CallToolRequest, in CreatePartInput) (*mcp.CallToolResult, PartOutput, error) {
	units := strings.ToLower(strings.TrimSpace(in.Units))
	if units != "" && units != "in" && units != "mm" {
		return nil, PartOutput{}, fmt.Errorf("units must be in or mm, or empty to detect them, not %q", in.Units)
	}

	hasText, hasURL := in.DXFText != "", strings.TrimSpace(in.UploadURL) != ""
	if hasText == hasURL {
		return nil, PartOutput{}, errors.New("give exactly one of dxf_text or upload_url")
	}

	var (
		name   string
		upload *url.URL
		err    error
	)

	if hasText {
		name, err = checkFilename(in.Filename, []string{".dxf"})
		if err != nil {
			return nil, PartOutput{}, err
		}

		if len(in.DXFText) > maxInlineBytes {
			return nil, PartOutput{}, fmt.Errorf("dxf_text is %d bytes; the limit is %d. Use get_upload_url for large files", len(in.DXFText), maxInlineBytes)
		}
	} else {
		name, err = checkFilename(in.Filename, uploadExtensions)
		if err != nil {
			return nil, PartOutput{}, err
		}

		upload, err = url.Parse(strings.TrimSpace(in.UploadURL))
		if err != nil || upload.Scheme != "https" || !c.uploadHostAllowed(upload) {
			return nil, PartOutput{}, errors.New("upload_url must be a URL that get_upload_url returned")
		}
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, PartOutput{}, err
	}

	target := ""

	if hasText {
		target, err = s.uploadURL(ctx, name)
		if err != nil {
			return nil, PartOutput{}, err
		}

		if err := s.put(ctx, target, []byte(in.DXFText)); err != nil {
			return nil, PartOutput{}, err
		}
	} else {
		ok, err := s.uploaded(ctx, upload)
		if err != nil {
			return nil, PartOutput{}, err
		}

		if !ok {
			return nil, PartOutput{}, errors.New("nothing has been uploaded to upload_url yet; PUT the file to it first (see get_upload_url)")
		}

		target = upload.String()
	}

	d, err := s.createDrawing(ctx, target, name, units)
	if err != nil {
		return nil, PartOutput{}, err
	}

	d, err = s.waitProcessed(ctx, d)
	if err != nil {
		return nil, PartOutput{}, err
	}

	return nil, toPart(d), nil
}

// PartInput names one part.
type PartInput struct {
	PartID int64 `json:"part_id" jsonschema:"the part's ID from create_part or list_parts"`
}

func checkID(id int64, what string) error {
	if id <= 0 {
		return fmt.Errorf("%s must be a positive ID, not %d", what, id)
	}

	return nil
}

func (c *Client) getPartTool(ctx context.Context, _ *mcp.CallToolRequest, in PartInput) (*mcp.CallToolResult, PartOutput, error) {
	if err := checkID(in.PartID, "part_id"); err != nil {
		return nil, PartOutput{}, err
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, PartOutput{}, err
	}

	d, err := s.drawing(ctx, in.PartID)
	if err != nil {
		return nil, PartOutput{}, err
	}

	return nil, toPart(d), nil
}

// ListInput pages through a list.
type ListInput struct {
	Limit int `json:"limit,omitempty" jsonschema:"results per page, 1 to 100 (default 20)"`
	Page  int `json:"page,omitempty" jsonschema:"page number, from 1 (default 1)"`
}

func (in ListInput) check() (limit, page int, err error) {
	limit, err = checkLimit(in.Limit, defaultListLimit, maxListLimit)
	if err != nil {
		return 0, 0, err
	}

	page = in.Page
	if page == 0 {
		page = 1
	}

	if page < 1 {
		return 0, 0, fmt.Errorf("page must be 1 or more, not %d", in.Page)
	}

	return limit, page, nil
}

// ListPartsOutput is list_parts' output.
type ListPartsOutput struct {
	Total int          `json:"total" jsonschema:"parts on the account"`
	Page  int          `json:"page"`
	Parts []PartOutput `json:"parts"`
}

func (c *Client) listPartsTool(ctx context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, ListPartsOutput, error) {
	limit, page, err := in.check()
	if err != nil {
		return nil, ListPartsOutput{}, err
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, ListPartsOutput{}, err
	}

	ds, total, err := s.drawings(ctx, page, limit)
	if err != nil {
		return nil, ListPartsOutput{}, err
	}

	out := ListPartsOutput{Total: total, Page: page, Parts: make([]PartOutput, 0, len(ds))}
	for i := range ds {
		out.Parts = append(out.Parts, toPart(&ds[i]))
	}

	return nil, out, nil
}

// DeletedOutput confirms a deletion.
type DeletedOutput struct {
	Deleted int64 `json:"deleted"`
}

func (c *Client) deletePartTool(ctx context.Context, _ *mcp.CallToolRequest, in PartInput) (*mcp.CallToolResult, DeletedOutput, error) {
	if err := checkID(in.PartID, "part_id"); err != nil {
		return nil, DeletedOutput{}, err
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, DeletedOutput{}, err
	}

	if err := s.deleteDrawing(ctx, in.PartID); err != nil {
		return nil, DeletedOutput{}, err
	}

	return nil, DeletedOutput{Deleted: in.PartID}, nil
}

// --- quote_part

// QuoteInput is quote_part's input.
type QuoteInput struct {
	PartID      int64  `json:"part_id" jsonschema:"the part's ID"`
	Quantity    int    `json:"quantity" jsonschema:"how many to make, 1 to 10000"`
	MaterialSKU string `json:"material_sku,omitempty" jsonschema:"price only this material (a SKU from list_materials), with delivery dates"`
	Category    string `json:"category,omitempty" jsonschema:"without material_sku: only materials in this category"`
	Limit       int    `json:"limit,omitempty" jsonschema:"without material_sku: at most this many materials, 1 to 100 (default 15)"`
}

// QuoteOutput is quote_part's output.
type QuoteOutput struct {
	PartID      int64              `json:"part_id"`
	Quantity    int                `json:"quantity"`
	Quotes      []QuoteResult      `json:"quotes" jsonschema:"cheapest first"`
	Quotable    int                `json:"quotable" jsonschema:"materials that can make the part (before the category filter and limit)"`
	Unavailable []UnavailableCount `json:"unavailable,omitempty" jsonschema:"why the other materials can't, by reason"`
}

// QuoteResult is the price in one material.
type QuoteResult struct {
	MaterialSKU    string        `json:"material_sku"`
	Material       string        `json:"material"`
	Category       string        `json:"category"`
	ThicknessIn    string        `json:"thickness_in"`
	UnitPrice      float64       `json:"unit_price_usd"`
	TotalPrice     float64       `json:"total_price_usd"`
	DiscountPct    float64       `json:"discount_pct" jsonschema:"quantity discount already in the price"`
	OutOfStock     bool          `json:"out_of_stock,omitempty"`
	ReviewRequired bool          `json:"review_required,omitempty" jsonschema:"SendCutSend staff review the part before cutting"`
	Errors         []IssueItem   `json:"errors,omitempty" jsonschema:"why this material can't make the part"`
	Warnings       []IssueItem   `json:"warnings,omitempty"`
	PriceBreaks    []PriceBreakR `json:"price_breaks,omitempty" jsonschema:"unit price from each quantity up"`
	EstShipDate    string        `json:"est_ship_date,omitempty"`
	InHandDate     string        `json:"in_hand_date,omitempty" jsonschema:"estimated delivery with standard shipping"`
}

// PriceBreakR is a quantity price break.
type PriceBreakR struct {
	Quantity    int     `json:"quantity"`
	UnitPrice   float64 `json:"unit_price_usd"`
	DiscountPct float64 `json:"discount_pct"`
}

// UnavailableCount counts materials that can't make the part for one reason.
type UnavailableCount struct {
	Reason    string `json:"reason"`
	Materials int    `json:"materials"`
}

func (c *Client) quoteTool(ctx context.Context, _ *mcp.CallToolRequest, in QuoteInput) (*mcp.CallToolResult, QuoteOutput, error) {
	if err := checkID(in.PartID, "part_id"); err != nil {
		return nil, QuoteOutput{}, err
	}

	if in.Quantity < 1 || in.Quantity > maxQuantity {
		return nil, QuoteOutput{}, fmt.Errorf("quantity must be between 1 and %d, not %d", maxQuantity, in.Quantity)
	}

	limit, err := checkLimit(in.Limit, defaultQuotes, maxQuotes)
	if err != nil {
		return nil, QuoteOutput{}, err
	}

	sku := strings.ToUpper(strings.TrimSpace(in.MaterialSKU))

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, QuoteOutput{}, err
	}

	if err := s.requireQuotable(ctx, in.PartID); err != nil {
		return nil, QuoteOutput{}, err
	}

	mats, err := s.materials(ctx)
	if err != nil {
		return nil, QuoteOutput{}, err
	}

	bySKU := make(map[string]Material, len(mats))
	for _, m := range mats {
		bySKU[m.SKU] = m
	}

	if sku != "" {
		if _, ok := bySKU[sku]; !ok {
			return nil, QuoteOutput{}, fmt.Errorf("unknown material_sku %q; see list_materials", in.MaterialSKU)
		}
	}

	quotes, dates, err := s.quotes(ctx, in.PartID, in.Quantity, sku)
	if err != nil {
		return nil, QuoteOutput{}, err
	}

	out := QuoteOutput{PartID: in.PartID, Quantity: in.Quantity, Quotes: []QuoteResult{}}

	if sku != "" {
		if len(quotes) == 0 {
			return nil, QuoteOutput{}, fmt.Errorf("SendCutSend returned no price for %s", sku)
		}

		q := toQuote(quotes[0], bySKU)
		q.EstShipDate, q.InHandDate = deref(dates.EstShipDate), deref(dates.InHandDate)

		for _, b := range quotes[0].Breaks {
			q.PriceBreaks = append(q.PriceBreaks, PriceBreakR{Quantity: b.Quantity, UnitPrice: money(b.UnitPrice), DiscountPct: b.Discount})
		}

		if len(q.Errors) == 0 {
			out.Quotable = 1
		}

		out.Quotes = append(out.Quotes, q)

		return nil, out, nil
	}

	reasons := map[string]int{}

	var ok []QuoteResult

	for _, raw := range quotes {
		if len(raw.Errors) > 0 {
			reasons[raw.Errors[0].Code]++

			continue
		}

		out.Quotable++

		q := toQuote(raw, bySKU)
		if in.Category != "" && !strings.EqualFold(q.Category, strings.TrimSpace(in.Category)) {
			continue
		}

		ok = append(ok, q)
	}

	sort.SliceStable(ok, func(i, j int) bool { return ok[i].TotalPrice < ok[j].TotalPrice })

	if len(ok) > limit {
		ok = ok[:limit]
	}

	out.Quotes = append(out.Quotes, ok...)

	for r, n := range reasons {
		out.Unavailable = append(out.Unavailable, UnavailableCount{Reason: r, Materials: n})
	}

	sort.Slice(out.Unavailable, func(i, j int) bool {
		if out.Unavailable[i].Materials != out.Unavailable[j].Materials {
			return out.Unavailable[i].Materials > out.Unavailable[j].Materials
		}

		return out.Unavailable[i].Reason < out.Unavailable[j].Reason
	})

	return nil, out, nil
}

// requireQuotable fails with a clear message when a part can't be priced
// yet, instead of letting every material report an error.
func (s *session) requireQuotable(ctx context.Context, id int64) error {
	d, err := s.drawing(ctx, id)
	if err != nil {
		return err
	}

	p := toPart(d)

	switch {
	case !p.Processed:
		return fmt.Errorf("part %d is still processing (state %s); try again shortly", id, d.State)
	case !p.Quotable:
		var codes []string
		for _, e := range p.Errors {
			codes = append(codes, strings.TrimSpace(e.Code+": "+e.Message))
		}

		return fmt.Errorf("part %d can't be priced: %s", id, strings.Join(codes, "; "))
	}

	return nil
}

func toQuote(q Quote, bySKU map[string]Material) QuoteResult {
	m := bySKU[q.MaterialSKU]

	name := m.FullName
	if name == "" {
		name = m.Name
	}

	return QuoteResult{
		MaterialSKU:    q.MaterialSKU,
		Material:       name,
		Category:       m.Category,
		ThicknessIn:    m.Thickness,
		UnitPrice:      money(q.UnitCost),
		TotalPrice:     money(q.TotalCost),
		DiscountPct:    q.Discount,
		OutOfStock:     m.OutOfStock,
		ReviewRequired: q.ReviewRequired,
		Errors:         toIssues(q.Errors),
		Warnings:       toIssues(q.Warnings),
	}
}

// --- saved carts

// CartItemInput is one line of a new saved cart.
type CartItemInput struct {
	PartID      int64  `json:"part_id"`
	MaterialSKU string `json:"material_sku" jsonschema:"a SKU from list_materials or quote_part"`
	Quantity    int    `json:"quantity" jsonschema:"1 to 10000"`
}

// CreateCartInput is create_saved_cart's input.
type CreateCartInput struct {
	Title string          `json:"title" jsonschema:"a name for the cart, shown on the website"`
	Items []CartItemInput `json:"items" jsonschema:"1 to 25 lines"`
}

// CartOutput is a saved cart.
type CartOutput struct {
	ID          int64            `json:"id" jsonschema:"the saved cart's ID"`
	Title       string           `json:"title"`
	Reference   string           `json:"reference,omitempty" jsonschema:"SendCutSend's reference number"`
	CheckoutURL string           `json:"checkout_url,omitempty" jsonschema:"open while signed in to load the cart for checkout"`
	Created     string           `json:"created,omitempty"`
	ItemCount   int              `json:"item_count"`
	Items       []CartItemResult `json:"items,omitempty"`
	Subtotal    *float64         `json:"subtotal_usd,omitempty" jsonschema:"parts only, when the cart was made"`
	Shipping    *float64         `json:"shipping_estimate_usd,omitempty" jsonschema:"standard shipping to the account's address; final at checkout"`
	EstShipDate string           `json:"est_ship_date,omitempty"`
	InHandDate  string           `json:"in_hand_date,omitempty"`
}

// CartItemResult is one line of a saved cart.
type CartItemResult struct {
	PartID      int64    `json:"part_id"`
	PartName    string   `json:"part_name,omitempty"`
	MaterialSKU string   `json:"material_sku"`
	Quantity    int      `json:"quantity"`
	UnitPrice   *float64 `json:"unit_price_usd,omitempty"`
	TotalPrice  *float64 `json:"total_price_usd,omitempty"`
}

func (c *Client) checkoutURL(acceptPath string) string {
	if acceptPath == "" {
		return ""
	}

	// The accept path carries a fragment, so join as text rather than as a
	// URL path, which would escape the '#'.
	return strings.TrimRight(c.base.String(), "/") + "/" + strings.TrimLeft(acceptPath, "/")
}

func (c *Client) createCartTool(ctx context.Context, _ *mcp.CallToolRequest, in CreateCartInput) (*mcp.CallToolResult, CartOutput, error) {
	title := strings.TrimSpace(in.Title)
	if title == "" || len([]rune(title)) > maxTitle {
		return nil, CartOutput{}, fmt.Errorf("title must be 1 to %d characters", maxTitle)
	}

	if len(in.Items) == 0 || len(in.Items) > maxCartItems {
		return nil, CartOutput{}, fmt.Errorf("items must hold 1 to %d lines, not %d", maxCartItems, len(in.Items))
	}

	for i, it := range in.Items {
		if err := checkID(it.PartID, fmt.Sprintf("items[%d].part_id", i)); err != nil {
			return nil, CartOutput{}, err
		}

		if it.Quantity < 1 || it.Quantity > maxQuantity {
			return nil, CartOutput{}, fmt.Errorf("items[%d].quantity must be between 1 and %d, not %d", i, maxQuantity, it.Quantity)
		}

		if strings.TrimSpace(it.MaterialSKU) == "" {
			return nil, CartOutput{}, fmt.Errorf("items[%d].material_sku is empty", i)
		}
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, CartOutput{}, err
	}

	if !s.signed {
		return nil, CartOutput{}, errors.New("saved carts need a signed-in account; configure the credentials file")
	}

	mats, err := s.materials(ctx)
	if err != nil {
		return nil, CartOutput{}, err
	}

	bySKU := make(map[string]Material, len(mats))
	for _, m := range mats {
		bySKU[m.SKU] = m
	}

	materialIDs := make([]int64, len(in.Items))

	for i, it := range in.Items {
		m, ok := bySKU[strings.ToUpper(strings.TrimSpace(it.MaterialSKU))]

		switch {
		case !ok:
			return nil, CartOutput{}, fmt.Errorf("items[%d]: unknown material_sku %q; see list_materials", i, it.MaterialSKU)
		case m.OutOfStock:
			return nil, CartOutput{}, fmt.Errorf("items[%d]: %s is out of stock", i, m.SKU)
		}

		materialIDs[i] = m.ID

		if err := s.requireQuotable(ctx, it.PartID); err != nil {
			return nil, CartOutput{}, fmt.Errorf("items[%d]: %w", i, err)
		}
	}

	// The cart belongs to the account, and saving takes all of it, so only
	// build a saved cart when the cart starts empty.
	existing, err := s.cartItems(ctx)
	if err != nil {
		return nil, CartOutput{}, err
	}

	if len(existing) > 0 {
		return nil, CartOutput{}, fmt.Errorf("the account's SendCutSend cart already holds %d items, and saving a cart takes everything in it; "+
			"ask the user to check out, save or empty their cart on sendcutsend.com first", len(existing))
	}

	var added []int64

	out, err := c.fillAndSave(ctx, s, title, in.Items, materialIDs, &added)

	// Saving copies the cart, so take this call's items out again either way.
	// Only ours: the user may be using the cart in a browser meanwhile.
	for _, id := range added {
		_ = s.removeFromCart(context.WithoutCancel(ctx), id)
	}

	if err != nil {
		return nil, CartOutput{}, err
	}

	return nil, out, nil
}

func (c *Client) fillAndSave(ctx context.Context, s *session, title string, items []CartItemInput, materialIDs []int64, added *[]int64) (CartOutput, error) {
	out := CartOutput{Title: title}

	var (
		totals   *CartTotals
		problems []string
	)

	for i, it := range items {
		item, t, err := s.addToCart(ctx, it.PartID, materialIDs[i], it.Quantity)
		if err != nil {
			return CartOutput{}, fmt.Errorf("items[%d]: %w", i, err)
		}

		*added = append(*added, item.ID)
		totals = t

		if !item.CanOrder || len(item.QuoteErrors) > 0 {
			var codes []string
			for _, e := range item.QuoteErrors {
				codes = append(codes, e.Code)
			}

			problems = append(problems, fmt.Sprintf("items[%d] (part %d in %s) can't be ordered: %s", i, it.PartID, it.MaterialSKU, strings.Join(codes, ", ")))

			continue
		}

		unit, total := money(item.UnitCost), money(item.TotalCost)
		out.Items = append(out.Items, CartItemResult{
			PartID: it.PartID, PartName: itemName(item), MaterialSKU: item.Material.SKU,
			Quantity: item.Quantity, UnitPrice: &unit, TotalPrice: &total,
		})
	}

	if len(problems) > 0 {
		return CartOutput{}, fmt.Errorf("no cart was saved: %s", strings.Join(problems, "; "))
	}

	// Check again just before saving, in case the user added something in a
	// browser meanwhile.
	now, err := s.cartItems(ctx)
	if err != nil {
		return CartOutput{}, err
	}

	if len(now) != len(*added) {
		return CartOutput{}, errors.New("no cart was saved: items were added to the account's cart by someone else meanwhile; try again when the cart is empty")
	}

	saved, err := s.saveCart(ctx, title)
	if err != nil {
		return CartOutput{}, fmt.Errorf("save the cart: %w", err)
	}

	out.ID, out.Reference, out.Created = saved.ID, saved.ReferenceNumber, saved.Created
	out.CheckoutURL = c.checkoutURL(saved.AcceptPath)
	out.ItemCount = len(out.Items)

	if totals != nil {
		sub, ship := money(totals.Subtotal), money(totals.Shipping)
		out.Subtotal, out.Shipping = &sub, &ship
		out.EstShipDate, out.InHandDate = deref(totals.EstShipDate), deref(totals.InHandDate)
	}

	return out, nil
}

func itemName(it *CartItem) string {
	if it.Drawing.CustomerFilename != "" {
		return it.Drawing.CustomerFilename
	}

	return it.Drawing.Filename
}

// ListCartsOutput is list_saved_carts' output.
type ListCartsOutput struct {
	Total int          `json:"total"`
	Page  int          `json:"page"`
	Carts []CartOutput `json:"carts"`
}

func (c *Client) toCart(sc *SavedCart) CartOutput {
	return CartOutput{
		ID: sc.ID, Title: sc.Title, Reference: sc.ReferenceNumber, Created: sc.Created,
		CheckoutURL: c.checkoutURL(sc.AcceptPath), ItemCount: len(sc.CartItemIDs),
	}
}

func (c *Client) listCartsTool(ctx context.Context, _ *mcp.CallToolRequest, in ListInput) (*mcp.CallToolResult, ListCartsOutput, error) {
	limit, page, err := in.check()
	if err != nil {
		return nil, ListCartsOutput{}, err
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, ListCartsOutput{}, err
	}

	carts, total, err := s.savedCarts(ctx, page, limit)
	if err != nil {
		return nil, ListCartsOutput{}, err
	}

	out := ListCartsOutput{Total: total, Page: page, Carts: make([]CartOutput, 0, len(carts))}
	for i := range carts {
		out.Carts = append(out.Carts, c.toCart(&carts[i]))
	}

	return nil, out, nil
}

// CartInput names one saved cart.
type CartInput struct {
	CartID int64 `json:"cart_id" jsonschema:"the saved cart's ID"`
}

func (c *Client) getCartTool(ctx context.Context, _ *mcp.CallToolRequest, in CartInput) (*mcp.CallToolResult, CartOutput, error) {
	if err := checkID(in.CartID, "cart_id"); err != nil {
		return nil, CartOutput{}, err
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, CartOutput{}, err
	}

	sc, err := s.savedCart(ctx, in.CartID)
	if err != nil {
		return nil, CartOutput{}, err
	}

	items, err := s.savedCartItems(ctx, in.CartID)
	if err != nil {
		return nil, CartOutput{}, err
	}

	out := c.toCart(sc)
	out.ItemCount = len(items)

	for i := range items {
		it := &items[i]
		out.Items = append(out.Items, CartItemResult{
			PartID: it.Drawing.ID, PartName: itemName(it), MaterialSKU: it.Material.SKU, Quantity: it.Quantity,
		})
	}

	return nil, out, nil
}

func (c *Client) deleteCartTool(ctx context.Context, _ *mcp.CallToolRequest, in CartInput) (*mcp.CallToolResult, DeletedOutput, error) {
	if err := checkID(in.CartID, "cart_id"); err != nil {
		return nil, DeletedOutput{}, err
	}

	s, err := c.newSession(ctx)
	if err != nil {
		return nil, DeletedOutput{}, err
	}

	if err := s.deleteSavedCart(ctx, in.CartID); err != nil {
		return nil, DeletedOutput{}, err
	}

	return nil, DeletedOutput{Deleted: in.CartID}, nil
}
