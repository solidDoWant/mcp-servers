// Package sendcutsend uploads parts to sendcutsend.com, prices them and
// builds saved carts, through the JSON API the site's own web app uses. The
// API is undocumented: the requests here are the ones the app sends, and the
// tests pin the response shapes this package relies on.
//
// The site has no API keys. The client signs in with the "remember me"
// cookie (user_credentials) from a browser session, which stays valid until
// the account logs out or changes its password. Every tool call starts a new
// site session from that cookie, because the site ties the shopping cart and
// the CSRF token to the session.
package sendcutsend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"strings"
	"time"

	"golang.org/x/net/html"
)

// DefaultBaseURL is the SendCutSend web app.
const DefaultBaseURL = "https://app.sendcutsend.com"

// DefaultUploadHosts are the hosts the site's upload URLs point at. Only
// these are accepted as a part's upload URL, so the server never fetches or
// forwards arbitrary URLs.
var DefaultUploadHosts = []string{"sendcutsend.s3-accelerate.amazonaws.com", "sendcutsend.s3.amazonaws.com"}

// DefaultModelHosts serve processed parts' geometry.
var DefaultModelHosts = []string{"cdn.sendcutsend.com"}

const (
	credentialsCookie = "user_credentials"
	maxResponseBytes  = 16 << 20
)

// ErrNotSignedIn means the site didn't accept the configured credentials.
var ErrNotSignedIn = errors.New("SendCutSend did not accept the saved sign-in cookie (user_credentials); " +
	"it stops working when the account logs out or changes its password. " +
	"Sign in at https://app.sendcutsend.com in a browser, copy the user_credentials cookie and replace the server's secret")

// ErrNotFound means the part or cart doesn't exist or isn't this account's.
var ErrNotFound = errors.New("not found")

// Options configures a Client.
type Options struct {
	// BaseURL is the web app's address. Defaults to DefaultBaseURL.
	BaseURL string
	// CredentialsFile holds the user_credentials cookie value. It is read on
	// every call, so a replaced secret takes effect without a restart. Empty
	// means a guest session, which can upload and price parts but can't keep
	// them between calls or save carts.
	CredentialsFile string
	// Version is the server's release, sent in the User-Agent.
	Version string
	// UploadHosts overrides DefaultUploadHosts.
	UploadHosts []string
	// ModelHosts overrides DefaultModelHosts.
	ModelHosts []string
	// PollInterval is how often to check a part that is still processing.
	PollInterval time.Duration
	// ProcessTimeout is how long create_part waits for processing.
	ProcessTimeout time.Duration
	// HTTPClient sends the requests. Its Jar is ignored: each session has its
	// own.
	HTTPClient *http.Client
}

// Client calls the SendCutSend web app's API.
type Client struct {
	base           *url.URL
	credsFile      string
	userAgent      string
	uploadHosts    []string
	modelHosts     []string
	pollInterval   time.Duration
	processTimeout time.Duration
	transport      http.RoundTripper
	timeout        time.Duration
}

// NewClient returns a client configured by opts.
func NewClient(opts Options) (*Client, error) {
	raw := opts.BaseURL
	if raw == "" {
		raw = DefaultBaseURL
	}

	base, err := url.Parse(raw)
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("invalid base URL %q", raw)
	}

	c := &Client{
		base:           base,
		credsFile:      opts.CredentialsFile,
		userAgent:      "mcp-servers-sendcutsend/" + opts.Version + " (+https://github.com/solidDoWant/mcp-servers)",
		uploadHosts:    opts.UploadHosts,
		modelHosts:     opts.ModelHosts,
		pollInterval:   opts.PollInterval,
		processTimeout: opts.ProcessTimeout,
		transport:      http.DefaultTransport,
		timeout:        60 * time.Second,
	}

	if len(c.uploadHosts) == 0 {
		c.uploadHosts = DefaultUploadHosts
	}

	if len(c.modelHosts) == 0 {
		c.modelHosts = DefaultModelHosts
	}

	if c.pollInterval <= 0 {
		c.pollInterval = 2 * time.Second
	}

	if c.processTimeout <= 0 {
		c.processTimeout = 90 * time.Second
	}

	if opts.HTTPClient != nil {
		if opts.HTTPClient.Transport != nil {
			c.transport = opts.HTTPClient.Transport
		}

		if opts.HTTPClient.Timeout > 0 {
			c.timeout = opts.HTTPClient.Timeout
		}
	}

	return c, nil
}

