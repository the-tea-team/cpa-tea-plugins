package main

import (
	"container/list"
	"crypto/sha256"
	"encoding/binary"
	"math"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

const (
	affinityTTL         = 60 * time.Minute
	affinityMaxBindings = 4096

	canonicalSessionMetadataKey = "canonical_session_id"
	executionSessionMetadataKey = "execution_session_id"

	reasonAffinity = "affinity"
	reasonProposed = "proposed"
	reasonRejected = "rejected"
)

// affinityKey is the SHA-256 of the length-prefixed tuple (caller scope,
// provider scope, model, session). Raw identifiers never leave affinityKeyFor.
type affinityKey [sha256.Size]byte

// affinityKeyFor derives the binding key from host-provided scheduler
// Options.Metadata only. caller_scope is a namespace, not proof of identity.
// Client headers are never consulted. Both a non-empty caller scope and a
// non-empty session id (canonical preferred, then execution) are required;
// otherwise the request has no affinity.
func affinityKeyFor(req pluginapi.SchedulerPickRequest) (affinityKey, bool) {
	md := req.Options.Metadata
	caller := metadataString(md, pluginapi.CallerScopeMetadataKey)
	session := metadataString(md, canonicalSessionMetadataKey)
	if session == "" {
		session = metadataString(md, executionSessionMetadataKey)
	}
	if caller == "" || session == "" {
		return affinityKey{}, false
	}
	providers := append([]string{}, req.Providers...)
	sort.Strings(providers)
	h := sha256.New()
	for _, part := range []string{"tea-quota-affinity-v1", caller, req.Provider, strings.Join(providers, ","), req.Model, session} {
		var n [8]byte
		binary.BigEndian.PutUint64(n[:], uint64(len(part)))
		h.Write(n[:])
		h.Write([]byte(part))
	}
	var k affinityKey
	copy(k[:], h.Sum(nil))
	return k, true
}

func metadataString(md map[string]any, key string) string {
	if v, ok := md[key].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

type affinityEntry struct {
	key     affinityKey
	authID  string
	expires time.Time
}

// affinityTable is a bounded, TTL'd, LRU map of hashed session keys to auth
// IDs. The TTL is idle-based: every honored hit extends it. All methods with
// the Locked suffix require mu; pick holds mu across lookup, selection and
// bind so concurrent first requests of one session cannot bind two accounts.
type affinityTable struct {
	mu   sync.Mutex
	ttl  time.Duration
	max  int
	byID map[affinityKey]*list.Element
	lru  *list.List // front = most recently used
}

func newAffinityTable(ttl time.Duration, max int) *affinityTable {
	return &affinityTable{ttl: ttl, max: max, byID: map[affinityKey]*list.Element{}, lru: list.New()}
}

func (t *affinityTable) removeLocked(e *list.Element) {
	delete(t.byID, e.Value.(*affinityEntry).key)
	t.lru.Remove(e)
}

// pruneLocked drops expired entries from the cold end.
func (t *affinityTable) pruneLocked(now time.Time) {
	for e := t.lru.Back(); e != nil; e = t.lru.Back() {
		if now.Before(e.Value.(*affinityEntry).expires) {
			return
		}
		t.removeLocked(e)
	}
}

func (t *affinityTable) getLocked(k affinityKey, now time.Time) (string, bool) {
	e, ok := t.byID[k]
	if !ok {
		return "", false
	}
	en := e.Value.(*affinityEntry)
	if !now.Before(en.expires) {
		t.removeLocked(e)
		return "", false
	}
	return en.authID, true
}

func (t *affinityTable) bindLocked(k affinityKey, id string, now time.Time) {
	if e, ok := t.byID[k]; ok {
		en := e.Value.(*affinityEntry)
		en.authID, en.expires = id, now.Add(t.ttl)
		t.lru.MoveToFront(e)
		return
	}
	for len(t.byID) >= t.max && t.lru.Len() > 0 {
		t.removeLocked(t.lru.Back())
	}
	t.byID[k] = t.lru.PushFront(&affinityEntry{key: k, authID: id, expires: now.Add(t.ttl)})
}

func (t *affinityTable) dropLocked(k affinityKey) {
	if e, ok := t.byID[k]; ok {
		t.removeLocked(e)
	}
}

func (t *affinityTable) len() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.byID)
}

// knownExhaustion reports whether account a is KNOWN exhausted for model: a
// matching window that is fresh, valid and fully used with a known future
// reset. A zero reset is an unknown replenishment time, so it is not known
// exhausted; a reset in the past or a stale observation is unknown too. The
// returned time is when the last exhausted window resets.
func knownExhaustion(a quota.Account, model string, now time.Time, maxAge time.Duration) (time.Time, bool) {
	var until time.Time
	for _, w := range a.Windows {
		match := len(w.Models) == 0
		for _, pat := range w.Models {
			ok, _ := path.Match(pat, model)
			match = match || ok || pat == model
		}
		if !match || w.UsedFraction == nil || math.IsNaN(*w.UsedFraction) || *w.UsedFraction < 1 {
			continue
		}
		if w.ObservedAt.IsZero() || w.ObservedAt.After(now) || now.Sub(w.ObservedAt) > maxAge {
			continue
		}
		if w.ResetAt.IsZero() || !w.ResetAt.After(now) {
			continue
		}
		if w.ResetAt.After(until) {
			until = w.ResetAt
		}
	}
	return until, !until.IsZero()
}
