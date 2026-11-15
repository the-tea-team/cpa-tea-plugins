package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testMgmtEnv = "QUOTA_TEST_MGMT"
	testMgmtKey = "synthetic-mgmt-key"
)

var t0 = time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)

// ---- fake clock --------------------------------------------------------------

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time { c.mu.Lock(); defer c.mu.Unlock(); return c.t }
func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// ---- mock management server --------------------------------------------------
//
// Speaks the real /v0/management/api-call contract: POST JSON request with
// auth_index/method/url/header, answers HTTP 200 with the envelope
// {"status_code":N,"header":{...},"body":"<upstream body as a STRING>"}.

type mgmtCall struct {
	AuthIndex string
	URL       string
	Header    map[string]string
}

type mgmtReply struct {
	httpStatus int // management-level status; 0 = 200
	httpHeader map[string]string
	status     int // upstream status inside the envelope; 0 = 200
	header     map[string][]string
	body       string // upstream body
	raw        string // if set, written verbatim as the HTTP body
	block      bool   // hold until the client goes away
}

type mockMgmt struct {
	t   *testing.T
	srv *httptest.Server

	mu          sync.Mutex
	calls       []mgmtCall
	inflight    int
	maxInflight int
	delay       time.Duration
	respond     func(authIndex, url string) mgmtReply
	overrides   map[string]func(url string) mgmtReply
}

func newMockMgmt(t *testing.T, respond func(authIndex, url string) mgmtReply) *mockMgmt {
	t.Helper()
	t.Setenv(testMgmtEnv, testMgmtKey)
	m := &mockMgmt{t: t, respond: respond, overrides: map[string]func(string) mgmtReply{}}
	m.srv = httptest.NewServer(http.HandlerFunc(m.handle))
	t.Cleanup(m.srv.Close)
	return m
}

func (m *mockMgmt) handle(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if r.Method != http.MethodPost || r.URL.Path != "/v0/management/api-call" || r.URL.RawQuery != "" {
		m.t.Errorf("unexpected management request %s %s", r.Method, r.URL.String())
		http.Error(w, "bad", 400)
		return
	}
	if r.Header.Get("Authorization") != "Bearer "+testMgmtKey {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	if strings.Contains(string(raw), testMgmtKey) {
		m.t.Errorf("management key leaked into api-call body")
	}
	var req map[string]any
	if err := json.Unmarshal(raw, &req); err != nil {
		m.t.Errorf("api-call body not JSON: %v", err)
		http.Error(w, "bad", 400)
		return
	}
	for k := range req {
		if k != "auth_index" && k != "method" && k != "url" && k != "header" {
			m.t.Errorf("unexpected api-call field %q (no data/proxy_url allowed)", k)
		}
	}
	if req["method"] != "GET" {
		m.t.Errorf("api-call method = %v, want GET", req["method"])
	}
	call := mgmtCall{AuthIndex: fmt.Sprint(req["auth_index"]), URL: fmt.Sprint(req["url"]), Header: map[string]string{}}
	if h, ok := req["header"].(map[string]any); ok {
		for k, v := range h {
			call.Header[k] = fmt.Sprint(v)
		}
	}
	if call.Header["Authorization"] != "Bearer $TOKEN$" {
		m.t.Errorf("Authorization header must be the $TOKEN$ placeholder, got %q", call.Header["Authorization"])
	}

	m.mu.Lock()
	m.calls = append(m.calls, call)
	m.inflight++
	if m.inflight > m.maxInflight {
		m.maxInflight = m.inflight
	}
	delay := m.delay
	ov := m.overrides[call.AuthIndex]
	respond := m.respond
	m.mu.Unlock()
	defer func() { m.mu.Lock(); m.inflight--; m.mu.Unlock() }()

	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
	}
	var rep mgmtReply
	if ov != nil {
		rep = ov(call.URL)
	} else {
		rep = respond(call.AuthIndex, call.URL)
	}
	if rep.block {
		select {
		case <-r.Context().Done():
		case <-time.After(10 * time.Second):
		}
		return
	}
	for k, v := range rep.httpHeader {
		w.Header().Set(k, v)
	}
	if rep.raw != "" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(rep.raw))
		return
	}
	if rep.httpStatus != 0 && rep.httpStatus != 200 {
		w.WriteHeader(rep.httpStatus)
		_, _ = w.Write([]byte(`{"error":"secret-body-marker"}`))
		return
	}
	status := rep.status
	if status == 0 {
		status = 200
	}
	hdr := rep.header
	if hdr == nil {
		hdr = map[string][]string{"Content-Type": {"application/json"}}
	}
	b, _ := json.Marshal(map[string]any{"status_code": status, "header": hdr, "body": rep.body})
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(b)
}

