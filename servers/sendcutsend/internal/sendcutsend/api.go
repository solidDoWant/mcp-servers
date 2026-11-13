package sendcutsend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"time"
)

// Decimal is a number the API sends either as a JSON string ("25.1") or as
// a JSON number (0), depending on the endpoint.
type Decimal float64

// UnmarshalJSON accepts a string, a number or null.
func (d *Decimal) UnmarshalJSON(b []byte) error {
	var n json.Number

	if string(b) == "null" {
		*d = 0

		return nil
	}

	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		n = json.Number(s)
	} else if err := json.Unmarshal(b, &n); err != nil {
		return fmt.Errorf("decimal: %w", err)
	}

	if n == "" {
		*d = 0

		return nil
	}

	f, err := strconv.ParseFloat(string(n), 64)
	if err != nil {
		return fmt.Errorf("decimal %q: %w", n, err)
	}

	*d = Decimal(f)

	return nil
}

// Issue is a problem the site reports with a part or a quote.
type Issue struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Drawing is an uploaded part file and what the site's processing found.
type Drawing struct {
	ID               int64    `json:"id"`
	Filename         string   `json:"filename"`
	CustomerFilename string   `json:"customer_filename"`
	State            string   `json:"state"`
	Progress         *int     `json:"progress"`
	Units            string   `json:"units"`
	Width            Decimal  `json:"width"`
	Height           Decimal  `json:"height"`
	Length           Decimal  `json:"length"`
	Area             Decimal  `json:"area"`
	Contours         int      `json:"contours"`
	IsStep           bool     `json:"is_step"`
	LikelyFormat     *string  `json:"likely_drawing_format"`
	PossibleFormats  []string `json:"possible_drawing_formats"`
	Errors           []Issue  `json:"drawing_errors"`
	Warnings         []Issue  `json:"drawing_warnings"`
	PreviewURL       string   `json:"preview_url"`
	ModelURL         string   `json:"model_url"`
	Created          string   `json:"created"`
	// Part is the part's saved configuration: what it was last ordered or
	// configured with, or the bends SendCutSend detected in a 3D file.
	Part *SavedPart `json:"part"`
}

// SavedPart is a part's saved service configuration.
type SavedPart struct {
	Operations []string     `json:"operations"`
	Bending    []BendConfig `json:"bending"`
	Tapping    []TapConfig  `json:"tapping"`
	Powder     string       `json:"powder"`
	Anodizing  string       `json:"anodizing"`
	Plating    string       `json:"plating"`
}

// BendConfig is how to bend one bend line.
type BendConfig struct {
	ID        string  `json:"id"`
	Angle     float64 `json:"angle"`
	Direction string  `json:"direction"`
	Radius    float64 `json:"radius,omitempty"`
}

// TapConfig is the tap for one hole.
type TapConfig struct {
	ID  string `json:"id"`
	Tap string `json:"tap"`
}

// Name is the file name the customer uploaded.
func (d *Drawing) Name() string {
	if d.CustomerFilename != "" {
		return d.CustomerFilename
	}

	return d.Filename
}

// Processed reports whether the site has finished with the drawing.
func (d *Drawing) Processed() bool {
	return d.State == "complete" || d.State == "error" || d.State == "failed"
}

type envelope[T any] struct {
	Data T `json:"data"`
}

// uploadURL asks for a pre-signed URL to upload one file to. It is valid
// for 15 minutes.
func (s *session) uploadURL(ctx context.Context, filename string) (string, error) {
	var out envelope[struct {
		URL string `json:"url"`
	}]

	if err := s.call(ctx, http.MethodPost, "/drawings/upload", map[string]string{"filename": filename}, &out); err != nil {
		return "", err
	}

	if out.Data.URL == "" {
		return "", errors.New("SendCutSend returned no upload URL")
	}

	return out.Data.URL, nil
}

