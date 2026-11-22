package main

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"gopkg.in/yaml.v3"
)

const (
	// StrategyRoundRobin rotates members per request.
	StrategyRoundRobin = "round-robin"
	// StrategyFailover tries members in their configured order.
	StrategyFailover   = "failover"
	StrategyWeighted   = "weighted-round-robin"
	StrategyQuotaAware = "quota-aware-round-robin"
)

const (
	// AccountStrategyInherit leaves account selection to CPA core (no pin).
	AccountStrategyInherit = "inherit"
	// AccountStrategyRoundRobin rotates equally across the quota accounts of a member's pool.
	AccountStrategyRoundRobin = "round-robin"
	// AccountStrategyQuotaAware weights quota accounts of a member's pool by AccountScore.
	AccountStrategyQuotaAware = "quota-aware-round-robin"
	// maxAccountAttempts bounds how many credentials one member may try per request.
	maxAccountAttempts = 8
)

// Member is one upstream model a combo can execute.
type Member struct {
	Model     string  `json:"model" yaml:"model"`
	Provider  string  `json:"provider,omitempty" yaml:"provider,omitempty"`
	Weight    float64 `json:"weight,omitempty" yaml:"weight,omitempty"`
	Role      string  `json:"role,omitempty" yaml:"role,omitempty"`
	QuotaPool string  `json:"quotaPool,omitempty" yaml:"quotaPool,omitempty"`
	// AccountStrategy overrides the combo account policy for this member. Empty means the combo
	// policy; "inherit" means unpinned core selection.
	AccountStrategy string `json:"accountStrategy,omitempty" yaml:"accountStrategy,omitempty"`
	// AccountPool is the explicit eligibility boundary for account pinning. It is distinct from
	// QuotaPool, which only shapes member weighting and never grants permission.
	AccountPool string `json:"accountPool,omitempty" yaml:"accountPool,omitempty"`
	// AccountID optionally pins the member to one quota account (opaque snapshot id).
	AccountID string `json:"accountId,omitempty" yaml:"accountId,omitempty"`
	// ConnectionID optionally pins the member to one native credential (opaque snapshot id, not
	// an auth index). It is mutually exclusive with AccountID.
	ConnectionID string `json:"connectionId,omitempty" yaml:"connectionId,omitempty"`
}

// Pinned reports whether the member is pinned to one account or one connection.
func (m Member) Pinned() bool {
	return m.AccountID != "" || m.ConnectionID != ""
}

// EffectiveAccountStrategy resolves the member override against the combo policy. The result is
// "" or "inherit" for core behaviour, otherwise an explicit pinning strategy.
func (m Member) EffectiveAccountStrategy(combo Combo) string {
	strategy := combo.AccountStrategy
	if m.AccountStrategy != "" {
		strategy = m.AccountStrategy
	}
	// A pin always pins: with no explicit policy (empty or inherit) it rotates round-robin
	// within its target, so it overrides unpinned core behaviour. Explicit policies are kept.
	if m.Pinned() && (strategy == "" || strategy == AccountStrategyInherit) {
		return AccountStrategyRoundRobin
	}
	return strategy
}

// ExplicitAccountStrategy reports whether the member pins accounts itself.
func (m Member) ExplicitAccountStrategy(combo Combo) bool {
	switch m.EffectiveAccountStrategy(combo) {
	case AccountStrategyRoundRobin, AccountStrategyQuotaAware:
		return true
	}
	return false
}

func validAccountStrategy(strategy string) bool {
	switch strategy {
	case "", AccountStrategyInherit, AccountStrategyRoundRobin, AccountStrategyQuotaAware:
		return true
	}
	return false
}

