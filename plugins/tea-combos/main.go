package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginName = "Tea Combos"
	pluginID   = "tea-combos"

	defaultStateFileName = "tea-combos.state.json"
)

var pluginVersion = "0.1.0"

// comboError carries a real HTTP status through the ABI error envelope. ExecutorResponse has no
// status field, so a failed chain expressed as a response would reach the client as HTTP 200.
type comboError struct {
	Status    int
	Retryable bool
	Message   string
}

func (e *comboError) Error() string { return e.Message }

// combosPlugin implements ModelRouter and Executor for named model combos.
type combosPlugin struct {
	store      *Store
	roundRobin *roundRobinSelector
	affinity   *Affinity
}

// newCombosPlugin loads the state file, merges seed combos into it, and returns a ready plugin.
func newCombosPlugin(configYAML []byte, pluginDir string) (*combosPlugin, error) {
	cfg, err := parseSeedConfig(configYAML)
	if err != nil {
		return nil, err
	}
	statePath, err := resolveStatePath(cfg.StatePath, pluginDir)
	if err != nil {
		return nil, err
	}
	store, err := OpenStore(statePath)
	if err != nil {
		return nil, err
	}
	if err := store.MergeSeed(cfg.Combos); err != nil {
		return nil, fmt.Errorf("persist seed combos to %s: %w", statePath, err)
	}
	bindings := store.LoadBindings()
	return &combosPlugin{
		store:      store,
		roundRobin: newRoundRobinSelector(),
		affinity:   newAffinity(bindings, store.SaveBindings),
	}, nil
}

// shutdown flushes session bindings to the state file and stops the write-behind goroutine.
func (p *combosPlugin) shutdown() {
	if p == nil || p.affinity == nil {
		return
	}
	p.affinity.Close()
}

// resolveStatePath prefers the configured statePath, then the directory that holds the plugin
// library. It fails loudly when neither is usable so operators get told which key to set.
func resolveStatePath(configured, pluginDir string) (string, error) {
	if path := strings.TrimSpace(configured); path != "" {
		return path, nil
	}
	if dir := strings.TrimSpace(pluginDir); dir != "" && !isEphemeralPluginDir(dir) {
		return filepath.Join(dir, defaultStateFileName), nil
	}
	return "", fmt.Errorf("cannot determine a durable combo state file path: set plugins.configs.%s.statePath", pluginID)
}

// isEphemeralPluginDir reports whether dir is one of CPA's per-process shadow-copy directories.
// CPA copies every plugin .so into $TMPDIR/cliproxy-pluginhost/pid-<pid>/ before loading it, so
// that directory is neither the installed plugin directory nor stable across restarts. Writing
// combo state there would silently lose every runtime edit, so we refuse to.
func isEphemeralPluginDir(dir string) bool {
	clean := filepath.Clean(dir)
	if filepath.Base(filepath.Dir(clean)) == "cliproxy-pluginhost" {
		return true
	}
	if strings.HasPrefix(filepath.Base(clean), "pid-") && filepath.Base(filepath.Dir(clean)) == "cliproxy-pluginhost" {
		return true
	}
	return false
}

func (p *combosPlugin) plugin() pluginapi.Plugin {
	return pluginapi.Plugin{
		Metadata: pluginapi.Metadata{
			Name:             pluginName,
			Version:          pluginVersion,
			Author:           "The Tea Team",
			GitHubRepository: "https://github.com/the-tea-team/cpa-tea-plugins",
			Logo:             pluginLogo,
			ConfigFields: []pluginapi.ConfigField{
				{
					Name:        "statePath",
					Type:        pluginapi.ConfigFieldTypeString,
					Description: "Path of the plugin-owned combo state file. Defaults to " + defaultStateFileName + " beside the plugin library.",
				},
				{
					Name:        "combos",
					Type:        pluginapi.ConfigFieldTypeObject,
					Description: "Seed combos: a mapping of combo name to combo, or a list of combos. Seeds only add names absent from the state file.",
				},
			},
		},
		Capabilities: pluginapi.Capabilities{
			ModelRouter:   p,
			Executor:      p,
			ManagementAPI: p,
		},
	}
}

func buildPlugin(configYAML []byte, pluginDir string) (pluginapi.Plugin, error) {
	plugin, err := newCombosPlugin(configYAML, pluginDir)
	if err != nil {
		return pluginapi.Plugin{}, err
	}
	return plugin.plugin(), nil
}

// Identifier is the executor id the router targets.
func (p *combosPlugin) Identifier() string { return pluginID }

// RouteModel sends configured, enabled combos with at least one member to our executor and
// leaves every other model to CPA's built-in routing.
func (p *combosPlugin) RouteModel(_ context.Context, req pluginapi.ModelRouteRequest) (pluginapi.ModelRouteResponse, error) {
	name := strings.TrimSpace(req.RequestedModel)
	combo, ok := p.store.Get(name)
	if !ok || !combo.IsEnabled() || len(combo.Members) == 0 {
		return pluginapi.ModelRouteResponse{Handled: false}, nil
	}
	return pluginapi.ModelRouteResponse{
		Handled:    true,
		TargetKind: pluginapi.ModelRouteTargetExecutor,
		Target:     pluginID,
		Reason:     "combo " + name,
	}, nil
}

