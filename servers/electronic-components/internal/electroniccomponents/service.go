package electroniccomponents

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/solidDoWant/mcp-servers/internal/cache"
)

// Config configures the server. A source is enabled when its credentials
// are set; jlcsearch needs none and is enabled unless JLCSearchDisabled.
type Config struct {
	Version string

	DigiKey           DigiKeyConfig
	MouserURL         string
	MouserAPIKey      string
	JLCSearchURL      string
	JLCSearchDisabled bool

	// CacheStore holds cached answers and budget counts; nil keeps them in
	// memory, holding at most CacheSize answers.
	CacheStore cache.Store
	CacheSize  int
	// CacheTTL is how long answers stay fresh; CacheStaleFor is how much
	// longer they're kept to answer with when a source fails.
	CacheTTL      time.Duration
	CacheStaleFor time.Duration
	// CachePrefix starts every key in the store; on a shared server it's
	// what this server's ACL user is limited to.
	CachePrefix string
	// Logger receives cache store failures; nil discards them.
	Logger *slog.Logger

	DigiKeyBudget BudgetConfig
	MouserBudget  BudgetConfig
	JLCBudget     BudgetConfig

	// HTTPClient sends upstream requests; nil uses a default one.
	HTTPClient *http.Client
	// Now and Wait stand in for the clock in tests; nil uses the real ones.
	Now  func() time.Time
	Wait func(context.Context, time.Duration) error
}

// BudgetConfig sizes a source's request budget.
type BudgetConfig = cache.BudgetConfig

// Defaults stay below the distributors' published limits: DigiKey allows
// 120 calls a minute and 1000 a day, Mouser 30 a minute and 1000 a day.
var (
	DefaultDigiKeyBudget = BudgetConfig{PerMinute: 100, Daily: 950, MaxWait: 10 * time.Second}
	DefaultMouserBudget  = BudgetConfig{PerMinute: 25, Daily: 950, MaxWait: 10 * time.Second}
	DefaultJLCBudget     = BudgetConfig{PerMinute: 120, MaxWait: 10 * time.Second}
)

// sourceOrder is the order sources are listed and searched in.
var sourceOrder = []string{"digikey", "mouser", "jlcpcb"}

// Service searches the enabled sources through the cache.
type Service struct {
	providers map[string]Provider
	budgets   map[string]*cache.Budget
	disabled  map[string]string
	cache     *cache.Cache
	cacheTTL  time.Duration
	staleFor  time.Duration
}

// cacheVersion is part of every cache key. Bump it when the shape of a
// cached Offer changes, so old entries are never decoded into the new one.
const cacheVersion = "1"

// notFoundTTL is how long an answer with no offers stays fresh: long enough
// to stop repeated misses spending budget, short enough for new parts.
const notFoundTTL = 10 * time.Minute

// New builds the service from cfg.
func New(cfg Config) *Service {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}

	if cfg.Wait == nil {
		cfg.Wait = sleep
	}

	if cfg.CacheSize <= 0 {
		cfg.CacheSize = 1000
	}

	if cfg.CacheTTL <= 0 {
		cfg.CacheTTL = time.Hour
	}

	if cfg.CacheStaleFor < 0 {
		cfg.CacheStaleFor = 0
	}

	if cfg.CacheStore == nil {
		cfg.CacheStore = cache.NewMemoryStore(cfg.CacheSize, cfg.Now)
	}

	c := cache.New(cache.Options{
		Store: cfg.CacheStore, Prefix: orString(cfg.CachePrefix, "electronic-components"),
		StaleFor: cfg.CacheStaleFor, Now: cfg.Now, Logger: cfg.Logger,
	})

	base := &http.Client{
		Timeout: cfg.HTTPClient.Timeout,
		Transport: &userAgentTransport{
			base: transportOf(cfg.HTTPClient),
			ua:   "mcp-servers-electronic-components/" + cfg.Version + " (+https://github.com/solidDoWant/mcp-servers)",
		},
	}

	s := &Service{
		providers: map[string]Provider{},
		budgets:   map[string]*cache.Budget{},
		disabled:  map[string]string{},
		cache:     c,
		cacheTTL:  cfg.CacheTTL,
		staleFor:  cfg.CacheStaleFor,
	}

	if cfg.DigiKey.ClientID != "" && cfg.DigiKey.ClientSecret != "" {
		b := c.NewBudget("DigiKey", orDefault(cfg.DigiKeyBudget, DefaultDigiKeyBudget))
		s.add(newDigiKey(withDigiKeyDefaults(cfg.DigiKey), base, b, cfg.Wait), b)
	} else {
		s.disabled["digikey"] = "no DigiKey client ID and secret are configured"
	}

	if cfg.MouserAPIKey != "" {
		b := c.NewBudget("Mouser", orDefault(cfg.MouserBudget, DefaultMouserBudget))
		s.add(&Mouser{base: orString(cfg.MouserURL, DefaultMouserURL), apiKey: cfg.MouserAPIKey, client: base, budget: b, wait: cfg.Wait}, b)
	} else {
		s.disabled["mouser"] = "no Mouser Search API key is configured"
	}

	if !cfg.JLCSearchDisabled {
		b := c.NewBudget("jlcsearch", orDefault(cfg.JLCBudget, DefaultJLCBudget))
		s.add(&JLCSearch{base: orString(cfg.JLCSearchURL, DefaultJLCSearchURL), client: base, budget: b, wait: cfg.Wait}, b)
	} else {
		s.disabled["jlcpcb"] = "jlcsearch is turned off"
	}

	return s
}

