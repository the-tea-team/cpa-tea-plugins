package main

import (
	"strings"
	"testing"
	"time"
)

func TestAccountBindingKeyRequiresTrustedCallerAndSession(t *testing.T) {
	combo := Combo{Name: "c"}
	member := Member{Model: "m", AccountPool: "p"}
	if key := accountBindingKey("", "sess", combo, member, AccountStrategyRoundRobin); key != "" {
		t.Fatalf("missing caller scope must never produce a (global) binding key, got %q", key)
	}
	if key := accountBindingKey("caller", "", combo, member, AccountStrategyRoundRobin); key != "" {
		t.Fatalf("missing session must not bind, got %q", key)
	}
	key := accountBindingKey("caller-1", "sess", combo, member, AccountStrategyRoundRobin)
	if key == "" || strings.Contains(key, "caller-1") || strings.Contains(key, "sess") {
		t.Fatalf("key must be a hash, got %q", key)
	}
	other := accountBindingKey("caller-2", "sess", combo, member, AccountStrategyRoundRobin)
	if other == key {
		t.Fatal("different callers must not share a binding key")
	}
	if accountBindingKey("caller-1", "sess", Combo{Name: "d"}, member, AccountStrategyRoundRobin) == key {
		t.Fatal("different combos must not share a binding key")
	}
	if accountBindingKey("caller-1", "sess", combo, Member{Model: "m", AccountPool: "q"}, AccountStrategyRoundRobin) == key {
		t.Fatal("different pools must not share a binding key")
	}
	if accountBindingKey("caller-1", "sess", combo, member, AccountStrategyQuotaAware) == key {
		t.Fatal("different strategies must not share a binding key")
	}
}

func TestAccountBindingsTTLRemoveAndCapacity(t *testing.T) {
	now := time.Date(2026, 11, 15, 0, 0, 0, 0, time.UTC)
	b := newAccountBindings(func() time.Time { return now })
	b.Bind("k", "cred-1", time.Minute)
	if got, ok := b.Lookup("k"); !ok || got != "cred-1" {
		t.Fatalf("lookup = %q %v", got, ok)
	}
	now = now.Add(2 * time.Minute)
	if _, ok := b.Lookup("k"); ok {
		t.Fatal("expired binding must not resolve")
	}
	b.Bind("k", "cred-1", time.Hour)
	b.Remove("k")
	if _, ok := b.Lookup("k"); ok {
		t.Fatal("removed binding must not resolve")
	}
	b.Bind("", "cred", time.Hour)
	if _, ok := b.Lookup(""); ok {
		t.Fatal("empty key must never bind")
	}
	small := newAccountBindings(func() time.Time { return now })
	small.capacity = 2
	small.Bind("a", "1", time.Hour)
	small.Bind("b", "2", time.Hour)
	small.Lookup("a")
	small.Bind("c", "3", time.Hour)
	if _, ok := small.Lookup("b"); ok {
		t.Fatal("least recently used binding must be evicted")
	}
	if _, ok := small.Lookup("a"); !ok {
		t.Fatal("recently used binding must survive")
	}
}

func TestNilAccountBindingsAreInert(t *testing.T) {
	var b *accountBindings
	b.Bind("k", "c", time.Hour)
	b.Remove("k")
	if _, ok := b.Lookup("k"); ok {
		t.Fatal("nil bindings must not resolve")
	}
}