// createDrawing registers an uploaded file as a part. The site processes it
// in the background.
func (s *session) createDrawing(ctx context.Context, uploadURL, name, units string) (*Drawing, error) {
	body := map[string]any{
		// The app passes the whole pre-signed URL as the file name; the site
		// takes the object key from it.
		"filename":          uploadURL,
		"customer_filename": name,
		// The app sends this fixed address with every upload.
		"email":             "SUPPORT@SENDCUTSEND.COM",
		"processor_version": s.app.DrawingAPIVersion,
	}

	if units != "" {
		body["units"] = units
	}

	if s.app.UserToken != nil {
		body["user_token"] = *s.app.UserToken
	}

	var out envelope[Drawing]
	if err := s.call(ctx, http.MethodPost, "/drawings", body, &out); err != nil {
		return nil, err
	}

	return &out.Data, nil
}

func (s *session) guestParams() url.Values {
	q := url.Values{}
	if s.app.UserToken != nil {
		q.Set("user_token", *s.app.UserToken)
	}

	return q
}

func (s *session) drawing(ctx context.Context, id int64) (*Drawing, error) {
	var out envelope[Drawing]
	if err := s.call(ctx, http.MethodGet, fmt.Sprintf("/drawings/%d", id), s.guestParams(), &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("part %d: %w (it may have been deleted, or belong to another account)", id, ErrNotFound)
		}

		return nil, err
	}

	return &out.Data, nil
}

// waitProcessed polls a drawing until the site has processed it or the
// client's process timeout passes. It returns the latest state either way.
func (s *session) waitProcessed(ctx context.Context, d *Drawing) (*Drawing, error) {
	deadline := time.NewTimer(s.c.processTimeout)
	defer deadline.Stop()

	tick := time.NewTicker(s.c.pollInterval)
	defer tick.Stop()

	for !d.Processed() {
		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("waiting for part %d to process: %w", d.ID, ctx.Err())
		case <-deadline.C:
			return d, nil
		case <-tick.C:
		}

		next, err := s.drawing(ctx, d.ID)
		if err != nil {
			return nil, err
		}

		d = next
	}

	return d, nil
}

// drawingPage is one page of the account's drawings, newest first.
func (s *session) drawings(ctx context.Context, page, perPage int) ([]Drawing, int, error) {
	q := s.guestParams()
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(perPage))

	var out struct {
		Data  []Drawing `json:"data"`
		Stats struct {
			All int `json:"all"`
		} `json:"stats"`
	}

	if err := s.call(ctx, http.MethodGet, "/drawings", q, &out); err != nil {
		return nil, 0, err
	}

	return out.Data, out.Stats.All, nil
}

func (s *session) deleteDrawing(ctx context.Context, id int64) error {
	err := s.call(ctx, http.MethodDelete, fmt.Sprintf("/drawings/%d", id), nil, nil)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("part %d: %w", id, ErrNotFound)
	}

	return err
}

// Operation is a service (bending, tapping, ...) a material offers, with
// its limits for that material.
type Operation struct {
	Operation     string          `json:"operation"`
	Configuration json.RawMessage `json:"configuration"`
}

// TappingLimits are a material's tapping rules.
type TappingLimits struct {
	TapSizes []struct {
		Tap string `json:"tap"`
	} `json:"tap_sizes"`
	MinHoleToEdge Decimal `json:"min_hole_to_edge_delta"`
}

// BendingLimits are a material's bending rules. Lengths are in inches.
type BendingLimits struct {
	MinAngle        Decimal `json:"min_bend_angle"`
	MaxAngle        Decimal `json:"max_bend_angle"`
	MaxWidth        Decimal `json:"max_bend_width"`
	BendRadius      Decimal `json:"effective_bend_radius"`
	BottomDie       Decimal `json:"bottom_die"`
	MinFlangeLength Decimal `json:"min_flange_length"`
}

