package cache

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// clock is a controllable time source. advance also moves miniredis's
// clock, so stored TTLs expire with it.
type clock struct {
	mu  sync.Mutex
	t   time.Time
	mrs *miniredis.Miniredis
}

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.t
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()

	if c.mrs != nil {
		c.mrs.FastForward(d)
	}
}

type backend struct {
	name string
	// open returns a store and the clock it shares. Calling it twice in one
	// test gives two stores over the same data, like two replicas.
	open func(t *testing.T) (func() Store, *clock)
}

func backends() []backend {
	start := time.Date(2026, 11, 13, 22, 0, 0, 0, time.UTC)

	return []backend{
		{"memory", func(t *testing.T) (func() Store, *clock) {
			t.Helper()

			c := &clock{t: start}
			s := NewMemoryStore(100, c.now)

			return func() Store { return s }, c
		}},
		{"redis", func(t *testing.T) (func() Store, *clock) {
			t.Helper()

			m := miniredis.RunT(t)
			m.SetTime(start)
			c := &clock{t: start, mrs: m}

			return func() Store {
				s, err := NewRedisStore("redis://" + m.Addr())
				require.NoError(t, err)
				t.Cleanup(func() { _ = s.Close() })

				return s
			}, c
		}},
	}
}

func hour(string) time.Duration { return time.Hour }

// counter is a fetch that counts its calls and returns what it's told.
type counter struct {
	calls int
	value string
	err   error
}

func (f *counter) fetch(context.Context) (string, error) {
	f.calls++

	return f.value, f.err
}

func TestFetch(t *testing.T) {
	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			t.Run("answers from the cache until the value's lifetime ends", func(t *testing.T) {
				open, clk := b.open(t)
				c := New(Options{Store: open(), Prefix: "svc", Now: clk.now})
				f := &counter{value: "v1"}

				v, meta, err := Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				require.NoError(t, err)
				assert.Equal(t, "v1", v)
				assert.False(t, meta.Hit)

				clk.advance(59 * time.Minute)

				f.value = "v2"

				v, meta, err = Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				require.NoError(t, err)
				assert.Equal(t, "v1", v)
				assert.Equal(t, Meta{Hit: true, Age: 59 * time.Minute}, meta)
				assert.Equal(t, 1, f.calls)

				clk.advance(2 * time.Minute)

				v, meta, err = Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				require.NoError(t, err)
				assert.Equal(t, "v2", v)
				assert.False(t, meta.Hit)
				assert.Equal(t, 2, f.calls)
			})

			t.Run("answers with a stale value, saying so, when a fetch fails", func(t *testing.T) {
				open, clk := b.open(t)
				c := New(Options{Store: open(), Prefix: "svc", StaleFor: 24 * time.Hour, Now: clk.now})
				f := &counter{value: "v1"}

				_, _, err := Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				require.NoError(t, err)

				clk.advance(3 * time.Hour)

				f.err = errors.New("upstream is down")

				v, meta, err := Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				require.NoError(t, err)
				assert.Equal(t, "v1", v)
				assert.True(t, meta.Stale)
				assert.Equal(t, 3*time.Hour, meta.Age)
				require.EqualError(t, meta.FetchErr, "upstream is down")

				clk.advance(23 * time.Hour)

				_, _, err = Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				require.EqualError(t, err, "upstream is down", "past the stale window the failure shows")
			})

			t.Run("doesn't cache failures or values whose lifetime is zero", func(t *testing.T) {
				open, clk := b.open(t)
				c := New(Options{Store: open(), Prefix: "svc", Now: clk.now})
				f := &counter{err: errors.New("boom")}

				_, _, err := Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				require.Error(t, err)

				f.err, f.value = nil, "ok"

				_, _, err = Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				require.NoError(t, err)
				assert.Equal(t, 2, f.calls)

				never := func(string) time.Duration { return 0 }
				_, _, _ = Fetch(t.Context(), c, c.Key("z"), never, f.fetch)
				_, meta, _ := Fetch(t.Context(), c, c.Key("z"), never, f.fetch)
				assert.False(t, meta.Hit)
			})

			t.Run("skips values over the size limit", func(t *testing.T) {
				open, clk := b.open(t)
				c := New(Options{Store: open(), Prefix: "svc", MaxValueBytes: 64, Now: clk.now})
				f := &counter{value: strings.Repeat("x", 100)}

				_, _, _ = Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
				_, meta, _ := Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)

				assert.False(t, meta.Hit)
				assert.Equal(t, 2, f.calls)
			})

			t.Run("shares values between instances over the same store", func(t *testing.T) {
				open, clk := b.open(t)
				if b.name == "memory" {
					t.Skip("a memory store belongs to one process")
				}

				first := New(Options{Store: open(), Prefix: "svc", Now: clk.now})
				second := New(Options{Store: open(), Prefix: "svc", Now: clk.now})
				f := &counter{value: "v1"}

				_, _, _ = Fetch(t.Context(), first, first.Key("q"), hour, f.fetch)
				v, meta, err := Fetch(t.Context(), second, second.Key("q"), hour, f.fetch)

				require.NoError(t, err)
				assert.Equal(t, "v1", v)
				assert.True(t, meta.Hit)
				assert.Equal(t, 1, f.calls)
			})
		})
	}
}

// brokenStore fails every call, like a server that is down.
type brokenStore struct{}

