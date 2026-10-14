package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kryspal/plugin-sdk/sdk"
)

const (
	// defaultBaseURL is the TMDB API v3 URL.
	defaultBaseURL = "https://api.themoviedb.org/3"
	// imageBaseURL is the URL prefix for original-size images; thumbnails are handled by the server.
	imageBaseURL = "https://image.tmdb.org/t/p/original"
	// requestsPerSecond is the self-imposed rate limit: TMDB's cap is about 50 per second, leaving a 20% margin (owner, 2026-10-10: TMDB's limit is not low, so it can be raised).
	requestsPerSecond = 40
	// maxConcurrent is the cap on simultaneous TMDB requests: 40 per second times a request time of about 0.3 s gives about 12 in flight.
	maxConcurrent = 12
	// slowdownAfter429 is how long to slow down (halve the rate) after a 429: once the pause ends, do not rush back to the cap and get rate limited again.
	slowdownAfter429 = 60 * time.Second
	// defaultRetryAfter is the pause when a 429 carries no Retry-After.
	defaultRetryAfter = 10 * time.Second
	maxResponseBytes  = 8 << 20
	requestTimeout    = 20 * time.Second
	// retryWait is the wait before retrying after a 5xx or a dropped connection (plus up to the same length of random jitter). Retries only once.
	retryWait = 500 * time.Millisecond
)

var (
	errUnauthorized = errors.New("TMDB rejected the API token")
	errNotFound     = errors.New("not found on TMDB")
	// errTransient marks an error that can be retried once: a 5xx or an interrupted connection (TMDB's CDN occasionally returns 502, 504 or resets the connection).
	errTransient = errors.New("transient TMDB error")
)

// tokenKind is the kind of credential the user entered.
type tokenKind int

const (
	tokenInvalid tokenKind = iota
	tokenBearer            // v4 API Read Access Token (JWT, starts with eyJ), sent as Authorization: Bearer
	tokenAPIKey            // v3 API key (32 hex digits), sent in the api_key query parameter
)

var apiKeyRE = regexp.MustCompile(`^[0-9a-fA-F]{32}$`)

func classifyToken(t string) tokenKind {
	switch {
	case strings.HasPrefix(t, "eyJ"):
		return tokenBearer
	case apiKeyRE.MatchString(t):
		return tokenAPIKey
	}
	return tokenInvalid
}

// tmdbClient calls the TMDB API: authentication, self-imposed rate limiting, concurrency cap, error translation.
//
// With a v3 API key the key is in the URL: every error message (including net/http's url.Error) is masked by redact before it is returned or logged.
type tmdbClient struct {
	http    *http.Client
	baseURL string
	token   string
	kind    tokenKind
	log     *slog.Logger

	sem     chan struct{}
	mu      sync.Mutex
	next    time.Time // earliest time the next request may be sent (requestsPerSecond per second)
	limiter time.Duration
	// slowUntil is the time until which the rate is halved (after a 429, see slowdownAfter429); the zero value means no slowdown.
	slowUntil time.Time
	retry     time.Duration // wait before a retry; tests can shorten it
}

func newClient(baseURL, token string, log *slog.Logger) *tmdbClient {
	if log == nil {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	return &tmdbClient{http: &http.Client{Timeout: requestTimeout}, baseURL: strings.TrimRight(baseURL, "/"), token: token,
		kind: classifyToken(token), log: log, sem: make(chan struct{}, maxConcurrent),
		limiter: time.Second / requestsPerSecond, retry: retryWait}
}

// redact replaces the credential in an error message. It preserves the identity of errUnauthorized, errNotFound and sdk.RateLimitError (they carry no URL).
func (c *tmdbClient) redact(err error) error {
	if err == nil || c.token == "" || !strings.Contains(err.Error(), c.token) {
		return err
	}
	return errors.New(strings.ReplaceAll(err.Error(), c.token, "[REDACTED]"))
}

// wait queues according to the self-imposed rate limit.
func (c *tmdbClient) wait(ctx context.Context) error {
	c.mu.Lock()
	now := time.Now()
	at := c.next
	if at.Before(now) {
		at = now
	}
	step := c.limiter
	if now.Before(c.slowUntil) {
		step *= 2 // just rate limited: halve the rate
	}
	c.next = at.Add(step)
	c.mu.Unlock()
	if d := time.Until(at); d > 0 {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-t.C:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

// get sends GET {baseURL}{path}?{query} and parses the JSON into out.
//
// Errors: 401 is errUnauthorized (the caller turns it into awaiting configuration); 404 is errNotFound; 429 is sdk.RateLimited;
// other status codes and connection errors are wrapped as-is; a 5xx or a dropped connection waits retry plus jitter and is retried once. Error messages carry no credential, and the URL only carries the path.
func (c *tmdbClient) get(ctx context.Context, path string, query url.Values, out any) error {
	err := c.do(ctx, path, query, out)
	if errors.Is(err, errTransient) {
		t := time.NewTimer(c.retry + rand.N(c.retry+1))
		select {
		case <-t.C:
			err = c.do(ctx, path, query, out)
		case <-ctx.Done():
			t.Stop()
		}
	}
	return c.redact(err)
}

func (c *tmdbClient) do(ctx context.Context, path string, query url.Values, out any) error {
	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { <-c.sem }()
	if err := c.wait(ctx); err != nil {
		return err
	}
	q := url.Values{}
	for k, v := range query {
		q[k] = v
	}
	if c.kind == tokenAPIKey {
		q.Set("api_key", c.token)
	}
	u := c.baseURL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return fmt.Errorf("TMDB %s: bad request", path) // do not return err: it carries the full URL
	}
	if c.kind != tokenAPIKey {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err // url.Error carries the full URL; keep only the cause
		}
		if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
			return fmt.Errorf("TMDB %s: %w (%w)", path, err, errTransient)
		}
		return fmt.Errorf("TMDB %s: %w", path, err)
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusUnauthorized:
		return errUnauthorized
	case http.StatusNotFound:
		return errNotFound
	case http.StatusTooManyRequests:
		d := defaultRetryAfter
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s > 0 {
			d = time.Duration(s) * time.Second
		}
		c.mu.Lock()
		// The host pauses the whole plugin until Retry-After; after the pause, proceed at half the rate for slowdownAfter429.
		if until := time.Now().Add(d + slowdownAfter429); until.After(c.slowUntil) {
			c.slowUntil = until
		}
		c.mu.Unlock()
		return sdk.RateLimited(d, "TMDB rate limit reached")
	default:
		if resp.StatusCode >= 500 {
			return fmt.Errorf("TMDB %s: %s (%w)", path, resp.Status, errTransient)
		}
		return fmt.Errorf("TMDB %s: %s", path, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("TMDB %s: %w", path, err)
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("TMDB %s: bad response: %w", path, err)
	}
	return nil
}
