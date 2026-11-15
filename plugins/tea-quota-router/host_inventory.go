package main

import (
	"context"
	"errors"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
)

// InventoryFunc returns the host credential inventory: the host.auth.list
// entries and the host.auth.models entries. The parent wires it to the native
// host callbacks. The list can omit config-defined credentials that the models
// callback still reports, so the collector always takes the union of both.
type InventoryFunc func(context.Context) ([]pluginapi.HostAuthFileEntry, []pluginapi.HostAuthModels, error)

// HostInventory is the exported name used by the CollectOnce seam.
type HostInventory = InventoryFunc

const (
	maxInventoryEntries    = 4096
	maxModelsPerCredential = 1024
	maxLabelRunes          = 128
	unknownLabel           = "unknown"
	unknownProvider        = "unknown"
)

var errInventoryTooLarge = errors.New("inventory too large")

type inventoryStats struct {
	ListOnly     int // in host.auth.list but not in host.auth.models
	ModelsOnly   int // in host.auth.models but not in host.auth.list (kept)
	Both         int
	Unidentified int // entries with neither ID nor auth index (cannot be addressed)
}

type invItem struct {
	id, idx   string
	provider  string
	cred      quota.Credential
	models    map[string]struct{}
	inList    bool
	inModels  bool
	hasRetry  bool
	labelSeen bool
}

