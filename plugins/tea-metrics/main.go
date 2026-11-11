package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const pluginName = "tea-metrics"

var pluginVersion = "0.1.0"

const defaultRetentionDays = 30

type metricsConfig struct {
	DataDir       string `yaml:"dataDir"`
	RetentionDays int    `yaml:"retentionDays"`
}

type tea_metricsPlugin struct {
	configYAML []byte
	pluginDir  string
	store      *Store
}

var (
	// The host may call plugin.register and plugin.reconfigure repeatedly, and the ABI has
	// no per-instance state, so the store is a process singleton and the latest instance
	// is what capability methods dispatch against.
	current   sync.Map // any single slot via pointer swap below
	storeOnce sync.Once
	sharedSt  *Store
	storeErr  error
)

func setCurrent(p *tea_metricsPlugin) { current.Store("p", p) }

func getCurrent() *tea_metricsPlugin {
	v, _ := current.Load("p")
	p, _ := v.(*tea_metricsPlugin)
	return p
}

func buildPlugin(configYAML []byte, pluginDir string) (pluginapi.Plugin, error) {
	var cfg metricsConfig
	if len(configYAML) > 0 {
		if err := yaml.Unmarshal(configYAML, &cfg); err != nil {
			return pluginapi.Plugin{}, fmt.Errorf("tea-metrics: parse config: %w", err)
		}
	}
	if cfg.RetentionDays <= 0 {
		cfg.RetentionDays = defaultRetentionDays
	}
	storeOnce.Do(func() {
		dir, err := resolveDataDir(cfg.DataDir, pluginDir)
		if err != nil {
			storeErr = err
			return
		}
		if err := os.MkdirAll(dir, 0o750); err != nil {
			storeErr = fmt.Errorf("tea-metrics: create data dir: %w", err)
			return
		}
		sharedSt, storeErr = openStore(filepath.Join(dir, "tea-metrics.db"), time.Duration(cfg.RetentionDays)*24*time.Hour)
		if storeErr == nil {
			sharedSt.startWriter()
			go sharedSt.pruneLoop()
			log.Printf("tea-metrics: store ready at %s (retention %dd)", filepath.Join(dir, "tea-metrics.db"), cfg.RetentionDays)
		}
	})
	if storeErr != nil {
		return pluginapi.Plugin{}, storeErr
	}
	if cfg.DataDir != "" && sharedSt.dataDir != cfg.DataDir {
		log.Printf("tea-metrics: dataDir changed to %q; takes effect on restart", cfg.DataDir)
	}
	p := &tea_metricsPlugin{
		configYAML: append([]byte(nil), configYAML...),
		pluginDir:  pluginDir,
		store:      sharedSt,
	}
	setCurrent(p)

	return pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "The Tea Team",
			GitHubRepository: "https://github.com/the-tea-team/cpa-tea-plugins",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "dataDir", Type: "string", Description: "Directory for tea-metrics.db (required; the host passes no plugin dir)"},
				{Name: "retentionDays", Type: "number", Description: "Days of per-request samples to keep (default 30)"},
			},
		},
		Capabilities: pluginapi.Capabilities{
			UsagePlugin:   p,
			ManagementAPI: p,
		},
	}, nil
}

// resolveDataDir prefers an explicit dataDir; otherwise uses the plugin dir unless it looks
// ephemeral (the host passes "" on 8.0.62, so in practice dataDir must be configured).
func resolveDataDir(configured, pluginDir string) (string, error) {
	if d := strings.TrimSpace(configured); d != "" {
		return d, nil
	}
	if pluginDir == "" || isEphemeralPluginDir(pluginDir) {
		return "", fmt.Errorf("tea-metrics: no usable data dir (plugin dir %q is ephemeral; set dataDir in plugins.configs.tea-metrics)", pluginDir)
	}
	return pluginDir, nil
}

func isEphemeralPluginDir(dir string) bool {
	d := filepath.Clean(dir)
	return strings.HasPrefix(d, "/tmp") || strings.Contains(d, ".tmp-")
}

// HandleUsage implements pluginapi.UsagePlugin. It never blocks the request path:
// records go to a bounded channel and are dropped (counted) if the writer falls behind.
func (p *tea_metricsPlugin) HandleUsage(_ context.Context, rec pluginapi.UsageRecord) {
	p.store.submit(rec)
}

func (p *tea_metricsPlugin) Reconfigure(configYAML []byte) error {
	_, err := buildPlugin(configYAML, p.pluginDir)
	return err
}

func (p *tea_metricsPlugin) Shutdown() error {
	if sharedSt != nil {
		return sharedSt.close()
	}
	return nil
}