// operation returns the material's configuration for one service.
func (m *Material) operation(name string) (*Operation, bool) {
	for i := range m.Operations {
		if m.Operations[i].Operation == name {
			return &m.Operations[i], true
		}
	}

	return nil, false
}

// limits decodes a service's configuration into out.
func (m *Material) limits(name string, out any) error {
	op, ok := m.operation(name)
	if !ok || len(op.Configuration) == 0 || string(op.Configuration) == "null" {
		return fmt.Errorf("%s doesn't offer %s", m.SKU, name)
	}

	if err := json.Unmarshal(op.Configuration, out); err != nil {
		return fmt.Errorf("decode %s's %s limits: %w", m.SKU, name, err)
	}

	return nil
}

// Material is one stock material and thickness.
type Material struct {
	ID         int64       `json:"id"`
	SKU        string      `json:"sku"`
	Name       string      `json:"name"`
	FullName   string      `json:"fullname"`
	Category   string      `json:"category"`
	Group      string      `json:"group"`
	Form       string      `json:"form"`
	Thickness  string      `json:"thickness"`
	OutOfStock bool        `json:"out_of_stock"`
	Operations []Operation `json:"operations"`
}

// Services lists the services the material offers, without repeats.
func (m *Material) Services() []string {
	seen := map[string]bool{}

	var out []string

	for _, o := range m.Operations {
		if o.Operation != "" && !seen[o.Operation] {
			seen[o.Operation] = true
			out = append(out, o.Operation)
		}
	}

	return out
}

func (s *session) materials(ctx context.Context) ([]Material, error) {
	var out envelope[[]Material]
	if err := s.call(ctx, http.MethodGet, "/materials", s.guestParams(), &out); err != nil {
		return nil, err
	}

	return out.Data, nil
}

// TapSize is a tap and the drill size SendCutSend cuts its hole to, in
// inches.
type TapSize struct {
	Tap               string  `json:"tap"`
	Drill             float64 `json:"drill"`
	MeasurementSystem string  `json:"measurement_system"`
	Deleted           bool    `json:"deleted"`
}

func (s *session) tapSizes(ctx context.Context) (map[string]TapSize, error) {
	var out envelope[[]TapSize]
	if err := s.call(ctx, http.MethodGet, "/tap_sizes", nil, &out); err != nil {
		return nil, err
	}

	taps := make(map[string]TapSize, len(out.Data))

	for _, t := range out.Data {
		if !t.Deleted {
			taps[t.Tap] = t
		}
	}

	return taps, nil
}

// Finish is one powder coat, anodizing or plating option.
type Finish struct {
	Value      string `json:"value"`
	FinishType string `json:"finish_type"`
	Color      string `json:"color"`
	Code       string `json:"code"`
}

// finishes reads the finish options from the ordering catalog SendCutSend
// publishes for automated clients.
func (s *session) finishes(ctx context.Context) ([]Finish, error) {
	var out struct {
		FinishOptions []Finish `json:"finish_options"`
	}

	if err := s.call(ctx, http.MethodGet, "/sendcutsend-catalog.json", nil, &out); err != nil {
		return nil, err
	}

	return out.FinishOptions, nil
}

// model downloads a processed part's geometry from SendCutSend's CDN.
func (s *session) model(ctx context.Context, d *Drawing) (*Model, error) {
	if d.ModelURL == "" {
		return nil, fmt.Errorf("part %d has no processed geometry yet", d.ID)
	}

	u, err := url.Parse(d.ModelURL)
	if err != nil || u.Scheme != "https" || !s.c.modelHostAllowed(u) {
		return nil, fmt.Errorf("part %d's geometry is at an unexpected address", d.ID)
	}

	raw, err := s.fetchPublic(ctx, d.ModelURL)
	if err != nil {
		return nil, fmt.Errorf("download part %d's geometry: %w", d.ID, err)
	}

	var m Model
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode part %d's geometry: %w", d.ID, err)
	}

	return &m, nil
}

