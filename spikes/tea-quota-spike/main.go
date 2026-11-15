package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

const pluginName = "Tea Combo Spike"

var pluginVersion = "0.1.0"

// spikeConfig is the plugin-owned config under plugins.configs.tea-combo-spike.
type spikeConfig struct {
	Combos map[string]spikeCombo `json:"combos"`
}

type spikeCombo struct {
	Provider string   `json:"provider"`
	Members  []string `json:"members"`
}

// comboError is returned from Execute so the ABI layer can emit it through the error envelope
// with a real http_status. ExecutorResponse has no status field, so a failed chain cannot be
// expressed as a response without it being reported to the client as HTTP 200.
type comboError struct {
	Status    int
	Retryable bool
	Message   string
}

func (e *comboError) Error() string { return e.Message }

type comboSpikePlugin struct {
	mu     sync.RWMutex
	config spikeConfig
}

func buildPlugin(configYAML []byte, pluginDir string) (pluginapi.Plugin, error) {
	p := &comboSpikePlugin{}
	if err := p.Reconfigure(configYAML); err != nil {
		return pluginapi.Plugin{}, err
	}
	return pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "The Tea Team",
			GitHubRepository: "https://github.com/the-tea-team/cpa-tea-plugins",
			ConfigFields: []pluginapi.ConfigField{
				{Name: "combos", Type: "json", Description: "map of combo model name to its ordered member models"},
			},
		},
		Capabilities: pluginapi.Capabilities{
			ModelRouter: p,
			Executor:    p,
		},
	}, nil
}

func (p *comboSpikePlugin) Reconfigure(configYAML []byte) error {
	cfg, err := parseSpikeConfig(configYAML)
	if err != nil {
		return err
	}
	p.mu.Lock()
	p.config = cfg
	p.mu.Unlock()
	return nil
}

func (p *comboSpikePlugin) Shutdown() error { return nil }

func (p *comboSpikePlugin) combo(name string) (spikeCombo, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	combo, ok := p.config.Combos[name]
	return combo, ok
}

// RouteModel sends any model that is a configured combo to our own executor.
// Everything else is left to CPA's built-in routing.
func (p *comboSpikePlugin) RouteModel(ctx context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, error) {
	combo, ok := p.combo(req.RequestedModel)
	if !ok || len(combo.Members) == 0 {
		return pluginapi.ModelRouteResponse{Handled: false}, nil
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetExecutor,
		Target:     "tea-combo-spike",
		Reason:     "combo " + req.RequestedModel,
	}, nil
}

func (p *comboSpikePlugin) Identifier() string { return "tea-combo-spike" }

