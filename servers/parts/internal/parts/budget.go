package parts

import (
	"fmt"
	"sync"
	"time"

	"golang.org/x/time/rate"
)

// Budget limits one source's upstream calls: a steady rate, and a total per
// UTC day. A call that would wait longer than maxWait for the rate is
// refused rather than left hanging, and so is any call once the day's total
// is spent.
type Budget struct {
	source  string
	limiter *rate.Limiter
	daily   int
	maxWait time.Duration
	now     func() time.Time

	mu   sync.Mutex
	day  string
	used int
}

// BudgetConfig sizes a Budget.
type BudgetConfig struct {
	PerMinute float64
	Daily     int // 0 for no daily limit
	MaxWait   time.Duration
}

// NewBudget returns a budget for source. now is the clock; nil uses
// time.Now.
func NewBudget(source string, cfg BudgetConfig, now func() time.Time) *Budget {
	if now == nil {
		now = time.Now
	}

	limit := rate.Inf
	burst := 1

	if cfg.PerMinute > 0 {
		limit = rate.Limit(cfg.PerMinute / 60)
		burst = max(1, int(cfg.PerMinute/10))
	}

	return &Budget{
		source:  source,
		limiter: rate.NewLimiter(limit, burst),
		daily:   cfg.Daily,
		maxWait: cfg.MaxWait,
		now:     now,
	}
}

// Take claims one call, waiting for the rate if needed. It fails without
// waiting when the day's total is spent or the wait would be too long.
func (b *Budget) Take(wait func(time.Duration) error) error {
	b.mu.Lock()

	today := b.now().UTC().Format(time.DateOnly)
	if today != b.day {
		b.day, b.used = today, 0
	}

	if b.daily > 0 && b.used >= b.daily {
		b.mu.Unlock()

		return fmt.Errorf("%w: %s's daily limit of %d calls is spent; it resets at 00:00 UTC", ErrBudget, b.source, b.daily)
	}

	r := b.limiter.ReserveN(b.now(), 1)
	delay := r.DelayFrom(b.now())

	if !r.OK() || (b.maxWait > 0 && delay > b.maxWait) {
		r.CancelAt(b.now())
		b.mu.Unlock()

		return fmt.Errorf("%w: %s is being called too often; try again in a minute", ErrBudget, b.source)
	}

	b.used++
	b.mu.Unlock()

	if delay > 0 {
		return wait(delay)
	}

	return nil
}

// Status reports the day's use.
func (b *Budget) Status() (used, daily int) {
	b.mu.Lock()
	defer b.mu.Unlock()

	if b.now().UTC().Format(time.DateOnly) != b.day {
		return 0, b.daily
	}

	return b.used, b.daily
}