// Execute recovers the combo name from the requested model (route metadata does not survive),
// asks the selector for an attempt order, and walks it through the host with session affinity.
func (p *combosPlugin) Execute(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return p.executeCombo(ctx, req, strings.TrimSpace(req.Model))
}

// executeCombo is the executor seam: session affinity resolves a bound member here, keeps it
// first in the attempt order, and records the member that finally succeeds.
func (p *combosPlugin) executeCombo(ctx context.Context, req pluginapi.ExecutorRequest, comboName string) (pluginapi.ExecutorResponse, error) {
	combo, ok := p.store.Get(comboName)
	if !ok {
		return pluginapi.ExecutorResponse{}, &comboError{Status: http.StatusNotFound, Message: fmt.Sprintf("combo %q is not configured", comboName)}
	}
	if !combo.IsEnabled() {
		return pluginapi.ExecutorResponse{}, &comboError{Status: http.StatusNotFound, Message: fmt.Sprintf("combo %q is disabled", comboName)}
	}
	if len(combo.Members) == 0 {
		return pluginapi.ExecutorResponse{}, &comboError{Status: http.StatusNotFound, Message: fmt.Sprintf("combo %q has no members", comboName)}
	}
	selector, err := selectorForStrategy(p.roundRobin, combo.Strategy)
	if err != nil {
		return pluginapi.ExecutorResponse{}, &comboError{Status: http.StatusInternalServerError, Message: err.Error()}
	}

	key, preferred, bound := p.affinity.Resolve(combo, req)
	if !bound {
		preferred = -1
	}
	order := withPreferred(selector.Order(combo, key), preferred)
	response, used, err := p.runAttempts(ctx, req, combo, order)
	if err != nil {
		return pluginapi.ExecutorResponse{}, err
	}
	// A binding is recorded only on success. A non-retryable client error stopped the chain
	// above and left the binding untouched, as does an exhausted chain.
	p.affinity.Bind(key, used, combo)
	return response, nil
}

type attemptTrace struct {
	Member    string `json:"member"`
	Status    int    `json:"status"`
	Error     string `json:"error,omitempty"`
	Retryable bool   `json:"retryable"`
}

func (p *combosPlugin) runAttempts(ctx context.Context, req pluginapi.ExecutorRequest, combo Combo, order []int) (pluginapi.ExecutorResponse, int, error) {
	trace := make([]attemptTrace, 0, len(order))
	lastStatus := 0
	lastMessage := ""

	for _, index := range order {
		member := combo.Members[index]
		payload := rewriteModel(req.Payload, member.Model)
		result, err := hostModelExecute(ctx, pluginapi.HostModelExecutionRequest{
			EntryProtocol:  req.SourceFormat,
			ExitProtocol:   req.Format,
			Model:          member.Model,
			Stream:         false,
			Body:           payload,
			Headers:        req.Headers,
			Query:          req.Query,
			ForcedProvider: member.Provider,
		})

		status := 0
		retryable := false
		message := ""
		if err != nil {
			var hostErr *hostError
			if errors.As(err, &hostErr) {
				status = hostErr.Status
				if status == 0 {
					status = http.StatusBadGateway
				}
				retryable = hostErr.Retryable || retryableStatus(status)
			} else {
				status = http.StatusBadGateway
				retryable = true
			}
			message = err.Error()
		} else {
			status = result.StatusCode
			if status == 0 {
				status = http.StatusOK
			}
			retryable = retryableStatus(status)
		}

		trace = append(trace, attemptTrace{Member: member.Model, Status: status, Error: message, Retryable: retryable})
		if !retryable {
			if err != nil {
				return pluginapi.ExecutorResponse{}, -1, &comboError{Status: status, Message: message}
			}
			return pluginapi.ExecutorResponse{Payload: result.Body, Headers: result.Headers}, index, nil
		}
		lastStatus = status
		lastMessage = message
	}

	if lastStatus == 0 {
		lastStatus = http.StatusBadGateway
	}
	if lastMessage == "" {
		lastMessage = "every member failed retryably"
	}
	encodedTrace, _ := json.Marshal(trace)
	return pluginapi.ExecutorResponse{}, -1, &comboError{
		Status:    lastStatus,
		Retryable: true,
		Message:   fmt.Sprintf("combo %s exhausted: %s | attempts=%s", combo.Name, lastMessage, string(encodedTrace)),
	}
}

// retryableStatus is the set of failures a combo falls through on: 408, 429 and every 5xx.
func retryableStatus(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599
}

// ExecuteStream falls back across members only before the first payload reaches the client. See
// stream.go; the ABI supplies the host stream id through the context.
func (p *combosPlugin) ExecuteStream(ctx context.Context, req pluginapi.ExecutorRequest) (pluginapi.ExecutorStreamResponse, error) {
	return p.executeStream(ctx, req)
}

func (p *combosPlugin) CountTokens(_ context.Context, _ pluginapi.ExecutorRequest) (pluginapi.ExecutorResponse, error) {
	return pluginapi.ExecutorResponse{}, errors.New("tea-combos: token counting is not supported")
}

func (p *combosPlugin) HttpRequest(_ context.Context, _ pluginapi.ExecutorHTTPRequest) (pluginapi.ExecutorHTTPResponse, error) {
	return pluginapi.ExecutorHTTPResponse{}, errors.New("tea-combos: HTTP bridging is not supported")
}
