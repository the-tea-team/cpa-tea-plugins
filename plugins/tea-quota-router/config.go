package main

import (
	"fmt"
	"github.com/the-tea-team/cpa-tea-plugins/internal/tea/quota"
	"gopkg.in/yaml.v3"
	"math"
	"net"
	"net/url"
	"regexp"
	"strings"
)

type AccountOverride struct {
	AccountID        string             `json:"accountId" yaml:"accountId"`
	Label            string             `json:"label" yaml:"label"`
	BillingType      string             `json:"billingType" yaml:"billingType"`
	Plan             string             `json:"plan" yaml:"plan"`
	CapacityByWindow map[string]float64 `json:"capacityByWindow" yaml:"capacityByWindow"`
}
type Price struct {
	Input     *float64 `json:"input" yaml:"input"`
	Output    *float64 `json:"output" yaml:"output"`
	CacheRead *float64 `json:"cacheRead" yaml:"cacheRead"`
}
type AdminSource struct {
	Provider string `json:"provider" yaml:"provider"`
	KeyEnv   string `json:"keyEnv" yaml:"keyEnv"`
}
type Config struct {
	DataDir       string `json:"dataDir" yaml:"dataDir"`
	SchedulerMode string `json:"schedulerMode" yaml:"schedulerMode"`
	// QuotaAcrossPriorities lets quota-aware strategies score candidates from
	// every priority tier. Default false keeps max-tier-only behavior.
	QuotaAcrossPriorities bool              `json:"quotaAcrossPriorities" yaml:"quotaAcrossPriorities"`
	ProviderStrategies    map[string]string `json:"providerStrategies" yaml:"providerStrategies"`
	ManagementURL         string            `json:"managementURL" yaml:"managementURL"`
	ManagementKeyEnv      string            `json:"managementKeyEnv" yaml:"managementKeyEnv"`
	PollIntervalSeconds   int               `json:"pollIntervalSeconds" yaml:"pollIntervalSeconds"`
	MaxAgeSeconds         int               `json:"maxAgeSeconds" yaml:"maxAgeSeconds"`
	// ResetBoost multiplies the score of accounts whose weekly window resets
	// within horizonHours, so quota that would expire gets spent first.
	ResetBoost quota.ResetBoost `json:"resetBoost" yaml:"resetBoost"`
	// DrainToZero disables the headroom guard (see quota.ScoreOptions).
	DrainToZero         bool                       `json:"drainToZero" yaml:"drainToZero"`
	Pools               []quota.Pool               `json:"pools" yaml:"pools"`
	AccountOverrides    map[string]AccountOverride `json:"accountOverrides" yaml:"accountOverrides"`
	FixtureSnapshotPath string                     `json:"fixtureSnapshotPath" yaml:"fixtureSnapshotPath"`
	Prices              map[string]Price           `json:"prices" yaml:"prices"`
	AdminSources        []AdminSource              `json:"adminSources" yaml:"adminSources"`
}

func parseConfig(raw []byte) (Config, error) {
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("invalid configuration")
	}
	if strings.TrimSpace(c.DataDir) == "" {
		return c, fmt.Errorf("dataDir required")
	}
	if c.SchedulerMode == "" {
		c.SchedulerMode = "shadow"
	}
	if c.SchedulerMode != "shadow" && c.SchedulerMode != "active" && c.SchedulerMode != "off" {
		return c, fmt.Errorf("invalid schedulerMode")
	}
	if c.PollIntervalSeconds == 0 {
		c.PollIntervalSeconds = 180
	}
	if c.MaxAgeSeconds == 0 {
		c.MaxAgeSeconds = 600
	}
	if c.PollIntervalSeconds < 1 || c.MaxAgeSeconds < 1 {
		return c, fmt.Errorf("invalid intervals")
	}
	if err := c.ResetBoost.Validate(); err != nil {
		return c, err
	}
	for _, v := range c.ProviderStrategies {
		if v != "inherit" && v != "quota-aware-round-robin" {
			return c, fmt.Errorf("invalid provider strategy")
		}
	}
	for _, price := range c.Prices {
		for _, rate := range []*float64{price.Input, price.Output, price.CacheRead} {
			if rate != nil && (*rate < 0 || math.IsNaN(*rate) || math.IsInf(*rate, 0)) {
				return c, fmt.Errorf("invalid price rate")
			}
		}
	}
	if c.ManagementURL != "" {
		u, e := url.Parse(c.ManagementURL)
		if e != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || net.ParseIP(u.Hostname()) == nil || !net.ParseIP(u.Hostname()).IsLoopback() || u.Port() == "" {
			return c, fmt.Errorf("managementURL must be literal loopback HTTP origin")
		}
		if !regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`).MatchString(c.ManagementKeyEnv) {
			return c, fmt.Errorf("managementKeyEnv required")
		}
	}
	return c, nil
}
