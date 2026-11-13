package main

import (
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
