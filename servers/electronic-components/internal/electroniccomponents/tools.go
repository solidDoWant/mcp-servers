package electroniccomponents

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"
	"unicode"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// Instructions is sent to clients with the tool list.
const Instructions = `Search electronic component distributors and compare offers: DigiKey, Mouser, and JLCPCB's parts library (the parts JLCPCB stocks for PCB assembly). list_sources says which are enabled.
search_parts searches every enabled source at once and groups offers by manufacturer part number, with the cheapest offer at a quantity. get_part looks up one part number for full specifications.
DigiKey and Mouser allow about a thousand calls a day shared by everyone, and results are cached for a while, so search broadly once rather than many narrow times, and don't repeat a search to refresh it.
For JLCPCB assembly, "basic" parts avoid the per-part setup fee that "extended" parts carry; jlc_basic_only finds them.`

const (
	searchDescription  = "Search enabled distributors (DigiKey, Mouser, JLCPCB parts library) for components by keyword or part number, e.g. 'STM32F103C8T6', '10k 0603 resistor', 'USB-C receptacle'. Offers for the same manufacturer part number are grouped, with the cheapest offer at the given quantity that has enough stock. A source that fails is listed under errors while the others still answer."
	getPartDescription = "Look up one part by manufacturer part number, or by a distributor's own number (DigiKey 296-1234-ND, Mouser 511-LM317T, LCSC C8734): every enabled source's offer with price breaks and specifications."
	sourcesDescription = "List the sources: which are enabled (and why not), and how much of each one's daily call budget is used."

	defaultLimit   = 10
	maxLimit       = 50
	maxQuantity    = 1_000_000
	maxPriceBreaks = 8
	maxQueryLen    = 200
)

// Tools registers the server's tools on srv.
func Tools(s *Service) func(*mcp.Server) {
	return func(srv *mcp.Server) {
		mcp.AddTool(srv, &mcp.Tool{Name: "search_parts", Description: searchDescription, Annotations: readOnly("Search component distributors")}, s.searchTool)
		mcp.AddTool(srv, &mcp.Tool{Name: "get_part", Description: getPartDescription, Annotations: readOnly("Look up a component")}, s.getPartTool)
		mcp.AddTool(srv, &mcp.Tool{Name: "list_sources", Description: sourcesDescription, Annotations: readOnly("List component sources")}, s.sourcesTool)
	}
}

func readOnly(title string) *mcp.ToolAnnotations {
	openWorld := true

	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, OpenWorldHint: &openWorld}
}

// SearchInput is search_parts' input.
type SearchInput struct {
	Query        string   `json:"query" jsonschema:"keywords or a part number"`
	Sources      []string `json:"sources,omitempty" jsonschema:"only these sources: digikey, mouser, jlcpcb; default all enabled"`
	Limit        int      `json:"limit,omitempty" jsonschema:"results per source, 1 to 50 (default 10)"`
	Quantity     int      `json:"quantity,omitempty" jsonschema:"how many you need, for the price comparison (default 1)"`
	InStockOnly  bool     `json:"in_stock_only,omitempty" jsonschema:"leave out parts with no stock"`
	JLCBasicOnly bool     `json:"jlc_basic_only,omitempty" jsonschema:"JLCPCB only: basic parts, which carry no assembly setup fee"`
}

// SearchOutput is search_parts' output.
type SearchOutput struct {
	Query           string        `json:"query"`
	Quantity        int           `json:"quantity"`
	SourcesSearched []string      `json:"sources_searched"`
	FromCache       []CacheNote   `json:"from_cache,omitempty" jsonschema:"sources answered from the cache rather than asked"`
	Parts           []PartGroup   `json:"parts"`
	Errors          []SourceError `json:"errors,omitempty"`
}

// CacheNote says that one source's answer came from the cache.
type CacheNote struct {
	Source     string `json:"source"`
	AgeSeconds int    `json:"age_seconds" jsonschema:"how long ago the source was asked"`
	// Stale answers are past their freshness, given because the source
	// failed just now.
	Stale bool   `json:"stale,omitempty" jsonschema:"the source failed just now, so this older answer was used"`
	Error string `json:"error,omitempty" jsonschema:"why the source failed, for a stale answer"`
}

func cacheNotes(results []result) []CacheNote {
	var notes []CacheNote

	for _, r := range results {
		if !r.meta.Hit {
			continue
		}

		n := CacheNote{Source: r.source, AgeSeconds: int(r.meta.Age.Seconds()), Stale: r.meta.Stale}
		if r.meta.FetchErr != nil {
			n.Error = r.meta.FetchErr.Error()
		}

		notes = append(notes, n)
	}

	return notes
}

// PartGroup is every offer for one manufacturer part number.
type PartGroup struct {
	MPN          string `json:"mpn"`
	Manufacturer string `json:"manufacturer,omitempty" jsonschema:"set when every offer that names a maker names this one"`
	// Generic parts such as AMS1117-3.3 are made by several companies under
	// one part number. They stay in one group, since they're usually drop-in
	// substitutes, but the group says so.
	Manufacturers []string   `json:"manufacturers,omitempty" jsonschema:"the makers, when offers for this part number come from more than one; each offer names its own"`
	Description   string     `json:"description,omitempty"`
	Package       string     `json:"package,omitempty"`
	Best          *BestOffer `json:"best,omitempty" jsonschema:"the cheapest offer at the quantity with enough (or unknown) stock"`
	Offers        []OfferOut `json:"offers"`
}