// credentials reads the cookie value from the credentials file. It accepts
// the bare value or "user_credentials=<value>", with surrounding space.
func (c *Client) credentials() (string, error) {
	if c.credsFile == "" {
		return "", nil
	}

	b, err := os.ReadFile(c.credsFile)
	if err != nil {
		return "", fmt.Errorf("read the SendCutSend credentials file: %w", err)
	}

	v := strings.TrimSpace(string(b))
	v = strings.TrimPrefix(v, credentialsCookie+"=")

	if v == "" {
		return "", errors.New("the SendCutSend credentials file is empty; put the user_credentials cookie value in it")
	}

	return v, nil
}

// appData is the configuration the web app's page embeds.
type appData struct {
	DrawingAPIVersion int     `json:"drawing_api_version"`
	UserToken         *string `json:"user_token"`
}

// session is one site session: its own cookies (and so its own cart) and
// CSRF token. Tools open one per call.
type session struct {
	c      *Client
	http   *http.Client
	csrf   string
	app    appData
	signed bool
}

// newSession signs in (or starts a guest session when no credentials are
// configured) and reads the CSRF token and app configuration from the app's
// page.
func (c *Client) newSession(ctx context.Context) (*session, error) {
	creds, err := c.credentials()
	if err != nil {
		return nil, err
	}

	jar, err := cookiejar.New(nil)
	if err != nil {
		return nil, fmt.Errorf("create cookie jar: %w", err)
	}

	if creds != "" {
		jar.SetCookies(c.base, []*http.Cookie{{Name: credentialsCookie, Value: creds, Path: "/"}})
	}

	s := &session{
		c:      c,
		http:   &http.Client{Transport: c.transport, Jar: jar, Timeout: c.timeout},
		signed: creds != "",
	}

	page, err := s.raw(ctx, http.MethodGet, "/customer", nil, "text/html")
	if err != nil {
		return nil, err
	}

	if err := s.readPage(page); err != nil {
		return nil, err
	}

	// The page names a guest token only when nobody is signed in.
	if s.signed && s.app.UserToken != nil && *s.app.UserToken != "" {
		return nil, ErrNotSignedIn
	}

	return s, nil
}

// readPage takes the CSRF token from the page's meta tag and the app
// configuration from its inline "appData = {...}" script.
func (s *session) readPage(page []byte) error {
	doc, err := html.Parse(bytes.NewReader(page))
	if err != nil {
		return fmt.Errorf("parse the SendCutSend app page: %w", err)
	}

	var script strings.Builder

	for n := range doc.Descendants() {
		if n.Type != html.ElementNode {
			continue
		}

		switch n.Data {
		case "meta":
			if attr(n, "name") == "csrf-token" {
				s.csrf = attr(n, "content")
			}
		case "script":
			for child := range n.ChildNodes() {
				if child.Type == html.TextNode && strings.Contains(child.Data, "appData") {
					script.WriteString(child.Data)
				}
			}
		}
	}

	if s.csrf == "" {
		return errors.New("the SendCutSend app page has no CSRF token; the site may have changed")
	}

	text := script.String()

	i := strings.Index(text, "appData")
	if i < 0 {
		return errors.New("the SendCutSend app page has no app configuration; the site may have changed")
	}

	j := strings.Index(text[i:], "{")
	if j < 0 {
		return errors.New("the SendCutSend app configuration isn't an object; the site may have changed")
	}

	// A JSON decoder stops at the end of the first value, so the rest of the
	// script doesn't matter.
	if err := json.NewDecoder(strings.NewReader(text[i+j:])).Decode(&s.app); err != nil {
		return fmt.Errorf("decode the SendCutSend app configuration: %w", err)
	}

	return nil
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}

	return ""
}

// raw sends one request to the app and returns the body of a 2xx response.
func (s *session) raw(ctx context.Context, method, path string, body any, accept string) ([]byte, error) {
	u := s.c.base.JoinPath(path)

	var reader io.Reader

	if body != nil {
		if method == http.MethodGet {
			q, ok := body.(url.Values)
			if !ok {
				return nil, errors.New("GET parameters must be url.Values")
			}

			u.RawQuery = q.Encode()
		} else {
			b, err := json.Marshal(body)
			if err != nil {
				return nil, fmt.Errorf("encode request: %w", err)
			}

			reader = bytes.NewReader(b)
		}
	}

	req, err := http.NewRequestWithContext(ctx, method, u.String(), reader)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("Accept", accept)
	req.Header.Set("User-Agent", s.c.userAgent)

	if reader != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if method != http.MethodGet && s.csrf != "" {
		req.Header.Set("X-CSRF-Token", s.csrf)
	}

	resp, err := s.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("call SendCutSend: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read SendCutSend response: %w", err)
	}

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return raw, nil
	}

	where := method + " " + path

	switch resp.StatusCode {
	case http.StatusUnauthorized:
		if s.signed {
			return nil, ErrNotSignedIn
		}

		return nil, fmt.Errorf("SendCutSend needs a signed-in account for %s; configure the credentials file", where)
	case http.StatusForbidden, http.StatusNotFound:
		return nil, fmt.Errorf("%w: SendCutSend returned HTTP %d for %s", ErrNotFound, resp.StatusCode, where)
	}

	return nil, fmt.Errorf("SendCutSend returned HTTP %d for %s: %s", resp.StatusCode, where, upstreamMessage(raw))
}

