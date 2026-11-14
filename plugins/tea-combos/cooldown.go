package main

import (
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// cooldownTracker remembers which combo members failed recently so selection can skip them.
// It is process-local on purpose: a restart forgets cooling, which is the right bias when the
// upstream may already have recovered. All methods are nil-safe so zero-valued plugins and
// tests keep working without wiring a tracker.
type cooldownTracker struct {
	mu    sync.Mutex
	until map[string]time.Time
	now   func() time.Time
}

func newCooldownTracker() *cooldownTracker {
	return &cooldownTracker{until: make(map[string]time.Time), now: time.Now}
}

func cooldownKey(comboName string, member Member) string {
	return comboName + "\x00" + member.Provider + "\x00" + member.Model
}

// Cool parks a member for d. A zero or negative duration is a no-op, which is how cooling
// stays disabled when cooldownSeconds is configured to 0.
func (t *cooldownTracker) Cool(comboName string, member Member, d time.Duration) {
	if t == nil || d <= 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.until[cooldownKey(comboName, member)] = t.now().Add(d)
}

// Clear forgets cooling once a member serves successfully, so recovery is immediate.
func (t *cooldownTracker) Clear(comboName string, member Member) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.until, cooldownKey(comboName, member))
}

// Active reports whether a member is cooling, pruning the entry once the window has passed.
func (t *cooldownTracker) Active(comboName string, member Member) bool {
	if t == nil {
		return false
	}
	key := cooldownKey(comboName, member)
	t.mu.Lock()
	defer t.mu.Unlock()
	expiry, ok := t.until[key]
	if !ok {
		return false
	}
	if !t.now().Before(expiry) {
		delete(t.until, key)
		return false
	}
	return true
}

// Cooling lists the member models currently cooling, for the exhausted-chain message.
func (t *cooldownTracker) Cooling(comboName string, members []Member) []string {
	if t == nil {
		return nil
	}
	var cooling []string
	for _, member := range members {
		if t.Active(comboName, member) {
			cooling = append(cooling, member.Model)
		}
	}
	return cooling
}

// parseRetryHint extracts an upstream "come back in X" signal. Response headers win over
// message text: Retry-After (delta-seconds or HTTP-date) is what CPA relays when the upstream
// sent it, and what CPA synthesizes from Claude's 5h/7d unified-cooldown metadata when every
// credential for the model is cooling. The message scan is a best-effort fallback for quota
// errors whose reset time only appears in prose ("try again in 4.5 hours"). A zero duration
// means the upstream accepts an immediate retry. Nil means "no signal, use the default window".
func parseRetryHint(headers map[string][]string, message string) *time.Duration {
	for name, values := range headers {
		if !strings.EqualFold(strings.TrimSpace(name), "Retry-After") {
			continue
		}
		for _, raw := range values {
			if d, ok := parseRetryAfterValue(raw, time.Now()); ok {
				return &d
			}
		}
	}
	if d, ok := parseRetryAfterProse(message); ok {
		return &d
	}
	return nil
}

func parseRetryAfterValue(raw string, now time.Time) (time.Duration, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return 0, false
	}
	if seconds, errParse := strconv.Atoi(raw); errParse == nil {
		if seconds < 0 {
			return 0, true
		}
		return time.Duration(seconds) * time.Second, true
	}
	if when, errParse := http.ParseTime(raw); errParse == nil {
		d := when.Sub(now)
		if d < 0 {
			return 0, true
		}
		return d, true
	}
	return 0, false
}

var retryAfterProse = regexp.MustCompile(`(?i)retry[-_ ]after["': ]+(\d+)`)
var tryAgainInProse = regexp.MustCompile(`(?i)try again in (\d+(?:\.\d+)?)\s*(second|minute|hour|day)s?`)

func parseRetryAfterProse(message string) (time.Duration, bool) {
	if m := retryAfterProse.FindStringSubmatch(message); m != nil {
		if seconds, errParse := strconv.Atoi(m[1]); errParse == nil {
			return time.Duration(seconds) * time.Second, true
		}
	}
	m := tryAgainInProse.FindStringSubmatch(message)
	if m == nil {
		return 0, false
	}
	amount, errParse := strconv.ParseFloat(m[1], 64)
	if errParse != nil {
		return 0, false
	}
	var unit time.Duration
	switch strings.ToLower(m[2]) {
	case "second":
		unit = time.Second
	case "minute":
		unit = time.Minute
	case "hour":
		unit = time.Hour
	case "day":
		unit = 24 * time.Hour
	default:
		return 0, false
	}
	return time.Duration(amount * float64(unit)), true
}
