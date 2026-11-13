package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfigReadsKeysFromFilesAndSkipsMissingOnes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "id"), []byte(" client-id\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "secret"), []byte("client-secret\n"), 0o600))

	t.Setenv("DIGIKEY_CLIENT_ID_FILE", filepath.Join(dir, "id"))
	t.Setenv("DIGIKEY_CLIENT_SECRET_FILE", filepath.Join(dir, "secret"))
	t.Setenv("MOUSER_API_KEY_FILE", filepath.Join(dir, "missing"))
	t.Setenv("CACHE_TTL", "30m")
	t.Setenv("MOUSER_DAILY_LIMIT", "0")

	cfg, err := config()
	require.NoError(t, err)

	assert.Equal(t, "client-id", cfg.DigiKey.ClientID)
	assert.Equal(t, "client-secret", cfg.DigiKey.ClientSecret)
	assert.Empty(t, cfg.MouserAPIKey, "a missing key file leaves Mouser off")
	assert.Equal(t, 30*time.Minute, cfg.CacheTTL)
	assert.Zero(t, cfg.MouserBudget.Daily, "0 means no daily limit")
	assert.InDelta(t, 25, cfg.MouserBudget.PerMinute, 0, "unset values keep their defaults")
}

func TestConfigRejectsMalformedSettings(t *testing.T) {
	for env, value := range map[string]string{
		"CACHE_TTL":           "soon",
		"CACHE_SIZE":          "-1",
		"DIGIKEY_DAILY_LIMIT": "lots",
		"MOUSER_PER_MINUTE":   "0",
		"JLCSEARCH_DISABLED":  "maybe",
	} {
		t.Run(env, func(t *testing.T) {
			t.Setenv(env, value)

			_, err := config()

			assert.ErrorContains(t, err, env)
		})
	}
}
