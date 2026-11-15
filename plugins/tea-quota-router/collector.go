package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---- fixed management api-call endpoints -----------------------------------
//
// The collector never accepts a URL from config, UI or upstream data. Every
// subrequest is one of the fixed GET endpoints below, sent through the
// protected /v0/management/api-call with the credential's auth_index and the
// "$TOKEN$" placeholder, so the raw OAuth/API token is resolved by the host
// and is never visible to this plugin.

type endpoint struct {
	url     string
	headers map[string]string
}

const (
	epClaudeUsage   = "claude-usage"
	epClaudeProfile = "claude-profile"
	epCodexUsage    = "codex-usage"
	epOpenRouterKey = "openrouter-key"

	claudeOAuthBeta = "oauth-2025-04-20"
	collectorUA     = "tea-quota-router/0.1"
	managementPath  = "/v0/management/api-call"

	maxMgmtResponse   = 4 << 20
	maxUpstreamBody   = 2 << 20
	maxRetryAfter     = time.Hour
	managementTimeout = 20 * time.Second
)

func claudeHeaders() map[string]string {
	return map[string]string{
		"Authorization":  "Bearer $TOKEN$",
		"anthropic-beta": claudeOAuthBeta,
		"Accept":         "application/json",
		"Content-Type":   "application/json",
		"User-Agent":     collectorUA,
	}
}

func plainBearerHeaders() map[string]string {
	return map[string]string{"Authorization": "Bearer $TOKEN$", "Accept": "application/json", "User-Agent": collectorUA}
}

func fixedEndpoint(id string) (endpoint, bool) {
	switch id {
	case epClaudeUsage:
		return endpoint{"https://api.anthropic.com/api/oauth/usage", claudeHeaders()}, true
	case epClaudeProfile:
		return endpoint{"https://api.anthropic.com/api/oauth/profile", claudeHeaders()}, true
	case epCodexUsage:
		return endpoint{"https://chatgpt.com/backend-api/wham/usage", plainBearerHeaders()}, true
	case epOpenRouterKey:
		return endpoint{"https://openrouter.ai/api/v1/key", plainBearerHeaders()}, true
	}
	return endpoint{}, false
}

// ---- generic, body-free errors ---------------------------------------------

type failKind int

const (
	failTransient   failKind = iota + 1 // network, 5xx: retried briefly in-pass
	failRateLimited                     // 429; Retry-After honored
	failAuth                            // credential cannot be resolved / upstream 401/403
	failRejected                        // other non-2xx
	failParse                           // body not recognized by adapter / malformed envelope
	failTooLarge                        // response above bound
	failKeyMissing                      // management key env empty
	failKeyRejected                     // management 401/403
	failIncomplete                      // pagination bound hit; no partial totals emitted
)

var failMessages = map[failKind]string{
	failTransient:   "upstream temporarily unavailable",
	failRateLimited: "rate limited",
	failAuth:        "credential rejected",
	failRejected:    "request rejected",
	failParse:       "response not recognized",
	failTooLarge:    "response too large",
	failKeyMissing:  "management key unavailable",
	failKeyRejected: "management key rejected",
	failIncomplete:  "report incomplete",
}

// sourceError carries only a classification and an optional Retry-After. Its
// message is a fixed generic string: upstream bodies, URLs, tokens and
// transport error text never reach it.
type sourceError struct {
	kind       failKind
	retryAfter time.Duration
}

func (e *sourceError) Error() string { return failMessages[e.kind] }

func kindOf(err error) failKind {
	var se *sourceError
	if errors.As(err, &se) {
		return se.kind
	}
	return failTransient
}

func retryAfterOf(err error) time.Duration {
	var se *sourceError
	if errors.As(err, &se) {
		return se.retryAfter
	}
	return 0
}

func parseRetryAfter(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if n, err := strconv.ParseInt(v, 10, 64); err == nil {
		switch {
		case n <= 0:
			return 0
		case n >= int64(maxRetryAfter/time.Second):
			return maxRetryAfter
		}
		return time.Duration(n) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		d := t.Sub(now)
		switch {
		case d <= 0:
			return 0
		case d > maxRetryAfter:
			return maxRetryAfter
		}
		return d
	}
	return 0
}

