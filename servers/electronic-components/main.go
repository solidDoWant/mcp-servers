// Command electronic-components serves the electronic-components MCP server: search DigiKey, Mouser and
// JLCPCB's parts library for electronic components and compare offers.
package main

import (
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/solidDoWant/mcp-servers/internal/cache"
	"github.com/solidDoWant/mcp-servers/internal/mcpserver"
	"github.com/solidDoWant/mcp-servers/servers/electronic-components/internal/electroniccomponents"
)

// version is set at build time.
var version = "dev"

func main() {
	cfg, err := config()
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	mcpserver.Main(mcpserver.Config{
		Name:         "electronic-components",
		Version:      version,
		Instructions: electroniccomponents.Instructions,
		Register:     electroniccomponents.Tools(electroniccomponents.New(cfg)),
	})
}

func config() (electroniccomponents.Config, error) {
	cfg := electroniccomponents.Config{
		Version: version,
		Logger:  slog.New(slog.NewJSONHandler(os.Stdout, nil)).With("server", "electronic-components", "version", version),
		DigiKey: electroniccomponents.DigiKeyConfig{
			BaseURL:  os.Getenv("DIGIKEY_API_URL"),
			Site:     os.Getenv("DIGIKEY_LOCALE_SITE"),
			Currency: os.Getenv("DIGIKEY_LOCALE_CURRENCY"),
		},
		MouserURL:    os.Getenv("MOUSER_API_URL"),
		JLCSearchURL: os.Getenv("JLCSEARCH_URL"),
	}

	var err error

	// Credentials come from files, so they never appear in the process's
	// environment or arguments. A missing file leaves its source off.
	if cfg.DigiKey.ClientID, err = secretFile("DIGIKEY_CLIENT_ID_FILE"); err != nil {
		return cfg, err
	}

	if cfg.DigiKey.ClientSecret, err = secretFile("DIGIKEY_CLIENT_SECRET_FILE"); err != nil {
		return cfg, err
	}

	if cfg.MouserAPIKey, err = secretFile("MOUSER_API_KEY_FILE"); err != nil {
		return cfg, err
	}

	if v := os.Getenv("JLCSEARCH_DISABLED"); v != "" {
		if cfg.JLCSearchDisabled, err = strconv.ParseBool(v); err != nil {
			return cfg, fmt.Errorf("JLCSEARCH_DISABLED: %w", err)
		}
	}

	if v := os.Getenv("CACHE_TTL"); v != "" {
		if cfg.CacheTTL, err = time.ParseDuration(v); err != nil || cfg.CacheTTL <= 0 {
			return cfg, fmt.Errorf("CACHE_TTL must be a positive duration such as 1h, not %q", v)
		}
	}

	cfg.CacheStaleFor = 24 * time.Hour

	if v := os.Getenv("CACHE_STALE_FOR"); v != "" {
		if cfg.CacheStaleFor, err = time.ParseDuration(v); err != nil || cfg.CacheStaleFor < 0 {
			return cfg, fmt.Errorf("CACHE_STALE_FOR must be a duration such as 24h, or 0 for none, not %q", v)
		}
	}

	cfg.CachePrefix = os.Getenv("CACHE_KEY_PREFIX")

	// A shared Redis-protocol server keeps answers and budgets across
	// restarts and replicas. CACHE_URL is its connection string; with an ACL
	// user's password in it, set it from a Secret. The CACHE_TLS_* files add
	// mutual TLS (with a rediss:// URL).
	if cacheURL := strings.TrimSpace(os.Getenv("CACHE_URL")); cacheURL != "" {
		files := cache.TLSFiles{
			CertFile: os.Getenv("CACHE_TLS_CERT_FILE"),
			KeyFile:  os.Getenv("CACHE_TLS_KEY_FILE"),
			CAFile:   os.Getenv("CACHE_TLS_CA_FILE"),
		}

		if cfg.CacheStore, err = cache.NewRedisStore(cacheURL, files); err != nil {
			return cfg, fmt.Errorf("CACHE_URL: %w", err)
		}
	}

	if v := os.Getenv("CACHE_SIZE"); v != "" {
		if cfg.CacheSize, err = strconv.Atoi(v); err != nil || cfg.CacheSize <= 0 {
			return cfg, fmt.Errorf("CACHE_SIZE must be a positive number, not %q", v)
		}
	}

	if cfg.DigiKeyBudget, err = budget("DIGIKEY", electroniccomponents.DefaultDigiKeyBudget); err != nil {
		return cfg, err
	}

	if cfg.MouserBudget, err = budget("MOUSER", electroniccomponents.DefaultMouserBudget); err != nil {
		return cfg, err
	}

	return cfg, nil
}

// secretFile reads the file named by the environment variable, trimmed. An
// unset variable or a missing file gives "".
func secretFile(env string) (string, error) {
	path := os.Getenv(env)
	if path == "" {
		return "", nil
	}

	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	}

	if err != nil {
		return "", fmt.Errorf("%s: %w", env, err)
	}

	return strings.TrimSpace(string(b)), nil
}

// budget reads <PREFIX>_DAILY_LIMIT and <PREFIX>_PER_MINUTE over def.
func budget(prefix string, def electroniccomponents.BudgetConfig) (electroniccomponents.BudgetConfig, error) {
	b := def

	if v := os.Getenv(prefix + "_DAILY_LIMIT"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return b, fmt.Errorf("%s_DAILY_LIMIT must be a number, 0 for none, not %q", prefix, v)
		}

		b.Daily = n
	}

	if v := os.Getenv(prefix + "_PER_MINUTE"); v != "" {
		n, err := strconv.ParseFloat(v, 64)
		if err != nil || n <= 0 {
			return b, fmt.Errorf("%s_PER_MINUTE must be a positive number, not %q", prefix, v)
		}

		b.PerMinute = n
	}

	return b, nil
}