// OperationCost is what one service adds to a line.
type OperationCost struct {
	Total          Decimal `json:"total"`
	OperationCount int     `json:"operation_count"`
}

// OperationCosts maps services to their cost. The API sends 0 instead of an
// object for free services.
type OperationCosts map[string]OperationCost

// UnmarshalJSON skips the entries that aren't objects.
func (o *OperationCosts) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return fmt.Errorf("operation costs: %w", err)
	}

	*o = OperationCosts{}

	for k, v := range raw {
		var c OperationCost
		if json.Unmarshal(v, &c) == nil {
			(*o)[k] = c
		}
	}

	return nil
}

// PriceBreak is the price per part from a quantity up.
type PriceBreak struct {
	Quantity  int     `json:"quantity"`
	Discount  float64 `json:"discount"`
	UnitPrice Decimal `json:"unit_tax_basis"`
}

// Quote is the price of a part in one material.
type Quote struct {
	MaterialSKU    string         `json:"material_sku"`
	Quantity       int            `json:"quantity"`
	UnitCost       Decimal        `json:"unit_cost"`
	TotalCost      Decimal        `json:"total_cost"`
	Discount       float64        `json:"discount"`
	ReviewRequired bool           `json:"review_required"`
	Errors         []Issue        `json:"errors"`
	Warnings       []Issue        `json:"warnings"`
	Breaks         []PriceBreak   `json:"discounts"`
	OperationCosts OperationCosts `json:"operation_costs"`
}

// QuoteDates are the delivery estimates for a single-material quote.
type QuoteDates struct {
	EstShipDate *string `json:"est_ship_date"`
	InHandDate  *string `json:"in_hand_date"`
}

// quotes prices a drawing. With sku empty it prices every material, without
// delivery dates; with a SKU it prices that material only, with dates. An
// unknown SKU yields no quotes rather than an error.
func (s *session) quotes(ctx context.Context, drawingID int64, quantity int, sku string, extra map[string]any) ([]Quote, QuoteDates, error) {
	body := map[string]any{"drawing_id": drawingID, "quantity": quantity, "material_sku": nil}
	for k, v := range extra {
		body[k] = v
	}

	if sku != "" {
		body["material_sku"] = sku
	}

	if s.app.UserToken != nil {
		body["user_token"] = *s.app.UserToken
	}

	var out struct {
		Data []Quote    `json:"data"`
		Meta QuoteDates `json:"meta"`
	}

	if err := s.call(ctx, http.MethodPost, fmt.Sprintf("/drawings/%d/quotes", drawingID), body, &out); err != nil {
		return nil, QuoteDates{}, err
	}

	return out.Data, out.Meta, nil
}

// CartItem is one line in a cart.
type CartItem struct {
	ID             int64          `json:"id"`
	Quantity       int            `json:"quantity"`
	UnitCost       Decimal        `json:"unit_cost"`
	TotalCost      Decimal        `json:"total_cost"`
	CanOrder       bool           `json:"can_order"`
	ReviewRequired bool           `json:"review_required"`
	QuoteErrors    []Issue        `json:"quote_errors"`
	Operations     []string       `json:"operations"`
	OperationCosts OperationCosts `json:"operation_costs"`
	Material       struct {
		SKU string `json:"sku"`
	} `json:"material"`
	Drawing struct {
		ID               int64  `json:"id"`
		Filename         string `json:"filename"`
		CustomerFilename string `json:"customer_filename"`
	} `json:"drawing"`
}

// CartTotals is the cart's price summary.
type CartTotals struct {
	Subtotal    Decimal `json:"subtotal"`
	Shipping    Decimal `json:"shipping"`
	Tax         Decimal `json:"tax"`
	Total       Decimal `json:"total"`
	EstShipDate *string `json:"est_ship_date"`
	InHandDate  *string `json:"in_hand_date"`
}