// BestOffer names the cheapest offer.
type BestOffer struct {
	Source       string  `json:"source"`
	Manufacturer string  `json:"manufacturer,omitempty"`
	UnitPrice    float64 `json:"unit_price"`
	TotalPrice   float64 `json:"total_price"`
	Currency     string  `json:"currency,omitempty"`
}

// OfferOut is an offer as tools return it.
type OfferOut struct {
	Offer

	PriceAtQuantity *float64 `json:"price_at_quantity,omitempty" jsonschema:"unit price at the requested quantity"`
}

func checkLimit(v int) (int, error) {
	if v == 0 {
		return defaultLimit, nil
	}

	if v < 1 || v > maxLimit {
		return 0, fmt.Errorf("limit must be between 1 and %d, not %d", maxLimit, v)
	}

	return v, nil
}

func checkQuantity(v int) (int, error) {
	if v == 0 {
		return 1, nil
	}

	if v < 1 || v > maxQuantity {
		return 0, fmt.Errorf("quantity must be between 1 and %d, not %d", maxQuantity, v)
	}

	return v, nil
}

func checkText(name, v string) (string, error) {
	v = strings.TrimSpace(v)

	switch {
	case v == "":
		return "", fmt.Errorf("%s is empty", name)
	case len([]rune(v)) > maxQueryLen:
		return "", fmt.Errorf("%s is longer than %d characters", name, maxQueryLen)
	case strings.ContainsFunc(v, func(r rune) bool { return r < 0x20 }):
		return "", fmt.Errorf("%s contains control characters", name)
	}

	return v, nil
}

// errorsAndFailures splits results into errors, and fails when every source
// failed: an empty answer must mean nothing was found.
func errorsAndFailures(results []result) ([]SourceError, error) {
	var (
		errs []SourceError
		msgs []string
	)

	for _, r := range results {
		if r.err != nil {
			errs = append(errs, SourceError{Source: r.source, Error: r.err.Error()})
			msgs = append(msgs, r.err.Error())
		}
	}

	if len(errs) == len(results) && len(errs) > 0 {
		return nil, errors.New(strings.Join(msgs, "; "))
	}

	return errs, nil
}

func (s *Service) searchTool(ctx context.Context, _ *mcp.CallToolRequest, in SearchInput) (*mcp.CallToolResult, SearchOutput, error) {
	query, err := checkText("query", in.Query)
	if err != nil {
		return nil, SearchOutput{}, err
	}

	limit, err := checkLimit(in.Limit)
	if err != nil {
		return nil, SearchOutput{}, err
	}

	qty, err := checkQuantity(in.Quantity)
	if err != nil {
		return nil, SearchOutput{}, err
	}

	names, err := s.resolve(in.Sources)
	if err != nil {
		return nil, SearchOutput{}, err
	}

	results := s.Search(ctx, names, SearchQuery{Keyword: query, Limit: limit, InStockOnly: in.InStockOnly, JLCBasicOnly: in.JLCBasicOnly})

	errs, err := errorsAndFailures(results)
	if err != nil {
		return nil, SearchOutput{}, err
	}

	out := SearchOutput{Query: query, Quantity: qty, SourcesSearched: names, Errors: errs, Parts: group(results, qty)}

	out.FromCache = cacheNotes(results)

	return nil, out, nil
}

// group merges offers by part number. Groups are ordered by each source's
// own ranking, interleaved, so no one source's results crowd out the
// others'.
func group(results []result, qty int) []PartGroup {
	byKey := map[string]*PartGroup{}

	var order []string

	for rank := 0; ; rank++ {
		more := false

		for _, r := range results {
			if rank >= len(r.offers) {
				continue
			}

			more = true
			o := r.offers[rank]
			k := mpnKey(o.MPN)

			g, ok := byKey[k]
			if !ok {
				g = &PartGroup{MPN: o.MPN, Offers: []OfferOut{}}
				byKey[k] = g
				order = append(order, k)
			}

			g.Offers = append(g.Offers, outOffer(o, qty, false))

			if g.Description == "" {
				g.Description = o.Description
			}

			if g.Package == "" {
				g.Package = o.Package
			}
		}

		if !more {
			break
		}
	}

	groups := make([]PartGroup, 0, len(order))

	for _, k := range order {
		g := byKey[k]
		g.Manufacturer, g.Manufacturers = makers(g.Offers)
		g.Best = best(g.Offers, qty)
		groups = append(groups, *g)
	}

	return groups
}