func headerValue(h map[string][]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) && len(v) > 0 {
			return v[0]
		}
	}
	return ""
}

// classifyStatus maps an HTTP status to a generic failure; ok statuses
// return nil.
func classifyStatus(code int, retryAfter string, now time.Time) error {
	switch {
	case code == http.StatusOK:
		return nil
	case code == http.StatusUnauthorized || code == http.StatusForbidden:
		return &sourceError{kind: failAuth}
	case code == http.StatusTooManyRequests:
		return &sourceError{kind: failRateLimited, retryAfter: parseRetryAfter(retryAfter, now)}
	case code >= 500:
		return &sourceError{kind: failTransient}
	}
	return &sourceError{kind: failRejected}
}

// ---- per-source retry/backoff state ----------------------------------------

const (
	backoffBase = 30 * time.Second
	backoffMax  = 15 * time.Minute
)

type sourceState struct {
	failures int
	nextAt   time.Time
	lastKind failKind
}

func backoffDelay(failures int) time.Duration {
	d := backoffBase
	for i := 1; i < failures && d < backoffMax; i++ {
		d *= 2
	}
	if d > backoffMax {
		d = backoffMax
	}
	return d
}

type sourceStates struct {
	mu sync.Mutex
	m  map[string]*sourceState
}

func newSourceStates() *sourceStates { return &sourceStates{m: map[string]*sourceState{}} }

// blocked reports whether the source is still in backoff and the failure kind
// that caused it.
func (s *sourceStates) blocked(key string, now time.Time) (failKind, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.m[key]
	if st == nil || !st.nextAt.After(now) {
		return 0, false
	}
	return st.lastKind, true
}

func (s *sourceStates) succeed(key string) {
	s.mu.Lock()
	delete(s.m, key)
	s.mu.Unlock()
}

// fail records a failure. A Retry-After from the upstream replaces the
// computed exponential backoff for that attempt (bounded by maxRetryAfter).
func (s *sourceStates) fail(key string, err error, now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := s.m[key]
	if st == nil {
		st = &sourceState{}
		s.m[key] = st
	}
	st.failures++
	st.lastKind = kindOf(err)
	d := backoffDelay(st.failures)
	if ra := retryAfterOf(err); ra > 0 {
		d = ra
	}
	st.nextAt = now.Add(d)
}

// prune drops state for sources that no longer exist (bounded memory).
func (s *sourceStates) prune(live map[string]bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k := range s.m {
		if !live[k] {
			delete(s.m, k)
		}
	}
}

func (s *sourceStates) get(key string) (sourceState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if st := s.m[key]; st != nil {
		return *st, true
	}
	return sourceState{}, false
}

// ---- management client ------------------------------------------------------

type managementClient struct {
	base, keyEnv string
	client       *http.Client
}

func newManagementClient(base, keyEnv string) (*managementClient, error) {
	// Re-validate through the shared config rules: literal loopback HTTP
	// origin only, key env name syntax.
	if _, err := parseConfig([]byte("dataDir: /unused\nmanagementURL: " + base + "\nmanagementKeyEnv: " + keyEnv + "\n")); err != nil {
		return nil, err
	}
	tr := &http.Transport{
		Proxy:                 nil, // loopback only: never via environment proxies
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConns:          2,
		IdleConnTimeout:       30 * time.Second,
		ResponseHeaderTimeout: managementTimeout,
	}
	return &managementClient{
		base:   strings.TrimRight(base, "/"),
		keyEnv: keyEnv,
		client: &http.Client{
			Timeout:   managementTimeout + 5*time.Second,
			Transport: tr,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return errors.New("redirect refused")
			},
		},
	}, nil
}

func (c *managementClient) keyAvailable() bool { return os.Getenv(c.keyEnv) != "" }