// Execute walks the combo members and returns the first non-retryable response.
// Each member is executed by CPA itself through the host callback, so real
// providers, auth files, schedulers and usage accounting stay core-owned.
func (p *comboSpikePlugin) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	// The router does not pass metadata through, so the combo name is the requested model.
	comboName := strings.TrimSpace(req.Model)
	combo, ok := p.combo(comboName)
	if !ok || len(combo.Members) == 0 {
		return pluginapi.ExecutorResponse{}, fmt.Errorf("combo %q is not configured", comboName)
	}

	attempts := make([]map[string]any, 0, len(combo.Members))
	var lastStatus int
	var lastBody []byte

	for _, member := range combo.Members {
		inventoryRaw, e := hostCall(pluginabi.MethodHostAuthList, map[string]any{})
		if e != nil {
			return pluginapi.ExecutorResponse{}, e
		}
		var inventory struct {
			Files []pluginapi.HostAuthFileEntry `json:"files"`
		}
		if e = json.Unmarshal(inventoryRaw, &inventory); e != nil {
			return pluginapi.ExecutorResponse{}, e
		}
		modelRaw, e := hostCall(pluginabi.MethodHostAuthModels, map[string]any{})
		if e != nil {
			return pluginapi.ExecutorResponse{}, e
		}
		var models pluginapi.HostAuthModelsResponse
		if e = json.Unmarshal(modelRaw, &models); e != nil {
			return pluginapi.ExecutorResponse{}, e
		}
		listCount := len(inventory.Files)
		sort.Slice(inventory.Files, func(i, j int) bool { return inventory.Files[i].Priority > inventory.Files[j].Priority })
		if len(inventory.Files) < 2 {
			for _, m := range models.Credentials {
				inventory.Files = append(inventory.Files, pluginapi.HostAuthFileEntry{ID: m.ID})
			}
		}
		if len(inventory.Files) < 2 {
			return pluginapi.ExecutorResponse{}, fmt.Errorf("insufficient inventory")
		}
		pin := ""
		if comboName == "low" {
			pin = inventory.Files[len(inventory.Files)-1].ID
		}
		if comboName == "high" {
			pin = inventory.Files[0].ID
		}
		payload := rewriteModel(req.Payload, member)
		result, err := hostModelExecute(pluginapi.HostModelExecutionRequest{
			EntryProtocol:  req.SourceFormat,
			ExitProtocol:   req.Format,
			Model:          member,
			Stream:         false,
			Body:           payload,
			Headers:        req.Headers,
			Query:          req.Query,
			ForcedProvider: models.Credentials[0].Provider,
			AuthID:         pin,
			CallerScope:    fmt.Sprint(req.Metadata[pluginapi.CallerScopeMetadataKey]),
		})
		status := 0
		var body []byte
		var headers http.Header
		var hostErr string
		fallThrough := false
		if err != nil {
			hostErr = err.Error()
			// CPA reports upstream failures as errors, carrying the retryable flag and status.
			var hErr *hostError
			if errors.As(err, &hErr) {
				status = hErr.Status
				fallThrough = hErr.Retryable || retryableStatus(hErr.Status)
			} else {
				fallThrough = true
			}
		} else {
			status = result.StatusCode
			body = result.Body
			headers = result.Headers
			fallThrough = retryableStatus(status)
		}
		attempts = append(attempts, map[string]any{
			"member": member, "status": status, "error": hostErr, "fell_through": fallThrough,
		})
		if !fallThrough {
			if err != nil {
				// Non-retryable upstream failure: stop the chain and surface it with its status.
				if status == 0 {
					status = http.StatusBadGateway
				}
				return pluginapi.ExecutorResponse{}, &comboError{Status: status, Message: hostErr}
			}
			if headers == nil {
				headers = http.Header{}
			}
			headers.Set("X-Spike-Inventory", fmt.Sprint(len(inventory.Files)))
			headers.Set("X-Spike-Models", fmt.Sprint(len(models.Credentials)))
			headers.Set("X-Spike-Priorities", fmt.Sprintf("%d,%d", inventory.Files[0].Priority, inventory.Files[len(inventory.Files)-1].Priority))
			_, scoped := req.Metadata[pluginapi.CallerScopeMetadataKey]
			headers.Set("X-Spike-Scope", fmt.Sprint(scoped))
			var doc map[string]any
			_ = json.Unmarshal(body, &doc)
			doc["spike"] = map[string]any{"list_count": listCount, "model_count": len(models.Credentials), "scope_present": scoped}
			body, _ = json.Marshal(doc)
			return pluginapi.ExecutorResponse{Payload: body, Headers: headers}, nil
		}
		if err == nil {
			lastStatus, lastBody = status, body
		} else {
			if status == 0 {
				status = http.StatusBadGateway
			}
			lastStatus, lastBody = status, []byte(hostErr)
		}
	}

	// Every member failed retryably: report the last upstream failure with its real status.
	trace, _ := json.Marshal(attempts)
	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	return pluginapi.ExecutorResponse{}, &comboError{
		Status:    lastStatus,
		Retryable: true,
		Message:   fmt.Sprintf("combo %s exhausted: %s | attempts=%s", comboName, string(lastBody), string(trace)),
	}
}

func (p *comboSpikePlugin) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return pluginapi.ExecutorStreamResponse{}, errors.New("spike implements non-stream only")
}

func (p *comboSpikePlugin) CountTokens(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, errors.New("not implemented in spike")
}

func (p *comboSpikePlugin) HttpRequest(ctx context.Context, req pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, errors.New("not implemented in spike")
}

// retryableStatus is the set of failures a combo should fall through on.
func retryableStatus(status int) bool {
	switch status {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func rewriteModel(payload []byte, model string) []byte {
	var doc map[string]any
	if err := json.Unmarshal(payload, &doc); err != nil {
		return payload
	}
	doc["model"] = model
	out, err := json.Marshal(doc)
	if err != nil {
		return payload
	}
	return out
}

// parseSpikeConfig reads the plugin's own config block. CPA hands the plugin raw YAML bytes for
// plugins.configs.<id>, so decode it with a real YAML parser rather than a hand-rolled format.
func parseSpikeConfig(raw []byte) (spikeConfig, error) {
	cfg := spikeConfig{Combos: map[string]spikeCombo{}}
	if strings.TrimSpace(string(raw)) == "" {
		return cfg, nil
	}
	var doc struct {
		Combos map[string]struct {
			Provider string   `yaml:"provider"`
			Members  []string `yaml:"members"`
		} `yaml:"combos"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return cfg, fmt.Errorf("parse plugin config: %w", err)
	}
	for name, entry := range doc.Combos {
		cfg.Combos[name] = spikeCombo{Provider: entry.Provider, Members: entry.Members}
	}
	return cfg, nil
}

func sortedCombos(cfg spikeConfig) []string {
	names := make([]string, 0, len(cfg.Combos))
	for name := range cfg.Combos {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

var _ = pluginabi.SchemaVersion