func (m *mockMgmt) setOverride(authIndex string, f func(url string) mgmtReply) {
	m.mu.Lock()
	if f == nil {
		delete(m.overrides, authIndex)
	} else {
		m.overrides[authIndex] = f
	}
	m.mu.Unlock()
}

func (m *mockMgmt) reset() {
	m.mu.Lock()
	m.calls = nil
	m.maxInflight = 0
	m.mu.Unlock()
}

func (m *mockMgmt) snapshotCalls() []mgmtCall {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]mgmtCall(nil), m.calls...)
}

func (m *mockMgmt) countFor(authIndex string) int {
	n := 0
	for _, c := range m.snapshotCalls() {
		if c.AuthIndex == authIndex {
			n++
		}
	}
	return n
}

func (m *mockMgmt) peakInflight() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.maxInflight
}

func okReply(body string) mgmtReply { return mgmtReply{body: body} }

func upstreamStatus(code int) func(string) mgmtReply {
	return func(string) mgmtReply {
		return mgmtReply{status: code, body: `{"error":{"message":"secret-body-marker"}}`}
	}
}

// ---- realistic upstream bodies ----------------------------------------------

func claudeUsageBody(now time.Time, fivePct, weekPct float64) string {
	return fmt.Sprintf(`{"five_hour":{"utilization":%v,"resets_at":%q},"seven_day":{"utilization":%v,"resets_at":%q},"seven_day_opus":null,"seven_day_sonnet":null,"extra_usage":{"is_enabled":false}}`,
		fivePct, now.Add(2*time.Hour).Format(time.RFC3339), weekPct, now.Add(72*time.Hour).Format(time.RFC3339))
}

const claudeProfileMax5Body = `{"account":{"uuid":"synthetic-uuid","email":"person@example.invalid","has_claude_max":true,"has_claude_pro":false},"organization":{"uuid":"synthetic-org","name":"Example","organization_type":"claude_max","rate_limit_tier":"default_claude_max_5x","subscription_status":"active"}}`

func codexBody(now time.Time, fivePct, weekPct float64) string {
	return fmt.Sprintf(`{"user_id":"synthetic-user","account_id":"synthetic-account","email":"person@example.invalid","plan_type":"plus","rate_limit":{"allowed":true,"limit_reached":false,"primary_window":{"used_percent":%v,"limit_window_seconds":18000,"reset_at":%d},"secondary_window":{"used_percent":%v,"limit_window_seconds":604800,"reset_at":%d}},"credits":{"has_credits":false,"unlimited":false,"balance":"0"}}`,
		fivePct, now.Add(90*time.Minute).Unix(), weekPct, now.Add(60*time.Hour).Unix())
}

func openRouterKeyBody(remaining, usage float64) string {
	return fmt.Sprintf(`{"data":{"label":"sk-or-v1-synthetic-label","limit":100,"limit_remaining":%v,"usage":%v,"is_free_tier":false}}`, remaining, usage)
}

