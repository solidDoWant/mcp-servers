package electroniccomponents

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const maxResponseBytes = 8 << 20

// httpError is an upstream answer that wasn't a success.
type httpError struct {
	source     string
	status     int
	body       string
	retryAfter time.Duration
}

func (e *httpError) Error() string {
	msg := fmt.Sprintf("%s returned HTTP %d", e.source, e.status)

	switch {
	case e.status == http.StatusTooManyRequests && e.retryAfter > 0:
		msg += fmt.Sprintf(" (rate limited; retry in %s)", e.retryAfter.Round(time.Second))
	case e.status == http.StatusTooManyRequests:
		msg += " (rate limited)"
	case e.status == http.StatusUnauthorized || e.status == http.StatusForbidden:
		msg += " (its API credentials were refused)"
	}

	if e.body != "" {
		msg += ": " + e.body
	}

	return msg
}

// doJSON sends one request and decodes a 2xx JSON response into out.
//
// Errors never contain the request URL: Mouser takes its API key as a query
// parameter, and net/http's errors quote the whole URL. Tool errors and logs
// must not carry the key.
func doJSON(ctx context.Context, client *http.Client, source, method, target string, header http.Header, body, out any) error {
	var reader io.Reader

	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("%s: encode request: %w", source, err)
		}

		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, target, reader)
	if err != nil {
		return fmt.Errorf("%s: build request: %w", source, redact(err))
	}

	for k, v := range header {
		req.Header[k] = v
	}

	req.Header.Set("Accept", "application/json")

	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: request failed: %w", source, redact(err))
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%s: read response: %w", source, redact(err))
	}

	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return &httpError{
			source: source, status: resp.StatusCode, body: snippet(raw),
			retryAfter: retryAfter(resp.Header.Get("Retry-After")),
		}
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%s: decode response: %w", source, err)
	}

	return nil
}

// redact drops the URL from a *url.Error, keeping its cause.
func redact(err error) error {
	var ue *url.Error
	if errors.As(err, &ue) {
		return fmt.Errorf("%s: %w", ue.Op, ue.Err)
	}

	return err
}

func retryAfter(v string) time.Duration {
	if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
		return time.Duration(secs) * time.Second
	}

	return 0
}

func snippet(b []byte) string {
	const limit = 300

	s := strings.Join(strings.Fields(string(b)), " ")
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}

	return s
}

// userAgentTransport names the server in every request.
type userAgentTransport struct {
	base http.RoundTripper
	ua   string
}

func (t *userAgentTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("User-Agent", t.ua)

	return t.base.RoundTrip(r)
}