func (s *Service) add(p Provider, b *cache.Budget) {
	s.providers[p.Name()] = p
	s.budgets[p.Name()] = b
}

func transportOf(c *http.Client) http.RoundTripper {
	if c.Transport != nil {
		return c.Transport
	}

	return http.DefaultTransport
}

func orDefault(b, def BudgetConfig) BudgetConfig {
	if b == (BudgetConfig{}) {
		return def
	}

	return b
}

func orString(s, def string) string {
	if s == "" {
		return def
	}

	return s
}

func withDigiKeyDefaults(c DigiKeyConfig) DigiKeyConfig {
	c.BaseURL = orString(c.BaseURL, DefaultDigiKeyURL)
	c.Site = orString(c.Site, "US")
	c.Currency = orString(c.Currency, "USD")

	return c
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// Enabled lists the enabled sources in order.
func (s *Service) Enabled() []string {
	var out []string

	for _, n := range sourceOrder {
		if _, ok := s.providers[n]; ok {
			out = append(out, n)
		}
	}

	return out
}

// SourceError is one source's failure.
type SourceError struct {
	Source string `json:"source"`
	Error  string `json:"error"`
}

// result is one source's answer.
type result struct {
	source string
	offers []Offer
	meta   cache.Meta
	err    error
}

// resolve checks the requested sources, defaulting to every enabled one.
func (s *Service) resolve(requested []string) ([]string, error) {
	if len(requested) == 0 {
		names := s.Enabled()
		if len(names) == 0 {
			return nil, errors.New("no source is enabled; configure DigiKey or Mouser credentials, or enable jlcsearch")
		}

		return names, nil
	}

	var names []string

	for _, r := range requested {
		n := strings.ToLower(strings.TrimSpace(r))

		switch {
		case !slices.Contains(sourceOrder, n):
			return nil, fmt.Errorf("unknown source %q; sources are %s", r, strings.Join(sourceOrder, ", "))
		case s.providers[n] == nil:
			return nil, fmt.Errorf("source %s isn't available: %s", n, s.disabled[n])
		case !slices.Contains(names, n):
			names = append(names, n)
		}
	}

	return names, nil
}

// fanOut runs call on each source at once, through the cache.
func (s *Service) fanOut(ctx context.Context, names []string, key func(string) string, call func(context.Context, Provider) ([]Offer, error)) []result {
	results := make([]result, len(names))

	var wg sync.WaitGroup

	for i, n := range names {
		wg.Go(func() {
			p := s.providers[n]
			offers, meta, err := cache.Fetch(ctx, s.cache, key(n), s.ttl, func(ctx context.Context) ([]Offer, error) { return call(ctx, p) })
			results[i] = result{source: n, offers: offers, meta: meta, err: err}
		})
	}

	wg.Wait()

	return results
}

// ttl is how long an answer stays fresh.
func (s *Service) ttl(offers []Offer) time.Duration {
	if len(offers) == 0 {
		return min(notFoundTTL, s.cacheTTL)
	}

	return s.cacheTTL
}

// Search runs q on the named sources.
func (s *Service) Search(ctx context.Context, names []string, q SearchQuery) []result {
	return s.fanOut(ctx, names,
		func(n string) string {
			return s.cache.Key(cacheVersion, n, "search", strings.ToLower(strings.Join(strings.Fields(q.Keyword), " ")),
				strconv.Itoa(q.Limit), strconv.FormatBool(q.InStockOnly), strconv.FormatBool(q.JLCBasicOnly))
		},
		func(ctx context.Context, p Provider) ([]Offer, error) { return p.Search(ctx, q) })
}

// Lookup finds partNumber on the named sources.
func (s *Service) Lookup(ctx context.Context, names []string, partNumber string) []result {
	return s.fanOut(ctx, names,
		func(n string) string { return s.cache.Key(cacheVersion, n, "lookup", mpnKey(partNumber)) },
		func(ctx context.Context, p Provider) ([]Offer, error) { return p.Lookup(ctx, partNumber) })
}