// standardResponder maps the synthetic auth indexes used across tests to
// realistic upstream replies at the fake clock's "now".
func standardResponder(clk *fakeClock) func(authIndex, url string) mgmtReply {
	return func(authIndex, url string) mgmtReply {
		now := clk.now()
		switch authIndex {
		case "ai-claude-1":
			if strings.HasSuffix(url, "/api/oauth/profile") {
				return okReply(claudeProfileMax5Body)
			}
			return okReply(claudeUsageBody(now, 40, 10))
		case "ai-codex-1":
			return okReply(codexBody(now, 31, 12))
		case "ai-codex-2":
			return okReply(codexBody(now, 20, 8))
		case "ai-codex-3":
			return okReply(codexBody(now, 90, 70))
		case "ai-or-1":
			return okReply(openRouterKeyBody(74.5, 25.5))
		}
		return mgmtReply{httpStatus: 400}
	}
}

// ---- management client -------------------------------------------------------

func newTestClient(t *testing.T, m *mockMgmt) *managementClient {
	t.Helper()
	mc, err := newManagementClient(m.srv.URL, testMgmtEnv)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(mc.close)
	return mc
}

func TestManagementClientFixedEndpointsAndEnvelope(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newMockMgmt(t, standardResponder(clk))
	mc := newTestClient(t, m)
	ctx := context.Background()

	body, err := mc.call(ctx, epCodexUsage, "ai-codex-1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"plan_type":"plus"`) {
		t.Fatalf("envelope string body not unwrapped: %.80s", body)
	}
	if _, err := mc.call(ctx, epClaudeUsage, "ai-claude-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.call(ctx, epClaudeProfile, "ai-claude-1"); err != nil {
		t.Fatal(err)
	}
	if _, err := mc.call(ctx, epOpenRouterKey, "ai-or-1"); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"ai-codex-1": "https://chatgpt.com/backend-api/wham/usage",
		"ai-or-1":    "https://openrouter.ai/api/v1/key",
	}
	calls := m.snapshotCalls()
	if len(calls) != 4 {
		t.Fatalf("calls = %d", len(calls))
	}
	for _, c := range calls {
		switch c.AuthIndex {
		case "ai-claude-1":
			if c.URL != "https://api.anthropic.com/api/oauth/usage" && c.URL != "https://api.anthropic.com/api/oauth/profile" {
				t.Errorf("claude url %q", c.URL)
			}
			if c.Header["anthropic-beta"] != "oauth-2025-04-20" {
				t.Errorf("claude request lacks anthropic-beta oauth-2025-04-20: %v", c.Header)
			}
		default:
			if c.URL != want[c.AuthIndex] {
				t.Errorf("%s url %q, want %q", c.AuthIndex, c.URL, want[c.AuthIndex])
			}
			if _, ok := c.Header["anthropic-beta"]; ok {
				t.Errorf("non-claude request carries anthropic-beta")
			}
		}
	}

	// Arbitrary endpoint ids / URLs and empty auth indexes are refused with no request.
	m.reset()
	for _, id := range []string{"https://evil.test/steal", "claude-usage?x=1", ""} {
		if _, err := mc.call(ctx, id, "ai-codex-1"); err == nil {
			t.Errorf("endpoint %q accepted", id)
		}
	}
	if _, err := mc.call(ctx, epCodexUsage, "  "); err == nil {
		t.Error("empty auth index accepted")
	}
	if n := len(m.snapshotCalls()); n != 0 {
		t.Fatalf("refused calls reached the server: %d", n)
	}

	// Missing management key: no request.
	t.Setenv(testMgmtEnv, "")
	if _, err := mc.call(ctx, epCodexUsage, "ai-codex-1"); kindOf(err) != failKeyMissing {
		t.Fatalf("missing key err = %v", err)
	}
	if len(m.snapshotCalls()) != 0 {
		t.Fatal("request sent without key")
	}
}