func (c *managementClient) close() {
	if tr, ok := c.client.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

// call performs one fixed GET through /v0/management/api-call and returns the
// upstream body (the api-call envelope carries it as a string) when the
// upstream answered 200. Every failure is a *sourceError (or the context
// error); no upstream or management body text is ever returned in an error.
func (c *managementClient) call(ctx context.Context, endpointID, authIndex string) ([]byte, error) {
	ep, ok := fixedEndpoint(endpointID)
	authIndex = strings.TrimSpace(authIndex)
	if !ok || authIndex == "" {
		return nil, &sourceError{kind: failRejected}
	}
	key := os.Getenv(c.keyEnv)
	if key == "" {
		return nil, &sourceError{kind: failKeyMissing}
	}
	reqBody, err := json.Marshal(map[string]any{
		"auth_index": authIndex,
		"method":     "GET",
		"url":        ep.url,
		"header":     ep.headers,
	})
	if err != nil {
		return nil, &sourceError{kind: failRejected}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base+managementPath, bytes.NewReader(reqBody))
	if err != nil {
		return nil, &sourceError{kind: failRejected}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &sourceError{kind: failTransient}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusOK:
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, &sourceError{kind: failKeyRejected}
	case resp.StatusCode == http.StatusTooManyRequests:
		return nil, &sourceError{kind: failRateLimited, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"), time.Now())}
	case resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusNotFound:
		// api-call answers 400 when the credential/token cannot be resolved.
		return nil, &sourceError{kind: failAuth}
	case resp.StatusCode >= 500:
		return nil, &sourceError{kind: failTransient}
	default:
		return nil, &sourceError{kind: failRejected}
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMgmtResponse+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &sourceError{kind: failTransient}
	}
	if len(raw) > maxMgmtResponse {
		return nil, &sourceError{kind: failTooLarge}
	}
	var env struct {
		StatusCode int                 `json:"status_code"`
		Header     map[string][]string `json:"header"`
		Body       string              `json:"body"`
	}
	if err := json.Unmarshal(raw, &env); err != nil || env.StatusCode == 0 {
		return nil, &sourceError{kind: failParse}
	}
	if serr := classifyStatus(env.StatusCode, headerValue(env.Header, "Retry-After"), time.Now()); serr != nil {
		return nil, serr
	}
	if len(env.Body) > maxUpstreamBody {
		return nil, &sourceError{kind: failTooLarge}
	}
	return []byte(env.Body), nil
}

// ---- direct admin HTTPS client (cost reports only) -------------------------

const (
	adminCostWindow = 30 * 24 * time.Hour
	adminPageLimit  = 31
	maxAdminPages   = 12
	adminTimeout    = 30 * time.Second
	adminScope      = "organization-cost-30d"
)

var defaultAdminBases = map[string]string{
	"openai":    "https://api.openai.com",
	"anthropic": "https://api.anthropic.com",
}

func newAdminHTTPClient() *http.Client {
	return &http.Client{
		Timeout: adminTimeout,
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 10 * time.Second}).DialContext,
			TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: adminTimeout,
			MaxIdleConns:          2,
			IdleConnTimeout:       30 * time.Second,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("redirect refused") },
	}
}

// adminGet performs one fixed GET against the provider's fixed HTTPS origin.
// path and query are built by this package only; the API key travels in the
// header and never appears in an error.
func adminGet(ctx context.Context, hc *http.Client, base, provider, path string, q url.Values, key string, now time.Time) ([]byte, error) {
	u := base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, &sourceError{kind: failRejected}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", collectorUA)
	switch provider {
	case "openai":
		req.Header.Set("Authorization", "Bearer "+key)
	case "anthropic":
		req.Header.Set("x-api-key", key)
		req.Header.Set("anthropic-version", "2023-06-01")
	default:
		return nil, &sourceError{kind: failRejected}
	}
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &sourceError{kind: failTransient}
	}
	defer resp.Body.Close()
	if serr := classifyStatus(resp.StatusCode, resp.Header.Get("Retry-After"), now); serr != nil {
		return nil, serr
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxUpstreamBody+1))
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &sourceError{kind: failTransient}
	}
	if len(raw) > maxUpstreamBody {
		return nil, &sourceError{kind: failTooLarge}
	}
	return raw, nil
}
