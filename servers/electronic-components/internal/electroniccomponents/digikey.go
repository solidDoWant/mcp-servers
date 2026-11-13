package electroniccomponents

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"time"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/clientcredentials"
)

// DefaultDigiKeyURL is DigiKey's production API.
const DefaultDigiKeyURL = "https://api.digikey.com"

// DigiKeyConfig configures the DigiKey source.
type DigiKeyConfig struct {
	BaseURL      string
	ClientID     string
	ClientSecret string
	Site         string // locale site, e.g. US
	Currency     string // e.g. USD
}

// DigiKey searches DigiKey's catalog with the Product Information v4 API.
type DigiKey struct {
	cfg    DigiKeyConfig
	client *http.Client
	budget *Budget
	wait   func(context.Context, time.Duration) error
}

// newDigiKey returns a DigiKey source that signs in with OAuth client
// credentials through base, the plain HTTP client.
func newDigiKey(cfg DigiKeyConfig, base *http.Client, budget *Budget, wait func(context.Context, time.Duration) error) *DigiKey {
	cc := clientcredentials.Config{
		ClientID:     cfg.ClientID,
		ClientSecret: cfg.ClientSecret,
		TokenURL:     strings.TrimRight(cfg.BaseURL, "/") + "/v1/oauth2/token",
		AuthStyle:    oauth2.AuthStyleInParams,
	}

	// The token source caches the token until shortly before it expires.
	ctx := context.WithValue(context.Background(), oauth2.HTTPClient, base)

	client := cc.Client(ctx)
	client.Timeout = base.Timeout

	return &DigiKey{cfg: cfg, client: client, budget: budget, wait: wait}
}

func (d *DigiKey) Name() string { return "digikey" }

func (d *DigiKey) header() http.Header {
	h := http.Header{}
	h.Set("X-DIGIKEY-Client-Id", d.cfg.ClientID)
	h.Set("X-DIGIKEY-Locale-Site", d.cfg.Site)
	h.Set("X-DIGIKEY-Locale-Language", "en")
	h.Set("X-DIGIKEY-Locale-Currency", d.cfg.Currency)

	return h
}

func (d *DigiKey) url(path string) string {
	return strings.TrimRight(d.cfg.BaseURL, "/") + "/products/v4/search" + path
}

type dkPriceBreak struct {
	BreakQuantity int     `json:"BreakQuantity"`
	UnitPrice     float64 `json:"UnitPrice"`
}

type dkVariation struct {
	DigiKeyProductNumber string `json:"DigiKeyProductNumber"`
	PackageType          struct {
		Name string `json:"Name"`
	} `json:"PackageType"`
	StandardPricing                 []dkPriceBreak `json:"StandardPricing"`
	MarketPlace                     bool           `json:"MarketPlace"`
	QuantityAvailableforPackageType int            `json:"QuantityAvailableforPackageType"`
	MinimumOrderQuantity            int            `json:"MinimumOrderQuantity"`
}

type dkProduct struct {
	Description struct {
		ProductDescription string `json:"ProductDescription"`
	} `json:"Description"`
	Manufacturer struct {
		Name string `json:"Name"`
	} `json:"Manufacturer"`
	ManufacturerProductNumber string        `json:"ManufacturerProductNumber"`
	ProductURL                string        `json:"ProductUrl"`
	DatasheetURL              string        `json:"DatasheetUrl"`
	ProductVariations         []dkVariation `json:"ProductVariations"`
	QuantityAvailable         *int          `json:"QuantityAvailable"`
	ProductStatus             struct {
		Status string `json:"Status"`
	} `json:"ProductStatus"`
	Parameters []struct {
		ParameterText string `json:"ParameterText"`
		ValueText     string `json:"ValueText"`
	} `json:"Parameters"`
}

func (d *DigiKey) take(ctx context.Context) error {
	return d.budget.Take(func(w time.Duration) error { return d.wait(ctx, w) })
}