// Combo is a named set of members with a selection strategy.
type Combo struct {
	Name            string   `json:"name" yaml:"name"`
	AccountStrategy string   `json:"accountStrategy,omitempty" yaml:"accountStrategy,omitempty"`
	Strategy        string   `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	Members         []Member `json:"members" yaml:"members"`
	SessionAffinity *bool    `json:"sessionAffinity,omitempty" yaml:"sessionAffinity,omitempty"`
	TTL             string   `json:"ttl,omitempty" yaml:"ttl,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty" yaml:"enabled"`
	// CooldownSeconds overrides the plugin-wide cooling window for this combo. Nil means use
	// the plugin default; zero disables cooling for this combo.
	CooldownSeconds *int `json:"cooldownSeconds,omitempty" yaml:"cooldownSeconds,omitempty"`
}

// IsEnabled reports whether the combo may be routed. An unset enabled flag means enabled.
func (c Combo) IsEnabled() bool {
	return c.Enabled == nil || *c.Enabled
}

// Normalize trims user text, applies the documented defaults and canonicalises the strategy.
// It does not validate; call Validate afterwards.
func (c Combo) Normalize() Combo {
	c.Name = strings.TrimSpace(c.Name)
	c.Strategy = strings.TrimSpace(c.Strategy)
	if c.Strategy == "" {
		c.Strategy = StrategyRoundRobin
	}
	c.TTL = strings.TrimSpace(c.TTL)
	c.AccountStrategy = strings.TrimSpace(c.AccountStrategy)
	if c.Enabled == nil {
		enabled := true
		c.Enabled = &enabled
	}
	members := make([]Member, 0, len(c.Members))
	for _, member := range c.Members {
		member.Model = strings.TrimSpace(member.Model)
		member.Provider = strings.TrimSpace(member.Provider)
		member.Role = strings.TrimSpace(member.Role)
		member.QuotaPool = strings.TrimSpace(member.QuotaPool)
		member.AccountStrategy = strings.TrimSpace(member.AccountStrategy)
		member.AccountPool = strings.TrimSpace(member.AccountPool)
		member.AccountID = strings.TrimSpace(member.AccountID)
		member.ConnectionID = strings.TrimSpace(member.ConnectionID)
		members = append(members, member)
	}
	c.Members = members
	return c
}

// Validate rejects combos that could never be routed unambiguously.
func (c Combo) Validate() error {
	if !validAccountStrategy(c.AccountStrategy) {
		return fmt.Errorf("combo %q: unknown accountStrategy %q (want inherit, %s or %s)", c.Name, c.AccountStrategy, AccountStrategyRoundRobin, AccountStrategyQuotaAware)
	}
	if c.Name == "" {
		return fmt.Errorf("combo name is required")
	}
	if strings.Contains(c.Name, "/") {
		return fmt.Errorf("combo name %q must not contain %q", c.Name, "/")
	}
	if _, err := normalizeStrategy(c.Strategy); err != nil {
		return fmt.Errorf("combo %q: %w", c.Name, err)
	}
	if c.TTL != "" {
		ttl, err := time.ParseDuration(c.TTL)
		if err != nil {
			return fmt.Errorf("combo %q: invalid ttl %q: %w", c.Name, c.TTL, err)
		}
		if ttl <= 0 {
			return fmt.Errorf("combo %q: ttl must be positive, got %q", c.Name, c.TTL)
		}
	}
	for index, member := range c.Members {
		if member.Weight < 0 || math.IsNaN(member.Weight) || math.IsInf(member.Weight, 0) {
			return fmt.Errorf("combo %q: member %d weight must be positive", c.Name, index)
		}
		if member.Role != "" && member.Role != "primary" && member.Role != "fallback" {
			return fmt.Errorf("combo %q: member %d invalid role %q", c.Name, index, member.Role)
		}
		if member.Model == "" {
			return fmt.Errorf("combo %q: member %d has an empty model", c.Name, index)
		}
		if !validAccountStrategy(member.AccountStrategy) {
			return fmt.Errorf("combo %q: member %d has unknown accountStrategy %q", c.Name, index, member.AccountStrategy)
		}
		if member.AccountID != "" && member.ConnectionID != "" {
			return fmt.Errorf("combo %q: member %d (%s) accountId and connectionId are mutually exclusive", c.Name, index, member.Model)
		}
		// accountPool is the pool boundary: explicit strategies require it unless the member is
		// pinned (a pin is its own boundary; a supplied pool then only narrows further). It is
		// meaningless for members that neither pin nor use a strategy. quotaPool never
		// substitutes for it.
		explicit := member.ExplicitAccountStrategy(c)
		if explicit && member.AccountPool == "" && !member.Pinned() {
			return fmt.Errorf("combo %q: member %d (%s) uses accountStrategy %q and requires accountPool", c.Name, index, member.Model, member.EffectiveAccountStrategy(c))
		}
		if !explicit && member.AccountPool != "" && !member.Pinned() {
			return fmt.Errorf("combo %q: member %d (%s) sets accountPool without an explicit accountStrategy", c.Name, index, member.Model)
		}
	}
	if c.CooldownSeconds != nil && *c.CooldownSeconds < 0 {
		return fmt.Errorf("combo %q: cooldownSeconds must not be negative, got %d", c.Name, *c.CooldownSeconds)
	}
	return nil
}

// normalizeStrategy maps the empty strategy to round-robin and rejects unknown names.
func normalizeStrategy(strategy string) (string, error) {
	switch strings.TrimSpace(strategy) {
	case "", StrategyRoundRobin:
		return StrategyRoundRobin, nil
	case StrategyFailover, StrategyWeighted, StrategyQuotaAware:
		return strings.TrimSpace(strategy), nil
	default:
		return "", fmt.Errorf("unknown strategy %q (want %q or %q)", strategy, StrategyRoundRobin, StrategyFailover)
	}
}

// seedConfig is the plugin-owned block under plugins.configs.tea-combos.
type seedConfig struct {
	StatePath          string
	QuotaSnapshotPath  string
	QuotaMaxAgeSeconds int
	ResetBoost         quota.ResetBoost
	DrainToZero        bool
	Combos             []Combo
	// CooldownSeconds is the plugin-wide cooling window. Nil means the built-in default;
	// zero disables cooling.
	CooldownSeconds *int
	// MaxCooldownSeconds caps how long a Retry-After hint may park a member. Nil means the
	// built-in default.
	MaxCooldownSeconds *int
}

// parseSeedConfig reads the plugin block. CPA hands plugins raw YAML bytes, so this uses a real
// YAML parser rather than a hand-rolled format. The host also adds its own enabled/priority keys
// to the same mapping; unknown keys are ignored here.
func parseSeedConfig(raw []byte) (seedConfig, error) {
	cfg := seedConfig{}
	if strings.TrimSpace(string(raw)) == "" {
		return cfg, nil
	}
	var doc struct {
		StatePath          string           `yaml:"statePath"`
		QuotaSnapshotPath  string           `yaml:"quotaSnapshotPath"`
		QuotaMaxAgeSeconds int              `yaml:"quotaMaxAgeSeconds"`
		ResetBoost         quota.ResetBoost `yaml:"resetBoost"`
		DrainToZero        bool             `yaml:"drainToZero"`
		CooldownSeconds    *int             `yaml:"cooldownSeconds"`
		MaxCooldownSeconds *int             `yaml:"maxCooldownSeconds"`
		Combos             yaml.Node        `yaml:"combos"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return cfg, fmt.Errorf("parse plugins.configs.tea-combos: %w", err)
	}
	cfg.StatePath = strings.TrimSpace(doc.StatePath)
	cfg.QuotaSnapshotPath = strings.TrimSpace(doc.QuotaSnapshotPath)
	cfg.QuotaMaxAgeSeconds = doc.QuotaMaxAgeSeconds
	if cfg.QuotaMaxAgeSeconds < 0 {
		return cfg, fmt.Errorf("quotaMaxAgeSeconds must not be negative")
	}
	if err := doc.ResetBoost.Validate(); err != nil {
		return cfg, err
	}
	cfg.ResetBoost = doc.ResetBoost
	cfg.DrainToZero = doc.DrainToZero
	cfg.CooldownSeconds = doc.CooldownSeconds
	cfg.MaxCooldownSeconds = doc.MaxCooldownSeconds
	if doc.CooldownSeconds != nil && *doc.CooldownSeconds < 0 {
		return cfg, fmt.Errorf("cooldownSeconds must not be negative, got %d", *doc.CooldownSeconds)
	}
	if doc.MaxCooldownSeconds != nil && *doc.MaxCooldownSeconds < 0 {
		return cfg, fmt.Errorf("maxCooldownSeconds must not be negative, got %d", *doc.MaxCooldownSeconds)
	}
	combos, err := decodeCombos(doc.Combos)
	if err != nil {
		return cfg, err
	}
	cfg.Combos = combos
	return cfg, nil
}