// call sends a JSON request and decodes the JSON response into out, which
// may be nil.
func (s *session) call(ctx context.Context, method, path string, body, out any) error {
	raw, err := s.raw(ctx, method, path, body, "application/json")
	if err != nil {
		return err
	}

	if out == nil {
		return nil
	}

	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("decode SendCutSend response to %s %s: %w", method, path, err)
	}

	return nil
}

// upstreamMessage turns an error body into something short enough for a
// tool error. The app answers errors with JSON objects of messages or with
// plain text.
func upstreamMessage(b []byte) string {
	var obj map[string]any
	if json.Unmarshal(b, &obj) == nil {
		var parts []string

		for k, v := range obj {
			parts = append(parts, fmt.Sprintf("%s: %v", k, v))
		}

		if len(parts) > 0 {
			return snippet([]byte(strings.Join(parts, "; ")))
		}
	}

	return snippet(b)
}

func snippet(b []byte) string {
	const limit = 300

	s := strings.TrimSpace(string(b))
	if r := []rune(s); len(r) > limit {
		s = string(r[:limit]) + "…"
	}

	return s
}

// uploadHostAllowed reports whether u is one of the site's upload URLs.
func (c *Client) uploadHostAllowed(u *url.URL) bool {
	for _, h := range c.uploadHosts {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}

	return false
}

func (c *Client) modelHostAllowed(u *url.URL) bool {
	for _, h := range c.modelHosts {
		if strings.EqualFold(u.Host, h) {
			return true
		}
	}

	return false
}

// put uploads data to a pre-signed upload URL.
func (s *session) put(ctx context.Context, uploadURL string, data []byte) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build upload request: %w", err)
	}

	req.Header.Set("User-Agent", s.c.userAgent)

	// The upload host is not the app: don't send the app's cookies.
	resp, err := (&http.Client{Transport: s.c.transport, Timeout: s.c.timeout}).Do(req)
	if err != nil {
		return fmt.Errorf("upload the file: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))

		return fmt.Errorf("the upload host returned HTTP %d: %s", resp.StatusCode, snippet(b))
	}

	return nil
}

// fetchPublic GETs a public file from a SendCutSend host other than the
// app, without the app's cookies.
func (s *session) fetchPublic(ctx context.Context, target string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	req.Header.Set("User-Agent", s.c.userAgent)

	resp, err := (&http.Client{Transport: s.c.transport, Timeout: s.c.timeout}).Do(req)
	if err != nil {
		return nil, fmt.Errorf("call SendCutSend: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // read-only body

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return nil, fmt.Errorf("read SendCutSend response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("SendCutSend returned HTTP %d: %s", resp.StatusCode, snippet(raw))
	}

	return raw, nil
}

// uploaded reports whether a file is at the upload URL. Uploaded files are
// public, so the object's address answers a HEAD without signing.
func (s *session) uploaded(ctx context.Context, u *url.URL) (bool, error) {
	obj := *u
	obj.RawQuery = ""

	req, err := http.NewRequestWithContext(ctx, http.MethodHead, obj.String(), nil)
	if err != nil {
		return false, fmt.Errorf("build upload check: %w", err)
	}

	req.Header.Set("User-Agent", s.c.userAgent)

	resp, err := (&http.Client{Transport: s.c.transport, Timeout: s.c.timeout}).Do(req)
	if err != nil {
		return false, fmt.Errorf("check the upload: %w", err)
	}

	_ = resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		return true, nil
	case http.StatusForbidden, http.StatusNotFound:
		// S3 answers 403 for a missing key when listing is denied.
		return false, nil
	default:
		return false, fmt.Errorf("the upload host returned HTTP %d when checking the upload", resp.StatusCode)
	}
}