func (d *DigiKey) Search(ctx context.Context, q SearchQuery) ([]Offer, error) {
	body := map[string]any{"Keywords": truncate(q.Keyword, 250), "Limit": q.Limit, "Offset": 0}
	if q.InStockOnly {
		body["FilterOptionsRequest"] = map[string]any{"MinimumQuantityAvailable": 1}
	}

	if err := d.take(ctx); err != nil {
		return nil, err
	}

	var out struct {
		ExactMatches []dkProduct `json:"ExactMatches"`
		Products     []dkProduct `json:"Products"`
	}

	if err := doJSON(ctx, d.client, "DigiKey", http.MethodPost, d.url("/keyword"), d.header(), body, &out); err != nil {
		return nil, err
	}

	seen := map[string]bool{}

	var offers []Offer

	for _, p := range append(out.ExactMatches, out.Products...) {
		o := d.offer(&p, false)
		key := o.DistributorPN + "|" + o.MPN

		if o.MPN == "" || seen[key] {
			continue
		}

		seen[key] = true

		offers = append(offers, o)

		if len(offers) == q.Limit {
			break
		}
	}

	return offers, nil
}

func (d *DigiKey) Lookup(ctx context.Context, partNumber string) ([]Offer, error) {
	if err := d.take(ctx); err != nil {
		return nil, err
	}

	var out struct {
		Product *dkProduct `json:"Product"`
	}

	// Part numbers can contain slashes and '#'.
	err := doJSON(ctx, d.client, "DigiKey", http.MethodGet, d.url("/"+url.PathEscape(strings.TrimSpace(partNumber))+"/productdetails"), d.header(), nil, &out)

	var he *httpError
	if errors.As(err, &he) && he.status == http.StatusNotFound {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	if out.Product == nil {
		return nil, nil
	}

	return []Offer{d.offer(out.Product, true)}, nil
}

// pickVariation chooses the packaging that best represents the offer: one
// with a price, in stock, and sold by DigiKey rather than its marketplace.
func pickVariation(vs []dkVariation) *dkVariation {
	var (
		best  *dkVariation
		score = -1
	)

	for i := range vs {
		v := &vs[i]
		s := 0

		if len(v.StandardPricing) > 0 {
			s += 4
		}

		if v.QuantityAvailableforPackageType > 0 {
			s += 2
		}

		if !v.MarketPlace {
			s++
		}

		if s > score {
			best, score = v, s
		}
	}

	return best
}

const maxSpecs = 25

func (d *DigiKey) offer(p *dkProduct, withSpecs bool) Offer {
	o := Offer{
		Source:       "digikey",
		MPN:          strings.TrimSpace(p.ManufacturerProductNumber),
		Manufacturer: p.Manufacturer.Name,
		Description:  truncate(p.Description.ProductDescription, 200),
		Stock:        p.QuantityAvailable,
		Lifecycle:    p.ProductStatus.Status,
		Currency:     d.cfg.Currency,
		ProductURL:   p.ProductURL,
		DatasheetURL: absoluteURL(p.DatasheetURL),
	}

	if v := pickVariation(p.ProductVariations); v != nil {
		o.DistributorPN = v.DigiKeyProductNumber
		o.MinOrderQty = v.MinimumOrderQuantity

		for _, b := range v.StandardPricing {
			o.PriceBreaks = append(o.PriceBreaks, PriceBreak{Quantity: b.BreakQuantity, UnitPrice: b.UnitPrice})
		}
	}

	for _, prm := range p.Parameters {
		if prm.ParameterText == "Package / Case" {
			o.Package = prm.ValueText
		}

		if withSpecs && prm.ValueText != "" && prm.ValueText != "-" && len(o.Specs) < maxSpecs {
			if o.Specs == nil {
				o.Specs = map[string]string{}
			}

			o.Specs[truncate(prm.ParameterText, 60)] = truncate(prm.ValueText, 100)
		}
	}

	return o
}

// absoluteURL gives a protocol-relative URL ("//host/path") a scheme.
func absoluteURL(u string) string {
	if strings.HasPrefix(u, "//") {
		return "https:" + u
	}

	return u
}
