package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/hex"
	"sync"
	"time"
)

const (
	accountBindingVersion  = "tea-combos-account-v1"
	accountBindingCapacity = 10000
)

// accountBindingKey scopes an account binding to the original trusted caller, the session and
// the combo/member/pool/strategy. It returns "" when either the caller scope or the session is
// missing: a binding without a caller namespace would be shared by every caller, so it is never
// created. Only a hash is ever kept.
func accountBindingKey(callerScope, sessionIdentity string, combo Combo, member Member, strategy string) string {
	if callerScope == "" || sessionIdentity == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(accountBindingVersion + "\x00" + callerScope + "\x00" + sessionIdentity + "\x00" +
		combo.Name + "\x00" + member.Model + "\x00" + member.Provider + "\x00" + member.AccountPool + "\x00" + strategy + pinIdentity(member)))
	return hex.EncodeToString(sum[:])
}

type accountBinding struct {
	key        string
	credential string
	expiresAt  time.Time
}

// accountBindings is a bounded in-memory LRU of session -> credential. It is intentionally not
// persisted: a restart should re-evaluate quota rather than resurrect an old pin.
type accountBindings struct {
	mu       sync.Mutex
	capacity int
	entries  map[string]*list.Element
	lru      *list.List
	now      func() time.Time
}

func newAccountBindings(now func() time.Time) *accountBindings {
	if now == nil {
		now = time.Now
	}
	return &accountBindings{capacity: accountBindingCapacity, entries: map[string]*list.Element{}, lru: list.New(), now: now}
}

func (b *accountBindings) Lookup(key string) (string, bool) {
	if b == nil || key == "" {
		return "", false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	element, ok := b.entries[key]
	if !ok {
		return "", false
	}
	entry := element.Value.(*accountBinding)
	if !entry.expiresAt.After(b.now()) {
		b.removeLocked(element)
		return "", false
	}
	b.lru.MoveToFront(element)
	return entry.credential, true
}

func (b *accountBindings) Bind(key, credential string, ttl time.Duration) {
	if b == nil || key == "" || credential == "" || ttl <= 0 {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	entry := &accountBinding{key: key, credential: credential, expiresAt: b.now().Add(ttl)}
	if element, ok := b.entries[key]; ok {
		element.Value = entry
		b.lru.MoveToFront(element)
		return
	}
	b.entries[key] = b.lru.PushFront(entry)
	for len(b.entries) > b.capacity {
		oldest := b.lru.Back()
		if oldest == nil {
			break
		}
		b.removeLocked(oldest)
	}
}

func (b *accountBindings) Remove(key string) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if element, ok := b.entries[key]; ok {
		b.removeLocked(element)
	}
}

func (b *accountBindings) removeLocked(element *list.Element) {
	entry := element.Value.(*accountBinding)
	b.lru.Remove(element)
	delete(b.entries, entry.key)
}