// outOffer copies an offer for output, with the price at qty. Cached offers
// are shared, so it never modifies o.
func outOffer(o Offer, qty int, withSpecs bool) OfferOut {
	out := OfferOut{Offer: o}

	if len(o.PriceBreaks) > maxPriceBreaks {
		out.PriceBreaks = o.PriceBreaks[:maxPriceBreaks]
	}

	if !withSpecs {
		out.Specs = nil
	}

	if p, ok := o.PriceAt(qty); ok {
		out.PriceAtQuantity = &p
	}

	return out
}

// makers returns the single maker the offers name, or, when they name more
// than one, each maker once in the order offers name them. Offers that name
// no maker (jlcsearch's) don't count.
func makers(offers []OfferOut) (single string, several []string) {
	seen := map[string]bool{}

	for _, o := range offers {
		k := makerKey(o.Manufacturer)
		if k == "" || seen[k] {
			continue
		}

		seen[k] = true

		several = append(several, o.Manufacturer)
	}

	if len(several) == 1 {
		return several[0], nil
	}

	return "", several
}

// makerKey compares makers' names ignoring case, spacing and punctuation, so
// "STMicroelectronics" matches "ST Microelectronics".
func makerKey(name string) string {
	var b strings.Builder

	for _, r := range strings.ToLower(name) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}

	return b.String()
}

func best(offers []OfferOut, qty int) *BestOffer {
	var b *BestOffer

	for _, o := range offers {
		if o.PriceAtQuantity == nil || (o.Stock != nil && *o.Stock < qty) || (o.MinOrderQty > qty) {
			continue
		}

		if b == nil || *o.PriceAtQuantity < b.UnitPrice {
			b = &BestOffer{
				Source: o.Source, Manufacturer: o.Manufacturer, UnitPrice: *o.PriceAtQuantity, Currency: o.Currency,
				TotalPrice: math.Round(*o.PriceAtQuantity*float64(qty)*100) / 100,
			}
		}
	}

	return b
}

// GetPartInput is get_part's input.
type GetPartInput struct {
	PartNumber string   `json:"part_number" jsonschema:"a manufacturer part number, or a DigiKey, Mouser or LCSC number"`
	Sources    []string `json:"sources,omitempty" jsonschema:"only these sources; default all enabled"`
}

// GetPartOutput is get_part's output.
type GetPartOutput struct {
	PartNumber string        `json:"part_number"`
	Offers     []OfferOut    `json:"offers" jsonschema:"empty when no source carries the part"`
	FromCache  []CacheNote   `json:"from_cache,omitempty" jsonschema:"sources answered from the cache rather than asked"`
	Errors     []SourceError `json:"errors,omitempty"`
}

func (s *Service) getPartTool(ctx context.Context, _ *mcp.CallToolRequest, in GetPartInput) (*mcp.CallToolResult, GetPartOutput, error) {
	pn, err := checkText("part_number", in.PartNumber)
	if err != nil {
		return nil, GetPartOutput{}, err
	}

	// Mouser treats '|' as a separator between part numbers.
	if strings.Contains(pn, "|") {
		return nil, GetPartOutput{}, errors.New("part_number must be one part number, without '|'")
	}

	names, err := s.resolve(in.Sources)
	if err != nil {
		return nil, GetPartOutput{}, err
	}

	results := s.Lookup(ctx, names, pn)

	errs, err := errorsAndFailures(results)
	if err != nil {
		return nil, GetPartOutput{}, err
	}

	out := GetPartOutput{PartNumber: pn, Offers: []OfferOut{}, Errors: errs, FromCache: cacheNotes(results)}

	for _, r := range results {
		for _, o := range r.offers {
			out.Offers = append(out.Offers, outOffer(o, 1, true))
		}
	}

	return nil, out, nil
}

// SourcesOutput is list_sources' output.
type SourcesOutput struct {
	Sources      []SourceInfo `json:"sources"`
	CacheBackend string       `json:"cache_backend" jsonschema:"memory (lost on restart) or redis (shared, kept across restarts)"`
	CacheTTL     string       `json:"cache_ttl" jsonschema:"how long answers are reused"`
	StaleFor     string       `json:"stale_for,omitempty" jsonschema:"how much longer old answers are kept, to use when a source fails"`
}

// SourceInfo is one source's state.
type SourceInfo struct {
	Source     string `json:"source"`
	Enabled    bool   `json:"enabled"`
	Reason     string `json:"reason,omitempty" jsonschema:"why it's disabled"`
	CallsToday int    `json:"calls_today,omitempty"`
	DailyLimit int    `json:"daily_limit,omitempty" jsonschema:"0 means no daily limit"`
}

func (s *Service) sourcesTool(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, SourcesOutput, error) {
	out := SourcesOutput{CacheBackend: s.cache.Store().Kind(), CacheTTL: s.cacheTTL.Round(time.Second).String()}
	if s.staleFor > 0 {
		out.StaleFor = s.staleFor.Round(time.Second).String()
	}

	for _, n := range sourceOrder {
		info := SourceInfo{Source: n}

		if b, ok := s.budgets[n]; ok {
			info.Enabled = true
			info.CallsToday, info.DailyLimit = b.Status(ctx)
		} else {
			info.Reason = s.disabled[n]
		}

		out.Sources = append(out.Sources, info)
	}

	return nil, out, nil
}
