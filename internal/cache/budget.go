package cache

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"golang.org/x/time/rate"
)

// Budget limits calls to one upstream: a steady rate, enforced in this
// process, and a total per UTC day, counted in the store so that every
// replica and every restart shares it. A call that would wait longer than
// MaxWait for the rate, or comes once the day's total is spent, is refused
// rather than left hanging.
type Budget struct {
	name     string
	key      string
	limiter  *rate.Limiter
	cfg      BudgetConfig
	store    Store
	fallback *MemoryStore
	now      func() time.Time
	log      *slog.Logger
}

// BudgetConfig sizes a Budget.
type BudgetConfig struct {
	PerMinute float64       // 0 for no rate limit
	Daily     int           // 0 for no daily limit
	MaxWait   time.Duration // 0 waits as long as the rate needs
}

// NewBudget returns a budget for the upstream name, counted in c's store
// under c's prefix.
func (c *Cache) NewBudget(name string, cfg BudgetConfig) *Budget {
	limit, burst := rate.Inf, 1
	if cfg.PerMinute > 0 {
		limit = rate.Limit(cfg.PerMinute / 60)
		burst = max(1, int(cfg.PerMinute/10))
	}

	return &Budget{
		name: name, key: c.prefix + ":budget:" + name, cfg: cfg, store: c.store,
		limiter:  rate.NewLimiter(limit, burst),
		fallback: NewMemoryStore(1, c.now), now: c.now, log: c.log,
	}
}

func (b *Budget) dayKey(now time.Time) (string, time.Time) {
	day := now.UTC().Truncate(24 * time.Hour)

	// Keep the counter an hour past the day, so a slightly fast clock
	// elsewhere can't restart it early.
	return b.key + ":" + day.Format(time.DateOnly), day.Add(25 * time.Hour)
}

func (b *Budget) incr(ctx context.Context, delta int64) (int64, error) {
	key, expires := b.dayKey(b.now())

	n, err := b.store.Incr(ctx, key, delta, expires)
	if err != nil {
		// Count here rather than refuse calls while the store is down.
		b.log.Warn("budget counter unavailable; counting in this process", "budget", b.name, "error", err)

		return b.fallback.Incr(ctx, key, delta, expires)
	}

	return n, nil
}

// Take claims one call. It waits for the rate through wait, and fails
// without waiting when the day's total is spent or the wait would be too
// long.
func (b *Budget) Take(ctx context.Context, wait func(time.Duration) error) error {
	r := b.limiter.ReserveN(b.now(), 1)
	delay := r.DelayFrom(b.now())

	if !r.OK() || (b.cfg.MaxWait > 0 && delay > b.cfg.MaxWait) {
		r.CancelAt(b.now())

		return fmt.Errorf("%w: %s is being called too often; try again in a minute", ErrBudget, b.name)
	}

	if b.cfg.Daily > 0 {
		n, err := b.incr(ctx, 1)
		if err == nil && n > int64(b.cfg.Daily) {
			_, _ = b.incr(ctx, -1)

			r.CancelAt(b.now())

			return fmt.Errorf("%w: %s's daily limit of %d calls is spent; it resets at 00:00 UTC", ErrBudget, b.name, b.cfg.Daily)
		}
	}

	if delay > 0 {
		return wait(delay)
	}

	return nil
}

// Status reports the day's use and the daily limit.
func (b *Budget) Status(ctx context.Context) (used, daily int) {
	key, _ := b.dayKey(b.now())

	n, err := b.store.Count(ctx, key)
	if err != nil {
		n, _ = b.fallback.Count(ctx, key)
	}

	return int(n), b.cfg.Daily
}