var errDown = errors.New("connection refused")

func (brokenStore) Get(context.Context, string) ([]byte, bool, error) { return nil, false, errDown }

func (brokenStore) Set(context.Context, string, []byte, time.Duration) error { return errDown }

func (brokenStore) Incr(context.Context, string, int64, time.Time) (int64, error) { return 0, errDown }

func (brokenStore) Count(context.Context, string) (int64, error) { return 0, errDown }

func (brokenStore) Kind() string { return "broken" }

func (brokenStore) Close() error { return nil }

func TestAStoreThatIsDownOnlyMakesCallsMiss(t *testing.T) {
	c := New(Options{Store: brokenStore{}, Prefix: "svc"})
	f := &counter{value: "v"}

	for range 2 {
		v, meta, err := Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
		require.NoError(t, err)
		assert.Equal(t, "v", v)
		assert.False(t, meta.Hit)
	}

	assert.Equal(t, 2, f.calls)
}

func TestKeys(t *testing.T) {
	c := New(Options{Store: NewMemoryStore(1, nil), Prefix: "svc"})

	t.Run("start with the prefix an ACL user can be limited to", func(t *testing.T) {
		assert.True(t, strings.HasPrefix(c.Key("q"), "svc:c:"))
	})

	t.Run("never collide when parts shift between positions", func(t *testing.T) {
		assert.NotEqual(t, c.Key("a", "bc"), c.Key("ab", "c"))
		assert.NotEqual(t, c.Key("a", ""), c.Key("a"))
	})

	t.Run("don't contain the parts themselves", func(t *testing.T) {
		assert.NotContains(t, c.Key("secret query"), "secret")
	})
}

func TestBudget(t *testing.T) {
	noWait := func(time.Duration) error { return nil }

	for _, b := range backends() {
		t.Run(b.name, func(t *testing.T) {
			t.Run("refuses calls past the daily limit without counting them, until midnight UTC", func(t *testing.T) {
				open, clk := b.open(t)
				c := New(Options{Store: open(), Prefix: "svc", Now: clk.now})
				bud := c.NewBudget("API", BudgetConfig{Daily: 2})

				require.NoError(t, bud.Take(t.Context(), noWait))
				require.NoError(t, bud.Take(t.Context(), noWait))

				err := bud.Take(t.Context(), noWait)
				require.ErrorIs(t, err, ErrBudget)
				assert.Contains(t, err.Error(), "API's daily limit of 2 calls is spent")

				used, daily := bud.Status(t.Context())
				assert.Equal(t, 2, used, "the refused call isn't counted")
				assert.Equal(t, 2, daily)

				clk.advance(2 * time.Hour) // 22:00 → 00:00 the next day

				require.NoError(t, bud.Take(t.Context(), noWait))
			})

			t.Run("shares the day's count between replicas and restarts", func(t *testing.T) {
				open, clk := b.open(t)
				if b.name == "memory" {
					t.Skip("a memory store belongs to one process")
				}

				first := New(Options{Store: open(), Prefix: "svc", Now: clk.now}).NewBudget("API", BudgetConfig{Daily: 2})
				second := New(Options{Store: open(), Prefix: "svc", Now: clk.now}).NewBudget("API", BudgetConfig{Daily: 2})

				require.NoError(t, first.Take(t.Context(), noWait))
				require.NoError(t, second.Take(t.Context(), noWait))
				require.ErrorIs(t, first.Take(t.Context(), noWait), ErrBudget)
			})

			t.Run("refuses a call that would wait longer than allowed for the rate", func(t *testing.T) {
				open, clk := b.open(t)
				bud := New(Options{Store: open(), Prefix: "svc", Now: clk.now}).NewBudget("API", BudgetConfig{PerMinute: 6, MaxWait: time.Second})

				require.NoError(t, bud.Take(t.Context(), noWait))
				require.ErrorIs(t, bud.Take(t.Context(), noWait), ErrBudget)
			})

			t.Run("waits for the rate when the wait is short", func(t *testing.T) {
				open, clk := b.open(t)
				bud := New(Options{Store: open(), Prefix: "svc", Now: clk.now}).NewBudget("API", BudgetConfig{PerMinute: 6, MaxWait: 15 * time.Second})

				var waited time.Duration

				require.NoError(t, bud.Take(t.Context(), noWait))
				require.NoError(t, bud.Take(t.Context(), func(d time.Duration) error { waited = d; return nil }))
				assert.Equal(t, 10*time.Second, waited)
			})
		})
	}
}

func TestABudgetCountsInProcessWhileTheStoreIsDown(t *testing.T) {
	bud := New(Options{Store: brokenStore{}, Prefix: "svc"}).NewBudget("API", BudgetConfig{Daily: 1})
	noWait := func(time.Duration) error { return nil }

	require.NoError(t, bud.Take(t.Context(), noWait))
	require.ErrorIs(t, bud.Take(t.Context(), noWait), ErrBudget)

	used, _ := bud.Status(t.Context())
	assert.Equal(t, 1, used)
}

func TestARedisURLThatIsInvalidIsRefusedWithoutQuotingIt(t *testing.T) {
	_, err := NewRedisStore("http://user:hunter2@example")

	require.Error(t, err)
	assert.NotContains(t, err.Error(), "hunter2")
}