func decodeCombos(node yaml.Node) ([]Combo, error) {
	if node.Kind == 0 || node.Tag == "!!null" {
		return nil, nil
	}
	var combos []Combo
	switch node.Kind {
	case yaml.MappingNode:
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := strings.TrimSpace(node.Content[index].Value)
			var combo Combo
			if err := node.Content[index+1].Decode(&combo); err != nil {
				return nil, fmt.Errorf("parse combo %q: %w", key, err)
			}
			if combo.Name == "" {
				combo.Name = key
			} else if strings.TrimSpace(combo.Name) != key {
				return nil, fmt.Errorf("combo key %q does not match name %q", key, combo.Name)
			}
			combos = append(combos, combo)
		}
	case yaml.SequenceNode:
		if err := node.Decode(&combos); err != nil {
			return nil, fmt.Errorf("parse combos list: %w", err)
		}
	default:
		return nil, fmt.Errorf("combos must be a mapping of name to combo or a list of combos")
	}

	seen := make(map[string]struct{}, len(combos))
	out := make([]Combo, 0, len(combos))
	for _, combo := range combos {
		combo = combo.Normalize()
		if err := combo.Validate(); err != nil {
			return nil, err
		}
		if _, exists := seen[combo.Name]; exists {
			return nil, fmt.Errorf("duplicate combo name %q", combo.Name)
		}
		seen[combo.Name] = struct{}{}
		out = append(out, combo)
	}
	return out, nil
}

// rewriteModel changes only the top-level "model" field, leaving every other value as it was.
func rewriteModel(payload []byte, model string) []byte {
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(payload, &doc); err != nil {
		return payload
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return payload
	}
	doc["model"] = encoded
	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return out
}
