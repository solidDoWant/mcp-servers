// Package electroniccomponents searches electronic component distributors (DigiKey, Mouser
// and JLCPCB's parts library through jlcsearch) and merges their offers by
// manufacturer part number, so prices and stock can be compared.
//
// Each source is enabled only when its credentials are configured, except
// jlcsearch, which needs none. DigiKey and Mouser allow about a thousand
// calls a day, so responses are cached and every source has a rate limit and
// a daily budget below the distributor's own.
package electroniccomponents

import (
	"context"
	"errors"
	"strings"
)

// PriceBreak is the unit price from a quantity up.
type PriceBreak struct {
	Quantity  int     `json:"quantity"`
	UnitPrice float64 `json:"unit_price"`
}

// Offer is one distributor's listing of a part.
type Offer struct {
	Source        string            `json:"source"`
	MPN           string            `json:"mpn"`
	Manufacturer  string            `json:"manufacturer,omitempty"`
	Description   string            `json:"description,omitempty"`
	DistributorPN string            `json:"distributor_pn,omitempty"`
	Package       string            `json:"package,omitempty"`
	Stock         *int              `json:"stock,omitempty"`
	MinOrderQty   int               `json:"min_order_qty,omitempty"`
	Lifecycle     string            `json:"lifecycle,omitempty"`
	Currency      string            `json:"currency,omitempty"`
	PriceBreaks   []PriceBreak      `json:"price_breaks,omitempty"`
	ProductURL    string            `json:"product_url,omitempty"`
	DatasheetURL  string            `json:"datasheet_url,omitempty"`
	JLCBasic      *bool             `json:"jlc_basic,omitempty"`
	JLCPreferred  *bool             `json:"jlc_preferred,omitempty"`
	Specs         map[string]string `json:"specs,omitempty"`
}

// PriceAt returns the unit price at quantity: the price of the largest break
// at or below it. ok is false when the offer has no price for that quantity.
func (o *Offer) PriceAt(quantity int) (float64, bool) {
	price, found, best := 0.0, false, 0

	for _, b := range o.PriceBreaks {
		if b.Quantity <= quantity && b.Quantity >= best && b.UnitPrice > 0 {
			price, found, best = b.UnitPrice, true, b.Quantity
		}
	}

	return price, found
}

// SearchQuery is a keyword search.
type SearchQuery struct {
	Keyword      string
	Limit        int
	InStockOnly  bool
	JLCBasicOnly bool
}

// Provider is one distributor's API.
type Provider interface {
	// Name is the source's ID in tool input and output.
	Name() string
	// Search runs a keyword search.
	Search(ctx context.Context, q SearchQuery) ([]Offer, error)
	// Lookup finds one part by manufacturer or distributor part number. An
	// unknown part number yields no offers, not an error.
	Lookup(ctx context.Context, partNumber string) ([]Offer, error)
}

// ErrBudget means a source's request budget is used up for now.
var ErrBudget = errors.New("request budget used up")

// mpnKey is the form of a part number offers are merged on.
func mpnKey(mpn string) string {
	return strings.ToUpper(strings.Join(strings.Fields(mpn), ""))
}

// truncate cuts s to n runes, marking the cut.
func truncate(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > n {
		return string(r[:n-1]) + "…"
	}

	return s
}

func intPtr(v int) *int { return &v }

func boolPtr(v bool) *bool { return &v }
