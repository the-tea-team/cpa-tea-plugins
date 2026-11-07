package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// StrategyRoundRobin rotates members per request.
	StrategyRoundRobin = "round-robin"
	// StrategyFailover tries members in their configured order.
	StrategyFailover = "failover"
)

// Member is one upstream model a combo can execute.
type Member struct {
	Model    string `json:"model" yaml:"model"`
	Provider string `json:"provider,omitempty" yaml:"provider,omitempty"`
}

// Combo is a named set of members with a selection strategy.
type Combo struct {
	Name            string   `json:"name" yaml:"name"`
	Strategy        string   `json:"strategy,omitempty" yaml:"strategy,omitempty"`
	Members         []Member `json:"members" yaml:"members"`
	SessionAffinity *bool    `json:"sessionAffinity,omitempty" yaml:"sessionAffinity,omitempty"`
	TTL             string   `json:"ttl,omitempty" yaml:"ttl,omitempty"`
	Enabled         *bool    `json:"enabled,omitempty" yaml:"enabled"`
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
	if c.Enabled == nil {
		enabled := true
		c.Enabled = &enabled
	}
	members := make([]Member, 0, len(c.Members))
	for _, member := range c.Members {
		members = append(members, Member{
			Model:    strings.TrimSpace(member.Model),
			Provider: strings.TrimSpace(member.Provider),
		})
	}
	c.Members = members
	return c
}

// Validate rejects combos that could never be routed unambiguously.
func (c Combo) Validate() error {
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
		if member.Model == "" {
			return fmt.Errorf("combo %q: member %d has an empty model", c.Name, index)
		}
	}
	return nil
}

// normalizeStrategy maps the empty strategy to round-robin and rejects unknown names.
func normalizeStrategy(strategy string) (string, error) {
	switch strings.TrimSpace(strategy) {
	case "", StrategyRoundRobin:
		return StrategyRoundRobin, nil
	case StrategyFailover:
		return StrategyFailover, nil
	default:
		return "", fmt.Errorf("unknown strategy %q (want %q or %q)", strategy, StrategyRoundRobin, StrategyFailover)
	}
}

// seedConfig is the plugin-owned block under plugins.configs.tea-combos.
type seedConfig struct {
	StatePath string
	Combos    []Combo
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
		StatePath string    `yaml:"statePath"`
		Combos    yaml.Node `yaml:"combos"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return cfg, fmt.Errorf("parse plugins.configs.tea-combos: %w", err)
	}
	cfg.StatePath = strings.TrimSpace(doc.StatePath)
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