func TestManagementClientErrorsAreGenericAndClassified(t *testing.T) {
	clk := &fakeClock{t: t0}
	m := newMockMgmt(t, standardResponder(clk))
	mc := newTestClient(t, m)
	ctx := context.Background()
	big := strings.Repeat("a", maxUpstreamBody+10)

	cases := []struct {
		name  string
		reply mgmtReply
		kind  failKind
		retry time.Duration
	}{
		{"upstream 500", mgmtReply{status: 500, body: `{"error":"secret-body-marker"}`}, failTransient, 0},
		{"upstream 401", mgmtReply{status: 401, body: `{"error":"secret-body-marker"}`}, failAuth, 0},
		{"upstream 403", mgmtReply{status: 403, body: `secret-body-marker`}, failAuth, 0},
		{"upstream 404", mgmtReply{status: 404, body: `secret-body-marker`}, failRejected, 0},
		{"upstream 429 seconds", mgmtReply{status: 429, header: map[string][]string{"Retry-After": {"120"}}, body: `secret-body-marker`}, failRateLimited, 120 * time.Second},
		{"upstream 429 capped", mgmtReply{status: 429, header: map[string][]string{"retry-after": {"999999"}}, body: `x`}, failRateLimited, maxRetryAfter},
		{"upstream 429 no header", mgmtReply{status: 429, body: `x`}, failRateLimited, 0},
		{"mgmt 400 credential unresolved", mgmtReply{httpStatus: 400}, failAuth, 0},
		{"mgmt 401", mgmtReply{httpStatus: 401}, failKeyRejected, 0},
		{"mgmt 429", mgmtReply{httpStatus: 429, httpHeader: map[string]string{"Retry-After": "30"}}, failRateLimited, 30 * time.Second},
		{"mgmt 502", mgmtReply{httpStatus: 502}, failTransient, 0},
		{"mgmt not json", mgmtReply{raw: `<html>secret-body-marker</html>`}, failParse, 0},
		{"envelope without status", mgmtReply{raw: `{"body":"secret-body-marker"}`}, failParse, 0},
		{"upstream body too large", mgmtReply{body: big}, failTooLarge, 0},
		{"management response too large", mgmtReply{raw: `{"status_code":200,"body":"` + strings.Repeat("b", maxMgmtResponse+10) + `"}`}, failTooLarge, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m.setOverride("ai-x", func(string) mgmtReply { return tc.reply })
			body, err := mc.call(ctx, epCodexUsage, "ai-x")
			if err == nil {
				t.Fatalf("expected error, got body %.40s", body)
			}
			if kindOf(err) != tc.kind {
				t.Fatalf("kind = %d (%v), want %d", kindOf(err), err, tc.kind)
			}
			if got := retryAfterOf(err); got != tc.retry {
				t.Fatalf("retryAfter = %v, want %v", got, tc.retry)
			}
			msg := err.Error()
			for _, bad := range []string{"secret-body-marker", "chatgpt.com", testMgmtKey, "$TOKEN$", "127.0.0.1"} {
				if strings.Contains(msg, bad) {
					t.Fatalf("error leaks %q: %s", bad, msg)
				}
			}
		})
	}
}

func TestParseRetryAfterForms(t *testing.T) {
	now := t0
	if d := parseRetryAfter("45", now); d != 45*time.Second {
		t.Errorf("seconds = %v", d)
	}
	if d := parseRetryAfter(now.Add(90*time.Second).UTC().Format(http.TimeFormat), now); d < 89*time.Second || d > 90*time.Second {
		t.Errorf("http-date = %v", d)
	}
	for _, v := range []string{"", "-5", "0", "soon", now.Add(-time.Minute).UTC().Format(http.TimeFormat)} {
		if d := parseRetryAfter(v, now); d != 0 {
			t.Errorf("%q = %v, want 0", v, d)
		}
	}
	if d := parseRetryAfter(now.Add(48*time.Hour).UTC().Format(http.TimeFormat), now); d != maxRetryAfter {
		t.Errorf("far date not capped: %v", d)
	}
}

func TestManagementClientRefusesRedirects(t *testing.T) {
	var hits int
	var mu sync.Mutex
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
	}))
	defer other.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL+"/v0/management/api-call", http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	t.Setenv(testMgmtEnv, testMgmtKey)
	mc, err := newManagementClient(redir.URL, testMgmtEnv)
	if err != nil {
		t.Fatal(err)
	}
	defer mc.close()
	if _, err := mc.call(context.Background(), epCodexUsage, "ai-codex-1"); err == nil {
		t.Fatal("redirect followed")
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 0 {
		t.Fatalf("redirect target reached %d times", hits)
	}
}