func (s *session) cartItems(ctx context.Context) ([]CartItem, error) {
	var out envelope[[]CartItem]
	if err := s.call(ctx, http.MethodGet, "/cart", nil, &out); err != nil {
		return nil, err
	}

	return out.Data, nil
}

// addToCart adds a part to the account's cart. extra holds the services'
// fields; nil means cut only.
func (s *session) addToCart(ctx context.Context, drawingID, materialID int64, quantity int, extra map[string]any) (*CartItem, *CartTotals, error) {
	body := map[string]any{
		"drawing_id":         drawingID,
		"material_id":        materialID,
		"material":           map[string]any{"id": materialID},
		"quantity":           quantity,
		"operations":         []any{},
		"finishing_explicit": true,
	}
	for k, v := range extra {
		body[k] = v
	}

	var out struct {
		Data CartItem `json:"data"`
		Meta struct {
			Totals CartTotals `json:"totals"`
		} `json:"meta"`
	}

	if err := s.call(ctx, http.MethodPost, "/cart", body, &out); err != nil {
		return nil, nil, err
	}

	return &out.Data, &out.Meta.Totals, nil
}

func (s *session) removeFromCart(ctx context.Context, id int64) error {
	return s.call(ctx, http.MethodDelete, fmt.Sprintf("/cart/%d", id), map[string]any{"id": id}, nil)
}

// SavedCart is a cart kept on the account, which the owner can load into
// their browser's cart and check out.
type SavedCart struct {
	ID              int64   `json:"id"`
	Title           string  `json:"title"`
	ReferenceNumber string  `json:"reference_number"`
	AcceptPath      string  `json:"accept_path"`
	Created         string  `json:"created"`
	Kind            string  `json:"kind"`
	CartItemIDs     []int64 `json:"cart_item_ids"`
}

// saveCart turns this session's cart into a saved cart.
func (s *session) saveCart(ctx context.Context, title string) (*SavedCart, error) {
	var out envelope[SavedCart]

	body := map[string]any{"title": title, "request_approval": false}
	if err := s.call(ctx, http.MethodPost, "/saved_carts", body, &out); err != nil {
		return nil, err
	}

	return &out.Data, nil
}

func (s *session) savedCarts(ctx context.Context, page, perPage int) ([]SavedCart, int, error) {
	q := url.Values{}
	q.Set("page", strconv.Itoa(page))
	q.Set("per_page", strconv.Itoa(perPage))
	q.Set("resource_type", "saved_carts")

	var out struct {
		Data  []SavedCart `json:"data"`
		Stats struct {
			All int `json:"all"`
		} `json:"stats"`
	}

	if err := s.call(ctx, http.MethodGet, "/saved_carts", q, &out); err != nil {
		return nil, 0, err
	}

	return out.Data, out.Stats.All, nil
}

func (s *session) savedCart(ctx context.Context, id int64) (*SavedCart, error) {
	var out envelope[SavedCart]
	if err := s.call(ctx, http.MethodGet, fmt.Sprintf("/saved_carts/%d", id), nil, &out); err != nil {
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("saved cart %d: %w", id, ErrNotFound)
		}

		return nil, err
	}

	return &out.Data, nil
}

func (s *session) savedCartItems(ctx context.Context, id int64) ([]CartItem, error) {
	var out envelope[[]CartItem]
	if err := s.call(ctx, http.MethodGet, fmt.Sprintf("/saved_carts/%d/cart_items", id), nil, &out); err != nil {
		return nil, err
	}

	return out.Data, nil
}

func (s *session) deleteSavedCart(ctx context.Context, id int64) error {
	err := s.call(ctx, http.MethodDelete, fmt.Sprintf("/saved_carts/%d", id), map[string]any{"id": id}, nil)
	if errors.Is(err, ErrNotFound) {
		return fmt.Errorf("saved cart %d: %w", id, ErrNotFound)
	}

	return err
}
