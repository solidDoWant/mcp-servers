package parts

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// DefaultJLCSearchURL is tscircuit's free, keyless search over JLCPCB's
// parts library (the parts JLCPCB stocks for assembly).
const DefaultJLCSearchURL = "https://jlcsearch.tscircuit.com"

// JLCSearch searches JLCPCB's parts library through jlcsearch.
type JLCSearch struct {
	base   string
	client *http.Client
	budget *Budget
	wait   func(context.Context, time.Duration) error
}

func (j *JLCSearch) Name() string { return "jlcpcb" }

type jlcComponent struct {
	LCSC        int64    `json:"lcsc"`
	MFR         string   `json:"mfr"`
	Package     string   `json:"package"`
	IsBasic     bool     `json:"is_basic"`
	IsPreferred bool     `json:"is_preferred"`
	Description string   `json:"description"`
	Stock       *int     `json:"stock"`
	Price       *float64 `json:"price"`
}

// lcscNumber matches an LCSC part number, which jlcsearch looks up exactly.
var lcscNumber = regexp.MustCompile(`^[Cc]?\d+$`)

func (j *JLCSearch) query(ctx context.Context, params url.Values) ([]jlcComponent, error) {
	var out struct {
		Components []jlcComponent `json:"components"`
	}

	target := strings.TrimRight(j.base, "/") + "/api/search?" + params.Encode()

	// jlcsearch answers the odd request with a transient 500; one retry is
	// cheap since it has no quota.
	for attempt := 0; ; attempt++ {
		if err := j.budget.Take(func(d time.Duration) error { return j.wait(ctx, d) }); err != nil {
			return nil, err
		}

		err := doJSON(ctx, j.client, "jlcsearch", http.MethodGet, target, nil, nil, &out)

		var he *httpError
		if attempt == 0 && errors.As(err, &he) && he.status >= 500 {
			continue
		}

		if err != nil {
			return nil, err
		}

		return out.Components, nil
	}
}

func (j *JLCSearch) Search(ctx context.Context, q SearchQuery) ([]Offer, error) {
	params := url.Values{}
	params.Set("q", q.Keyword)
	params.Set("limit", strconv.Itoa(q.Limit))

	if q.JLCBasicOnly {
		params.Set("is_basic", "true")
	}

	// jlcsearch only returns parts in stock, except for an exact LCSC
	// number, so InStockOnly needs no parameter.
	comps, err := j.query(ctx, params)
	if err != nil {
		return nil, err
	}

	offers := make([]Offer, 0, len(comps))

	for _, c := range comps {
		o := c.offer()
		if q.InStockOnly && (o.Stock == nil || *o.Stock <= 0) {
			continue
		}

		offers = append(offers, o)
	}

	return offers, nil
}

func (j *JLCSearch) Lookup(ctx context.Context, partNumber string) ([]Offer, error) {
	pn := strings.TrimSpace(partNumber)
	params := url.Values{}
	params.Set("q", pn)
	params.Set("limit", "20")

	comps, err := j.query(ctx, params)
	if err != nil {
		return nil, err
	}

	exactLCSC := lcscNumber.MatchString(pn)

	var offers []Offer

	for _, c := range comps {
		// A keyword search also matches longer part numbers; keep exact ones.
		if exactLCSC || mpnKey(c.MFR) == mpnKey(pn) {
			offers = append(offers, c.offer())
		}
	}

	return offers, nil
}

func (c *jlcComponent) offer() Offer {
	o := Offer{
		Source:        "jlcpcb",
		MPN:           c.MFR,
		Description:   truncate(c.Description, 200),
		DistributorPN: fmt.Sprintf("C%d", c.LCSC),
		Package:       c.Package,
		Stock:         c.Stock,
		Currency:      "USD",
		ProductURL:    fmt.Sprintf("https://jlcpcb.com/partdetail/C%d", c.LCSC),
		JLCBasic:      boolPtr(c.IsBasic),
		JLCPreferred:  boolPtr(c.IsPreferred),
	}

	if o.Package == "-" {
		o.Package = ""
	}

	if c.Price != nil && *c.Price > 0 {
		o.PriceBreaks = []PriceBreak{{Quantity: 1, UnitPrice: *c.Price}}
	}

	return o
}