func TestManagementClientRejectsNonLoopbackBase(t *testing.T) {
	for _, base := range []string{"https://127.0.0.1:8317", "http://example.com:8317", "http://localhost:8317", "http://127.0.0.1:8317/path", "http://user:pw@127.0.0.1:8317"} {
		if _, err := newManagementClient(base, "X_KEY"); err == nil {
			t.Errorf("base %q accepted", base)
		}
	}
	if _, err := newManagementClient("http://127.0.0.1:8317", "bad env!"); err == nil {
		t.Error("bad key env name accepted")
	}
}

func TestManagementClientCancelsInFlight(t *testing.T) {
	m := newMockMgmt(t, func(string, string) mgmtReply { return mgmtReply{block: true} })
	mc := newTestClient(t, m)
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(50 * time.Millisecond); cancel() }()
	start := time.Now()
	_, err := mc.call(ctx, epCodexUsage, "ai-codex-1")
	if err == nil || ctx.Err() == nil {
		t.Fatalf("expected context error, got %v", err)
	}
	if time.Since(start) > 3*time.Second {
		t.Fatal("cancel not prompt")
	}
}

func TestBackoffDelayBoundedExponential(t *testing.T) {
	want := []time.Duration{30 * time.Second, 60 * time.Second, 2 * time.Minute, 4 * time.Minute, 8 * time.Minute, 15 * time.Minute, 15 * time.Minute}
	for i, w := range want {
		if got := backoffDelay(i + 1); got != w {
			t.Errorf("backoffDelay(%d) = %v, want %v", i+1, got, w)
		}
	}
	if backoffDelay(1000) != backoffMax {
		t.Error("unbounded backoff")
	}
}

func TestSourceStatesRetryAfterOverridesBackoffAndPrunes(t *testing.T) {
	s := newSourceStates()
	s.fail("a", &sourceError{kind: failTransient}, t0)
	s.fail("a", &sourceError{kind: failTransient}, t0)
	if _, b := s.blocked("a", t0.Add(59*time.Second)); !b {
		t.Fatal("second failure should back off 60s")
	}
	if _, b := s.blocked("a", t0.Add(61*time.Second)); b {
		t.Fatal("backoff did not expire")
	}
	s.fail("b", &sourceError{kind: failRateLimited, retryAfter: 5 * time.Minute}, t0)
	if k, b := s.blocked("b", t0.Add(4*time.Minute)); !b || k != failRateLimited {
		t.Fatal("Retry-After not honored")
	}
	s.prune(map[string]bool{"b": true})
	if _, ok := s.get("a"); ok {
		t.Fatal("stale source state not pruned")
	}
	s.succeed("b")
	if _, ok := s.get("b"); ok {
		t.Fatal("success must clear state")
	}
}

// ---- inventory ---------------------------------------------------------------

