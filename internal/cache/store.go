// Package cache gives servers a response cache and request budgets that work
// in memory or in a shared Redis-protocol store.
//
// The Redis backend uses standard commands only (GET, SET with PX, INCRBY,
// EXPIREAT, MULTI/EXEC), so any Redis-compatible server works. On a server
// shared with other services, give each its own ACL user limited to its key
// prefix; see docs/server-contract.md.
package cache

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru/v2"
	"github.com/redis/go-redis/v9"
)

// Store keeps values with an expiry, and counters.
type Store interface {
	// Get returns the value at key; ok is false when it's missing or expired.
	Get(ctx context.Context, key string) (value []byte, ok bool, err error)
	// Set stores value at key for ttl.
	Set(ctx context.Context, key string, value []byte, ttl time.Duration) error
	// Incr adds delta to the counter at key, which expires at expireAt, and
	// returns its new value.
	Incr(ctx context.Context, key string, delta int64, expireAt time.Time) (int64, error)
	// Count returns the counter at key, 0 when it's missing.
	Count(ctx context.Context, key string) (int64, error)
	// Kind names the backend for status output: "memory" or "redis".
	Kind() string
	Close() error
}

// MemoryStore keeps values in this process, up to a number of entries.
type MemoryStore struct {
	now    func() time.Time
	values *lru.Cache[string, memEntry]

	mu       sync.Mutex
	counters map[string]memCounter
}

type memEntry struct {
	value   []byte
	expires time.Time
}

type memCounter struct {
	n       int64
	expires time.Time
}

// NewMemoryStore returns a store holding at most size values. now is the
// clock; nil uses time.Now.
func NewMemoryStore(size int, now func() time.Time) *MemoryStore {
	if now == nil {
		now = time.Now
	}

	values, err := lru.New[string, memEntry](max(1, size))
	if err != nil {
		// lru.New fails only for a size below 1, which max rules out.
		panic(err)
	}

	return &MemoryStore{now: now, values: values, counters: map[string]memCounter{}}
}

func (m *MemoryStore) Get(_ context.Context, key string) ([]byte, bool, error) {
	e, ok := m.values.Get(key)
	if !ok {
		return nil, false, nil
	}

	if !m.now().Before(e.expires) {
		m.values.Remove(key)

		return nil, false, nil
	}

	return e.value, true, nil
}

func (m *MemoryStore) Set(_ context.Context, key string, value []byte, ttl time.Duration) error {
	m.values.Add(key, memEntry{value: value, expires: m.now().Add(ttl)})

	return nil
}

func (m *MemoryStore) Incr(_ context.Context, key string, delta int64, expireAt time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()

	c := m.counters[key]
	if !now.Before(c.expires) {
		c = memCounter{}
	}

	c.n += delta
	c.expires = expireAt
	m.counters[key] = c

	// Drop expired counters now and then; there are only a few per day.
	if len(m.counters) > 64 {
		for k, v := range m.counters {
			if !now.Before(v.expires) {
				delete(m.counters, k)
			}
		}
	}

	return c.n, nil
}

func (m *MemoryStore) Count(_ context.Context, key string) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	c, ok := m.counters[key]
	if !ok || !m.now().Before(c.expires) {
		return 0, nil
	}

	return c.n, nil
}

func (m *MemoryStore) Kind() string { return "memory" }

func (m *MemoryStore) Close() error { return nil }

// Len returns the number of values held, expired ones included until they
// are next read or pushed out.
func (m *MemoryStore) Len() int { return m.values.Len() }

// RedisStore keeps values in a Redis-protocol server.
type RedisStore struct {
	client *redis.Client
}

// NewRedisStore connects to the server at rawURL (redis:// or rediss://,
// with credentials if needed). It doesn't contact the server until first
// use, so a server that is down at start-up only makes calls miss.
func NewRedisStore(rawURL string) (*RedisStore, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		// The parse error can quote the URL, password included.
		return nil, errors.New("the cache URL isn't a valid redis:// or rediss:// URL")
	}

	opts.DialTimeout = 2 * time.Second
	opts.ReadTimeout = time.Second
	opts.WriteTimeout = time.Second
	// The client otherwise sends CLIENT SETINFO, which a locked-down ACL
	// user may not be allowed.
	opts.DisableIdentity = true

	return &RedisStore{client: redis.NewClient(opts)}, nil
}

func (r *RedisStore) Get(ctx context.Context, key string) ([]byte, bool, error) {
	v, err := r.client.Get(ctx, key).Bytes()
	if errors.Is(err, redis.Nil) {
		return nil, false, nil
	}

	if err != nil {
		return nil, false, fmt.Errorf("cache get: %w", err)
	}

	return v, true, nil
}

func (r *RedisStore) Set(ctx context.Context, key string, value []byte, ttl time.Duration) error {
	if err := r.client.Set(ctx, key, value, ttl).Err(); err != nil {
		return fmt.Errorf("cache set: %w", err)
	}

	return nil
}

func (r *RedisStore) Incr(ctx context.Context, key string, delta int64, expireAt time.Time) (int64, error) {
	var incr *redis.IntCmd

	_, err := r.client.TxPipelined(ctx, func(p redis.Pipeliner) error {
		incr = p.IncrBy(ctx, key, delta)
		p.ExpireAt(ctx, key, expireAt)

		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("cache counter: %w", err)
	}

	return incr.Val(), nil
}

func (r *RedisStore) Count(ctx context.Context, key string) (int64, error) {
	v, err := r.client.Get(ctx, key).Result()
	if errors.Is(err, redis.Nil) {
		return 0, nil
	}

	if err != nil {
		return 0, fmt.Errorf("cache counter: %w", err)
	}

	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("cache counter %q isn't a number", key)
	}

	return n, nil
}

func (r *RedisStore) Kind() string { return "redis" }

func (r *RedisStore) Close() error { return r.client.Close() }