// unionInventory merges host.auth.list and host.auth.models into safe
// credentials. Only allowlisted metadata is carried: no file name, path,
// email, account string, base URL or note ever leaves this function.
// Entries present only in the models callback are kept (config credentials).
func unionInventory(list []pluginapi.HostAuthFileEntry, models []pluginapi.HostAuthModels) ([]quota.Credential, inventoryStats, error) {
	var stats inventoryStats
	if len(list) > maxInventoryEntries || len(models) > maxInventoryEntries {
		return nil, stats, errInventoryTooLarge
	}
	byID := map[string]*invItem{}
	byIdx := map[string]*invItem{}
	var items []*invItem

	find := func(id, idx string) *invItem {
		if id != "" {
			if it := byID[id]; it != nil {
				return it
			}
		}
		if idx != "" {
			if it := byIdx[idx]; it != nil {
				return it
			}
		}
		return nil
	}
	register := func(it *invItem, id, idx string) {
		if id != "" {
			if byID[id] == nil {
				byID[id] = it
			}
			if it.id == "" {
				it.id = id
			}
		}
		if idx != "" {
			if byIdx[idx] == nil {
				byIdx[idx] = it
			}
			if it.idx == "" {
				it.idx = idx
			}
		}
	}

	for _, e := range list {
		id, idx := strings.TrimSpace(e.ID), strings.TrimSpace(e.AuthIndex)
		if id == "" && idx == "" {
			stats.Unidentified++
			continue
		}
		it := find(id, idx)
		if it == nil {
			it = &invItem{models: map[string]struct{}{}}
			items = append(items, it)
		}
		register(it, id, idx)
		if it.inList {
			continue // duplicate list entry: first wins
		}
		it.inList = true
		it.provider = normProvider(e.Provider, e.Type)
		it.cred.Label = cleanLabel(e.Label)
		it.cred.AuthType = listAuthType(e, it.provider)
		it.cred.Priority = e.Priority
		it.cred.Disabled = e.Disabled || strings.EqualFold(strings.TrimSpace(e.Status), "disabled")
		it.cred.Unavailable = e.Unavailable
		if !e.NextRetryAfter.IsZero() {
			it.cred.NextRetryAt = e.NextRetryAfter.UTC()
		}
	}
	for _, m := range models {
		id, idx := strings.TrimSpace(m.ID), strings.TrimSpace(m.AuthIndex)
		if id == "" && idx == "" {
			stats.Unidentified++
			continue
		}
		it := find(id, idx)
		if it == nil {
			it = &invItem{models: map[string]struct{}{}}
			items = append(items, it)
		}
		register(it, id, idx)
		it.inModels = true
		if it.provider == "" {
			it.provider = normProvider(m.Provider, "")
		}
		for _, name := range m.Models {
			name = strings.TrimSpace(name)
			if name == "" || len(name) > 256 {
				continue
			}
			if len(it.models) >= maxModelsPerCredential {
				break
			}
			it.models[name] = struct{}{}
		}
	}

	out := make([]quota.Credential, 0, len(items))
	seen := map[string]int{}
	for _, it := range items {
		c := it.cred
		c.ID = it.id
		if c.ID == "" {
			c.ID = it.idx
		}
		c.AuthIndex = it.idx
		if c.AuthType == "" {
			// Config-defined credentials (usually models-callback only) carry
			// the host's "<kind>:apikey:<hash>" or
			// "openai-compatibility:<name>:<hash>" ID; both are API keys.
			c.AuthType = configIDAuthType(it.id)
		}
		c.Provider = it.provider
		if c.Provider == "" {
			c.Provider = unknownProvider
		}
		if c.Label == "" {
			c.Label = unknownLabel
		}
		for name := range it.models {
			c.Models = append(c.Models, name)
		}
		sort.Strings(c.Models)
		if c.Models == nil {
			c.Models = []string{}
		}
		if at, dup := seen[c.ID]; dup { // distinct entries resolving to one ID: merge, never drop models
			merged := map[string]struct{}{}
			for _, n := range append(out[at].Models, c.Models...) {
				merged[n] = struct{}{}
			}
			out[at].Models = out[at].Models[:0]
			for n := range merged {
				out[at].Models = append(out[at].Models, n)
			}
			sort.Strings(out[at].Models)
			continue
		}
		seen[c.ID] = len(out)
		out = append(out, c)
		switch {
		case it.inList && it.inModels:
			stats.Both++
		case it.inList:
			stats.ListOnly++
		default:
			stats.ModelsOnly++
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, stats, nil
}

func normProvider(provider, typ string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		p = strings.ToLower(strings.TrimSpace(typ))
	}
	if len(p) > 64 {
		p = p[:64]
	}
	return p
}

// authTypeOf maps the host's AccountType (derived from the credential kind)
// to a normalized auth type. Only exact known values classify; anything else
// stays unknown ("").
func authTypeOf(accountType string) string {
	switch strings.ToLower(strings.TrimSpace(accountType)) {
	case "oauth":
		return "oauth"
	case "api_key", "apikey", "api-key":
		return "api_key"
	}
	return ""
}

// listAuthType classifies a host.auth.list entry. An explicit AccountType
// wins. Without one, a file-backed JSON auth is called OAuth only when every
// piece of evidence lines up: a Claude/Codex provider, a ".json" ID or name,
// a real file (not runtime-only), an account email (OAuth files carry one)
// and no custom base URL (a configured endpoint points at an API key). Any
// gap leaves the type unknown, which Claude/Codex fetching treats as
// ineligible.
func listAuthType(e pluginapi.HostAuthFileEntry, provider string) string {
	if t := authTypeOf(e.AccountType); t != "" {
		return t
	}
	if provider != "claude" && provider != "codex" {
		return ""
	}
	if e.RuntimeOnly || strings.TrimSpace(e.BaseURL) != "" || strings.TrimSpace(e.Email) == "" {
		return ""
	}
	if hasJSONSuffix(e.ID) || hasJSONSuffix(e.Name) {
		return "oauth"
	}
	return ""
}

func hasJSONSuffix(s string) bool {
	return strings.HasSuffix(strings.ToLower(strings.TrimSpace(s)), ".json")
}

// The host's stable config-credential IDs end in a 12-hex-digit hash with an
// optional "-<n>" duplicate counter.
var (
	configAPIKeyID = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]*:apikey:[0-9a-f]{12}(-[0-9]+)?$`)
	configCompatID = regexp.MustCompile(`^openai-compatibility:[^:\s]+:[0-9a-f]{12}(-[0-9]+)?$`)
)

// configIDAuthType returns "api_key" for a documented config-credential ID
// (claude:apikey:<hash>, codex:apikey:<hash>, gemini:apikey:<hash>,
// openai-compatibility:<name>:<hash>, ...) and "" otherwise.
func configIDAuthType(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	if configAPIKeyID.MatchString(id) || configCompatID.MatchString(id) {
		return "api_key"
	}
	return ""
}

func cleanLabel(s string) string {
	var b strings.Builder
	n := 0
	for _, r := range strings.TrimSpace(s) {
		if unicode.IsControl(r) || r == unicode.ReplacementChar {
			continue
		}
		if n >= maxLabelRunes {
			break
		}
		b.WriteRune(r)
		n++
	}
	return strings.TrimSpace(b.String())
}
