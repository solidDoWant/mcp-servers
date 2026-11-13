package cache

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"golang.org/x/sync/singleflight"
)

// DefaultMaxValueBytes is the largest value the cache keeps.
const DefaultMaxValueBytes = 1 << 20

// Cache keeps fetched values for a while, and keeps them a while longer to
// answer with when a fresh fetch fails.
type Cache struct {
	store    Store
	prefix   string
	staleFor time.Duration
	maxValue int
	now      func() time.Time
	log      *slog.Logger
	flight   singleflight.Group
}

// Options configures a Cache.
type Options struct {
	// Store holds the values.
	Store Store
	// Prefix starts every key. On a shared server it is what the service's
	// ACL user is limited to, e.g. "electronic-components".
	Prefix string
	// StaleFor is how long past its lifetime an entry is kept to answer with
	// when a fetch fails. 0 keeps none.
	StaleFor time.Duration
	// MaxValueBytes skips caching larger values; 0 uses the default.
	MaxValueBytes int
	// Now is the clock; nil uses time.Now.
	Now func() time.Time
	// Logger receives store failures; nil discards them.
	Logger *slog.Logger
}

// New returns a cache over opts.Store.
func New(opts Options) *Cache {
	c := &Cache{
		store: opts.Store, prefix: opts.Prefix, staleFor: opts.StaleFor,
		maxValue: opts.MaxValueBytes, now: opts.Now, log: opts.Logger,
	}

	if c.maxValue <= 0 {
		c.maxValue = DefaultMaxValueBytes
	}

	if c.now == nil {
		c.now = time.Now
	}

	if c.log == nil {
		c.log = slog.New(slog.DiscardHandler)
	}

	return c
}

// Store returns the cache's store.
func (c *Cache) Store() Store { return c.store }

// Meta says where a value came from.
type Meta struct {
	// Hit is true when the value came from the cache rather than a fetch.
	Hit bool
	// Stale is true when the value is past its lifetime, served because the
	// fetch failed with FetchErr.
	Stale    bool
	FetchErr error
	// Age is how long ago the value was fetched.
	Age time.Duration
}

type entry struct {
	Stored     int64           `json:"s"`
	FreshUntil int64           `json:"f"`
	Value      json.RawMessage `json:"v"`
}

// Key builds a store key from parts. Each part is length-prefixed, so no
// two lists of parts make the same key, and the result is hashed, so the
// store never holds the parts themselves (queries, part numbers, user IDs).
func (c *Cache) Key(parts ...string) string {
	h := sha256.New()

	var n [binary.MaxVarintLen64]byte

	for _, p := range parts {
		h.Write(n[:binary.PutUvarint(n[:], uint64(len(p)))])
		h.Write([]byte(p))
	}

	return c.prefix + ":c:" + hex.EncodeToString(h.Sum(nil))
}

// Fetch returns the value for key: from the cache while fresh, otherwise
// from fetch, which runs once for concurrent callers of the same key. ttl
// says how long a fetched value stays fresh; 0 doesn't cache it. When fetch
// fails and a stale value is still kept, Fetch returns that instead, with
// Meta.Stale set. Store failures only make Fetch miss.
func Fetch[T any](ctx context.Context, c *Cache, key string, ttl func(T) time.Duration, fetch func(context.Context) (T, error)) (T, Meta, error) {
	var zero T

	now := c.now()

	cached, found := c.read(ctx, key)
	if found && now.UnixNano() < cached.FreshUntil {
		var v T
		if err := json.Unmarshal(cached.Value, &v); err == nil {
			return v, Meta{Hit: true, Age: now.Sub(time.Unix(0, cached.Stored))}, nil
		}
	}

	res, err, _ := c.flight.Do(key, func() (any, error) {
		// A caller that gives up shouldn't cancel the fetch for the others.
		v, err := fetch(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}

		c.write(ctx, key, v, ttl(v))

		return v, nil
	})
	if err == nil {
		return res.(T), Meta{}, nil
	}

	if found {
		var v T
		if json.Unmarshal(cached.Value, &v) == nil {
			return v, Meta{Hit: true, Stale: true, FetchErr: err, Age: now.Sub(time.Unix(0, cached.Stored))}, nil
		}
	}

	return zero, Meta{}, err
}

func (c *Cache) read(ctx context.Context, key string) (entry, bool) {
	raw, ok, err := c.store.Get(ctx, key)
	if err != nil {
		c.log.Warn("cache read failed", "error", err)

		return entry{}, false
	}

	if !ok {
		return entry{}, false
	}

	var e entry
	if err := json.Unmarshal(raw, &e); err != nil {
		return entry{}, false
	}

	return e, true
}

func (c *Cache) write(ctx context.Context, key string, v any, ttl time.Duration) {
	if ttl <= 0 {
		return
	}

	value, err := json.Marshal(v)
	if err != nil {
		c.log.Warn("cache encode failed", "error", err)

		return
	}

	now := c.now()

	raw, err := json.Marshal(entry{Stored: now.UnixNano(), FreshUntil: now.Add(ttl).UnixNano(), Value: value})
	if err != nil || len(raw) > c.maxValue {
		return
	}

	// Write with the caller's values but not its cancellation: the fetch
	// already succeeded.
	if err := c.store.Set(context.WithoutCancel(ctx), key, raw, ttl+c.staleFor); err != nil {
		c.log.Warn("cache write failed", "error", err)
	}
}

// ErrBudget means a request budget is used up for now.
var ErrBudget = errors.New("request budget used up")
