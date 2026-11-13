package electroniccomponents

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultMouserURL is Mouser's Search API.
const DefaultMouserURL = "https://api.mouser.com"

// Mouser searches Mouser's catalog with the Search API. Its key is a Search
// API key, which can't reach Mouser's cart or order APIs.
type Mouser struct {
	base   string
	apiKey string
	client *http.Client
	budget *Budget
	wait   func(context.Context, time.Duration) error
}

func (m *Mouser) Name() string { return "mouser" }

type mouserPart struct {
	ManufacturerPartNumber string `json:"ManufacturerPartNumber"`
	Manufacturer           string `json:"Manufacturer"`
	Description            string `json:"Description"`
	MouserPartNumber       string `json:"MouserPartNumber"`
	Availability           string `json:"Availability"`
	AvailabilityInStock    string `json:"AvailabilityInStock"`
	Min                    string `json:"Min"`
	LifecycleStatus        string `json:"LifecycleStatus"`
	ProductDetailURL       string `json:"ProductDetailUrl"`
	DataSheetURL           string `json:"DataSheetUrl"`
	Category               string `json:"Category"`
	PriceBreaks            []struct {
		Quantity int     `json:"Quantity"`
		RawPrice float64 `json:"RawPrice"`
		Currency string  `json:"Currency"`
	} `json:"PriceBreaks"`
	ProductAttributes []struct {
		AttributeName  string `json:"AttributeName"`
		AttributeValue string `json:"AttributeValue"`
	} `json:"ProductAttributes"`
}

type mouserResponse struct {
	Errors []struct {
		Code         string `json:"Code"`
		Message      string `json:"Message"`
		PropertyName string `json:"PropertyName"`
	} `json:"Errors"`
	SearchResults *struct {
		Parts []mouserPart `json:"Parts"`
	} `json:"SearchResults"`
}

func (m *Mouser) call(ctx context.Context, path string, body any) ([]mouserPart, error) {
	if err := m.budget.Take(func(d time.Duration) error { return m.wait(ctx, d) }); err != nil {
		return nil, err
	}

	target := strings.TrimRight(m.base, "/") + path + "?" + url.Values{"apiKey": {m.apiKey}}.Encode()

	var out mouserResponse
	if err := doJSON(ctx, m.client, "Mouser", http.MethodPost, target, nil, body, &out); err != nil {
		return nil, err
	}

	// Mouser reports most failures, a refused key included, in the body of
	// an HTTP 200.
	if len(out.Errors) > 0 {
		var msgs []string

		for _, e := range out.Errors {
			name := e.PropertyName
			if name == "" {
				name = e.Code
			}

			msgs = append(msgs, strings.TrimSpace(name+": "+e.Message))
		}

		return nil, fmt.Errorf("Mouser refused the request: %s", truncate(strings.Join(msgs, "; "), 300))
	}

	if out.SearchResults == nil {
		return nil, nil
	}

	return out.SearchResults.Parts, nil
}

func (m *Mouser) Search(ctx context.Context, q SearchQuery) ([]Offer, error) {
	opts := "None"
	if q.InStockOnly {
		opts = "InStock"
	}

	parts, err := m.call(ctx, "/api/v1/search/keyword", map[string]any{
		"SearchByKeywordRequest": map[string]any{
			"keyword": q.Keyword, "records": q.Limit, "startingRecord": 0, "searchOptions": opts,
		},
	})
	if err != nil {
		return nil, err
	}

	offers := make([]Offer, 0, len(parts))
	for i := range parts {
		if o := parts[i].offer(false); o.MPN != "" {
			offers = append(offers, o)
		}
	}

	return offers, nil
}

func (m *Mouser) Lookup(ctx context.Context, partNumber string) ([]Offer, error) {
	parts, err := m.call(ctx, "/api/v1/search/partnumber", map[string]any{
		"SearchByPartRequest": map[string]any{
			"mouserPartNumber": strings.TrimSpace(partNumber), "partSearchOptions": "Exact",
		},
	})
	if err != nil {
		return nil, err
	}

	var offers []Offer

	for i := range parts {
		if o := parts[i].offer(true); o.MPN != "" {
			offers = append(offers, o)
		}
	}

	return offers, nil
}

// leadingCount matches the number at the start of a value such as "1234"
// or "1,234 In Stock".
var leadingCount = regexp.MustCompile(`^\s*(\d[\d,]*)`)

// stockCount reads that number, or nil when there isn't one.
func stockCount(s string) *int {
	m := leadingCount.FindStringSubmatch(s)
	if m == nil {
		return nil
	}

	n, err := strconv.Atoi(strings.ReplaceAll(m[1], ",", ""))
	if err != nil {
		return nil
	}

	return &n
}

func (p *mouserPart) offer(withSpecs bool) Offer {
	o := Offer{
		Source:        "mouser",
		MPN:           strings.TrimSpace(p.ManufacturerPartNumber),
		Manufacturer:  p.Manufacturer,
		Description:   truncate(p.Description, 200),
		DistributorPN: p.MouserPartNumber,
		Lifecycle:     p.LifecycleStatus,
		ProductURL:    p.ProductDetailURL,
		DatasheetURL:  absoluteURL(p.DataSheetURL),
	}

	o.Stock = stockCount(p.AvailabilityInStock)
	if o.Stock == nil {
		o.Stock = stockCount(p.Availability)
	}

	if n := stockCount(p.Min); n != nil {
		o.MinOrderQty = *n
	}

	for _, b := range p.PriceBreaks {
		if b.RawPrice > 0 {
			o.PriceBreaks = append(o.PriceBreaks, PriceBreak{Quantity: b.Quantity, UnitPrice: b.RawPrice})
			o.Currency = b.Currency
		}
	}

	for _, a := range p.ProductAttributes {
		if a.AttributeName == "Package / Case" {
			o.Package = a.AttributeValue
		}

		if withSpecs && a.AttributeValue != "" && len(o.Specs) < maxSpecs {
			if o.Specs == nil {
				o.Specs = map[string]string{}
			}

			o.Specs[truncate(a.AttributeName, 60)] = truncate(a.AttributeValue, 100)
		}
	}

	if withSpecs && p.Category != "" {
		if o.Specs == nil {
			o.Specs = map[string]string{}
		}

		o.Specs["Category"] = p.Category
	}

	return o
}
