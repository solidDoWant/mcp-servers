package electroniccomponents

import (
	"context"
	"strings"
	"time"

	"github.com/hashicorp/golang-lru/v2/expirable"
	"golang.org/x/sync/singleflight"
)

// cache holds successful provider responses for a while, so repeated
// searches don't spend the distributors' daily budgets. Identical calls in
// flight at the same time share one upstream request. Failures aren't
// cached.
type cache struct {
	lru    *expirable.LRU[string, []Offer]
	flight singleflight.Group
}

func newCache(size int, ttl time.Duration) *cache {
	return &cache{lru: expirable.NewLRU[string, []Offer](size, nil, ttl)}
}

// key joins a call's parts with a separator no input can contain unescaped.
func cacheKey(parts ...string) string {
	escaped := make([]string, len(parts))
	for i, p := range parts {
		escaped[i] = strings.NewReplacer(`\`, `\\`, "\x00", `\0`).Replace(p)
	}

	return strings.Join(escaped, "\x00")
}

// get returns the cached offers for key, or calls fetch and caches its
// result. hit reports whether the result came from the cache.
func (c *cache) get(ctx context.Context, key string, fetch func(context.Context) ([]Offer, error)) (offers []Offer, hit bool, err error) {
	if v, ok := c.lru.Get(key); ok {
		return v, true, nil
	}

	v, err, _ := c.flight.Do(key, func() (any, error) {
		// A caller that gives up shouldn't cancel the call for others
		// waiting on it.
		res, err := fetch(context.WithoutCancel(ctx))
		if err != nil {
			return nil, err
		}

		c.lru.Add(key, res)

		return res, nil
	})
	if err != nil {
		return nil, false, err
	}

	return v.([]Offer), false, nil
}

func (c *cache) len() int { return c.lru.Len() }