func TestUnionInventoryKeepsConfigKeysAndDropsSensitiveFields(t *testing.T) {
	list := []pluginapi.HostAuthFileEntry{
		{ID: "claude-oauth.json", AuthIndex: "ai-1", Name: "claude-oauth.json", Provider: "Claude", Label: "Work\x00 Claude", AccountType: "oauth",
			Email: "person@example.invalid", Path: "/secret/dir/claude-oauth.json", Note: "private-note", BaseURL: "https://private.invalid", Account: "person@example.invalid", Priority: 3},
		{ID: "off.json", AuthIndex: "ai-off", Provider: "codex", Disabled: true},
		{ID: "retry.json", AuthIndex: "ai-retry", Provider: "codex", Unavailable: true, NextRetryAfter: t0.Add(time.Hour)},
		{Name: "no-identity.json", Provider: "codex"}, // nothing addressable: skipped
	}
	models := []pluginapi.HostAuthModels{
		{ID: "claude-oauth.json", AuthIndex: "ai-1", Provider: "claude", Models: []string{"claude-sonnet-4", " ", "claude-opus-4", "claude-sonnet-4"}},
		{ID: "openrouter-from-config", AuthIndex: "ai-or", Provider: "openrouter", Models: []string{"openrouter/auto"}},
		{AuthIndex: "ai-idx-only", Provider: "codex", Models: []string{"gpt-5-codex"}},
	}
	creds, stats, err := unionInventory(list, models)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, c := range creds {
		byID[c.ID] = i
	}
	for _, id := range []string{"claude-oauth.json", "off.json", "retry.json", "openrouter-from-config", "ai-idx-only"} {
		if _, ok := byID[id]; !ok {
			t.Fatalf("credential %q dropped: %+v", id, creds)
		}
	}
	if len(creds) != 5 {
		t.Fatalf("len = %d: %+v", len(creds), creds)
	}
	if stats.Both != 1 || stats.ListOnly != 2 || stats.ModelsOnly != 2 || stats.Unidentified != 1 {
		t.Fatalf("stats = %+v", stats)
	}
	cl := creds[byID["claude-oauth.json"]]
	if cl.Provider != "claude" || cl.AuthType != "oauth" || cl.AuthIndex != "ai-1" || cl.Priority != 3 || cl.Label != "Work Claude" {
		t.Fatalf("claude cred = %+v", cl)
	}
	if strings.Join(cl.Models, ",") != "claude-opus-4,claude-sonnet-4" {
		t.Fatalf("models = %v", cl.Models)
	}
	or := creds[byID["openrouter-from-config"]]
	if or.Label != unknownLabel || or.AuthIndex != "ai-or" || or.Provider != "openrouter" || len(or.Models) != 1 {
		t.Fatalf("models-only cred = %+v", or)
	}
	if !creds[byID["off.json"]].Disabled || !creds[byID["retry.json"]].Unavailable || !creds[byID["retry.json"]].NextRetryAt.Equal(t0.Add(time.Hour)) {
		t.Fatalf("flags lost: %+v", creds)
	}
	if c := creds[byID["ai-idx-only"]]; c.ID != "ai-idx-only" || c.AuthIndex != "ai-idx-only" {
		t.Fatalf("auth-index-only credential = %+v", c)
	}
	b, _ := json.Marshal(creds)
	for _, leak := range []string{"example.invalid", "/secret/dir", "private-note", "private.invalid"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("credential output leaks %q: %s", leak, b)
		}
	}
	for _, c := range creds {
		if c.Models == nil {
			t.Fatalf("nil Models for %q (want empty slice)", c.ID)
		}
	}
}

func TestUnionInventoryMatchesByAuthIndexWhenIDsDiffer(t *testing.T) {
	creds, _, err := unionInventory(
		[]pluginapi.HostAuthFileEntry{{ID: "a.json", AuthIndex: "idx", Provider: "codex"}},
		[]pluginapi.HostAuthModels{{ID: "", AuthIndex: "idx", Provider: "codex", Models: []string{"m1"}}},
	)
	if err != nil || len(creds) != 1 || creds[0].ID != "a.json" || len(creds[0].Models) != 1 {
		t.Fatalf("%+v %v", creds, err)
	}
}

func TestUnionInventoryBounded(t *testing.T) {
	big := make([]pluginapi.HostAuthFileEntry, maxInventoryEntries+1)
	if _, _, err := unionInventory(big, nil); err == nil {
		t.Fatal("oversized list accepted")
	}
	many := make([]string, maxModelsPerCredential+50)
	for i := range many {
		many[i] = fmt.Sprintf("model-%04d", i)
	}
	creds, _, err := unionInventory(nil, []pluginapi.HostAuthModels{{ID: "x", Provider: "codex", Models: many}})
	if err != nil || len(creds) != 1 || len(creds[0].Models) != maxModelsPerCredential {
		t.Fatalf("models not bounded: %v", err)
	}
}
