package main

import (
	"container/list"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	// DefaultAffinityTTL is how long one session keeps its member when a combo sets no ttl.
	DefaultAffinityTTL = time.Hour
	// DefaultAffinityCapacity bounds the in-memory binding map; the least recently used binding
	// is evicted first. State persistence mirrors the same bounded set.
	DefaultAffinityCapacity = 10000
	// affinityFlushInterval debounces state-file writes so request latency is never tied to disk.
	affinityFlushInterval = 2 * time.Second
)

// AffinityBinding is one persisted (combo, session) -> member binding. Key is the SHA-256
// session key scoped to the combo; no raw prompt text or header value is ever stored.
type AffinityBinding struct {
	Key            string    `json:"key"`
	Combo          string    `json:"combo"`
	MemberIndex    int       `json:"memberIndex"`
	MemberModel    string    `json:"memberModel"`
	MemberProvider string    `json:"memberProvider,omitempty"`
	ExpiresAt      time.Time `json:"expiresAt"`
}

type affinityEntry struct {
	binding AffinityBinding
	element *list.Element
}

// Affinity keeps session bindings in memory with a fixed LRU cap and mirrors them to the state
// file through a write-behind callback. A failed write never fails a request: it is remembered
// and retried on the next flush.
type Affinity struct {
	mu       sync.Mutex
	capacity int
	entries  map[string]*affinityEntry
	lru      *list.List
	now      func() time.Time
	persist  func(map[string]AffinityBinding) error
	dirty    bool
	lastErr  error

	wake      chan struct{}
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

// newAffinity builds the cache. When persist is nil the cache is memory-only and starts no
// background goroutine (tests and degenerate deployments). Expired entries in initial are
// dropped immediately, so a restart never resurrects a stale conversation.
func newAffinity(initial map[string]AffinityBinding, persist func(map[string]AffinityBinding) error) *Affinity {
	return newAffinityWithClock(initial, persist, time.Now)
}

func newAffinityWithClock(initial map[string]AffinityBinding, persist func(map[string]AffinityBinding) error, now func() time.Time) *Affinity {
	if now == nil {
		now = time.Now
	}
	affinity := &Affinity{
		capacity: DefaultAffinityCapacity,
		entries:  make(map[string]*affinityEntry, len(initial)),
		lru:      list.New(),
		now:      now,
		persist:  persist,
	}
	for key, binding := range initial {
		if key == "" || !binding.ExpiresAt.After(now()) {
			continue
		}
		binding.Key = key
		affinity.insertLocked(binding)
	}
	if persist != nil {
		affinity.wake = make(chan struct{}, 1)
		affinity.stop = make(chan struct{})
		affinity.done = make(chan struct{})
		go affinity.run()
	}
	return affinity
}

// Resolve returns the session key for this request and the member the session is bound to.
// key is empty when no identity can be derived or affinity is disabled; that request has no
// affinity. ok is true only when a live binding whose member identity still matches exists.
// A binding is dropped when it expired, its index is gone, or the member at that index no
// longer has the same model and provider (a runtime combo edit invalidates it).
func (a *Affinity) Resolve(combo Combo, req pluginapi.ExecutorRequest) (string, int, bool) {
	if a == nil || !comboAffinityEnabled(combo) {
		return "", -1, false
	}
	key, ok := deriveSessionKey(combo, req)
	if !ok {
		return "", -1, false
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	entry, exists := a.entries[key]
	if !exists {
		return key, -1, false
	}
	binding := entry.binding
	if !binding.ExpiresAt.After(a.now()) {
		a.removeLocked(key)
		return key, -1, false
	}
	index := binding.MemberIndex
	if index < 0 || index >= len(combo.Members) {
		a.removeLocked(key)
		return key, -1, false
	}
	member := combo.Members[index]
	if member.Model != binding.MemberModel || member.Provider != binding.MemberProvider {
		a.removeLocked(key)
		return key, -1, false
	}
	a.lru.MoveToFront(entry.element)
	return key, index, true
}

// Bind records a successful execution for one session and refreshes its expiry. The TTL is
// sliding: every successful use extends the binding by the combo's ttl (default one hour).
// An empty key is a no-op, which is how a request with no derivable identity stays unbound.
func (a *Affinity) Bind(key string, memberIndex int, combo Combo) {
	if a == nil || key == "" || memberIndex < 0 || memberIndex >= len(combo.Members) {
		return
	}
	member := combo.Members[memberIndex]
	binding := AffinityBinding{
		Key:            key,
		Combo:          combo.Name,
		MemberIndex:    memberIndex,
		MemberModel:    member.Model,
		MemberProvider: member.Provider,
		ExpiresAt:      a.now().Add(comboAffinityTTL(combo)),
	}

	a.mu.Lock()
	a.insertLocked(binding)
	a.dirty = true
	a.mu.Unlock()
	a.signal()
}

// Flush writes the live binding set through the persistence callback. It is safe to call
// concurrently; entries added after the snapshot stay dirty and are written by the next flush.
func (a *Affinity) Flush() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	if !a.dirty || a.persist == nil {
		err := a.lastErr
		a.mu.Unlock()
		return err
	}
	now := a.now()
	snapshot := make(map[string]AffinityBinding, len(a.entries))
	for key, entry := range a.entries {
		if entry.binding.ExpiresAt.After(now) {
			snapshot[key] = entry.binding
		}
	}
	persist := a.persist
	a.dirty = false
	a.mu.Unlock()

	err := persist(snapshot)

	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil {
		a.lastErr = err
		a.dirty = true
		return err
	}
	a.lastErr = nil
	return nil
}

// LastError reports the most recent persistence failure. Requests never see it; operators can
// surface it from the management API once slice D adds one.
func (a *Affinity) LastError() error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.lastErr
}

