package cache

import (
	"context"
	"net/url"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestServerIsolatesServicesWithACLUsers runs against a real Redis-protocol
// server named by CACHE_TEST_REDIS_URL (an admin connection). CI runs it
// against every server the homelab may use. It checks the deployment
// guidance in docs/server-contract.md: a service's ACL user, limited to its
// prefix, can use the cache and budgets, and can't see or touch another
// service's keys.
func TestServerIsolatesServicesWithACLUsers(t *testing.T) {
	admin := os.Getenv("CACHE_TEST_REDIS_URL")
	if admin == "" {
		t.Skip("CACHE_TEST_REDIS_URL is not set")
	}

	opts, err := redis.ParseURL(admin)
	require.NoError(t, err)

	rdb := redis.NewClient(opts)

	t.Cleanup(func() { _ = rdb.Close() })

	ctx := t.Context()

	for _, svc := range []string{"svc-a", "svc-b"} {
		// Delete and recreate rather than use the `reset` rule, which
		// Dragonfly (v2.1.0) doesn't support.
		_ = rdb.Do(ctx, "ACL", "DELUSER", svc).Err()
		require.NoError(t, rdb.Do(ctx, "ACL", "SETUSER", svc, "on", ">pw-"+svc,
			"~"+svc+":*", "&"+svc+":*", "+@all", "-@dangerous", "-@admin",
			"-scan", "-randomkey", "-dbsize").Err())
	}

	t.Cleanup(func() { _ = rdb.Do(context.WithoutCancel(ctx), "ACL", "DELUSER", "svc-a", "svc-b").Err() })
	require.NoError(t, rdb.Set(ctx, "svc-b:c:secret", "hunter2", time.Minute).Err())

	userURL := func(user string) string {
		u, err := url.Parse(admin)
		require.NoError(t, err)

		u.User = url.UserPassword(user, "pw-"+user)

		return u.String()
	}

	store, err := NewRedisStore(userURL("svc-a"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = store.Close() })

	c := New(Options{Store: store, Prefix: "svc-a", StaleFor: time.Hour})
	f := &counter{value: "v1"}

	t.Run("a service's user can use the cache", func(t *testing.T) {
		_, _, err := Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
		require.NoError(t, err)

		v, meta, err := Fetch(t.Context(), c, c.Key("q"), hour, f.fetch)
		require.NoError(t, err)
		assert.Equal(t, "v1", v)
		assert.True(t, meta.Hit)
	})

	t.Run("a service's user can count budgets", func(t *testing.T) {
		bud := c.NewBudget("API", BudgetConfig{Daily: 5})

		require.NoError(t, bud.Take(t.Context(), func(time.Duration) error { return nil }))

		used, _ := bud.Status(t.Context())
		assert.Equal(t, 1, used)
	})

	user, err := redis.ParseURL(userURL("svc-a"))
	require.NoError(t, err)

	user.DisableIdentity = true
	a := redis.NewClient(user)

	t.Cleanup(func() { _ = a.Close() })

	for name, cmd := range map[string][]any{
		"read another service's key":  {"GET", "svc-b:c:secret"},
		"write another service's key": {"SET", "svc-b:c:secret", "x"},
		"list every key":              {"KEYS", "*"},
		"scan every key":              {"SCAN", "0"},
		"pick a random key":           {"RANDOMKEY"},
		"count every key":             {"DBSIZE"},
		"flush the database":          {"FLUSHDB"},
	} {
		t.Run("a service's user can't "+name, func(t *testing.T) {
			err := a.Do(t.Context(), cmd...).Err()

			require.Error(t, err)
			assert.Contains(t, err.Error(), "NOPERM")
		})
	}

	got, err := rdb.Get(ctx, "svc-b:c:secret").Result()
	require.NoError(t, err)
	assert.Equal(t, "hunter2", got)
}