// Close stops the write-behind goroutine and flushes the final binding set. Repeated calls are
// safe.
func (a *Affinity) Close() {
	if a == nil {
		return
	}
	a.closeOnce.Do(func() {
		if a.stop != nil {
			close(a.stop)
			<-a.done
			return
		}
		_ = a.Flush()
	})
}

func (a *Affinity) run() {
	defer close(a.done)
	timer := time.NewTimer(affinityFlushInterval)
	if !timer.Stop() {
		<-timer.C
	}
	var timerC <-chan time.Time
	for {
		select {
		case <-a.wake:
			if timerC == nil {
				timer.Reset(affinityFlushInterval)
				timerC = timer.C
			}
		case <-timerC:
			timerC = nil
			if err := a.Flush(); err != nil {
				a.signal()
			}
		case <-a.stop:
			_ = a.Flush()
			return
		}
	}
}

func (a *Affinity) signal() {
	if a.wake == nil {
		return
	}
	select {
	case a.wake <- struct{}{}:
	default:
	}
}

// insertLocked stores or refreshes a binding and evicts the least recently used entry above the
// cap. Callers hold a.mu.
func (a *Affinity) insertLocked(binding AffinityBinding) {
	if existing := a.entries[binding.Key]; existing != nil {
		existing.binding = binding
		a.lru.MoveToFront(existing.element)
		return
	}
	element := a.lru.PushFront(binding.Key)
	a.entries[binding.Key] = &affinityEntry{binding: binding, element: element}
	for len(a.entries) > a.capacity {
		oldest := a.lru.Back()
		if oldest == nil {
			break
		}
		a.lru.Remove(oldest)
		delete(a.entries, oldest.Value.(string))
	}
}

func (a *Affinity) removeLocked(key string) {
	entry, exists := a.entries[key]
	if !exists {
		return
	}
	a.lru.Remove(entry.element)
	delete(a.entries, key)
	a.dirty = true
}

// comboAffinityEnabled defaults to on; only an explicit false disables it.
func comboAffinityEnabled(combo Combo) bool {
	return combo.SessionAffinity == nil || *combo.SessionAffinity
}

// comboAffinityTTL returns the combo's validated ttl, or the default.
func comboAffinityTTL(combo Combo) time.Duration {
	if combo.TTL == "" {
		return DefaultAffinityTTL
	}
	ttl, err := time.ParseDuration(combo.TTL)
	if err != nil || ttl <= 0 {
		return DefaultAffinityTTL
	}
	return ttl
}
