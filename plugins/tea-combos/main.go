package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	pluginName = "Tea Combos"
	pluginID   = "tea-combos"

	defaultStateFileName = "tea-combos.state.json"
)

var pluginVersion = "0.2.1"

// defaultCooldownSeconds parks a member after a member-side failure when neither the combo nor
// the plugin config overrides the window.
const defaultCooldownSeconds = 30

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
	store          *Store
	roundRobin     *roundRobinSelector
	affinity       *Affinity
	cooldown       *cooldownTracker
	cooldownWindow time.Duration
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
	window := time.Duration(defaultCooldownSeconds) * time.Second
	if cfg.CooldownSeconds != nil {
		window = time.Duration(*cfg.CooldownSeconds) * time.Second
		if window < 0 {
			window = 0
		}
	}
	return &combosPlugin{
		store:          store,
		roundRobin:     newRoundRobinSelector(),
		affinity:       newAffinity(bindings, store.SaveBindings),
		cooldown:       newCooldownTracker(),
		cooldownWindow: window,
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
				{
					Name:        "cooldownSeconds",
					Type:        pluginapi.ConfigFieldTypeInteger,
					Description: "Seconds a failed member is skipped by selection after a member-side failure (quota, rate limit, credential, capacity). Default 30; 0 disables cooling.",
				},
			},
		},
		Capabilities: pluginapi.Capabilities{
			ModelRegistrar: p,
			ModelRouter:    p,
			Executor:       p,
			ManagementAPI:  p,
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
	order := p.selectOrder(combo, selector, key, preferred)
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
		if !memberFailure(status, message) {
			p.cooldown.Clear(combo.Name, member)
			if err != nil {
				return pluginapi.ExecutorResponse{}, -1, &comboError{Status: status, Message: message}
			}
			return pluginapi.ExecutorResponse{Payload: result.Body, Headers: result.Headers}, index, nil
		}
		// The member itself failed (capacity, quota, credential, transient): park it, tell the
		// selector, and let the next member answer this request.
		p.cooldown.Cool(combo.Name, member, p.effectiveCooldown(combo))
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
		Message:   fmt.Sprintf("combo %s exhausted: %s | attempts=%s | cooling=%v", combo.Name, lastMessage, string(encodedTrace), p.cooldown.Cooling(combo.Name, combo.Members)),
	}
}

// retryableStatus is the set of failures a combo falls through on: 408, 429 and every 5xx.
func retryableStatus(status int) bool {
	if status == http.StatusRequestTimeout || status == http.StatusTooManyRequests {
		return true
	}
	return status >= 500 && status <= 599
}

// memberFailure reports whether a failed member attempt is one the NEXT member could escape
// (capacity, quota, credential, transient) rather than a client-side fault every member would
// repeat. Account exhaustion is the sharp edge: an upstream 401/403 carrying quota or billing
// language is the member's problem, not the client's, even though its status code looks fatal.
func memberFailure(status int, message string) bool {
	if retryableStatus(status) {
		return true
	}
	switch status {
	case http.StatusUnauthorized, http.StatusForbidden:
		return accountExhausted(message)
	case http.StatusBadRequest, http.StatusNotFound:
		// "unknown provider for model X" / "model X does not exist": the MEMBER lost its
		// model (renamed, retired, account lost access). The client never asked for that
		// model — the combo did — so the next member must get the request.
		return modelUnavailable(message)
	}
	return false
}

// modelUnavailable matches upstream "this model does not exist / no provider" wording.
// Narrow on purpose: payload-level 400s (bad parameters, unsupported fields) stay fatal
// because every member would repeat them.
func modelUnavailable(message string) bool {
	lower := strings.ToLower(message)
	needles := []string{
		"model_not_found", "unknown provider for model", "does not exist",
		"no such model", "model not found", "is not a valid model",
	}
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// accountExhausted matches the quota/billing/credential wording upstreams put into 401/403
// bodies: OpenAI insufficient_quota, Anthropic credit balances, Google RESOURCE_EXHAUSTED.
// Matching is deliberately narrow so an unrelated 403 stays fatal and genuine client faults
// still stop the chain on the first member.
func accountExhausted(message string) bool {
	lower := strings.ToLower(message)
	needles := []string{
		"insufficient_quota", "insufficient quota", "credit balance", "credit_balance",
		"billing", "out of credit", "quota exceeded", "quota limit", "resource_exhausted",
		"invalid api key", "invalid x-api-key", "invalid_api_key", "authentication_error",
		"invalid token", "token expired",
	}
	for _, needle := range needles {
		if strings.Contains(lower, needle) {
			return true
		}
	}
	return false
}

// selectOrder filters cooled members out of the selector order. A cooled session-affinity
// binding is skipped so the request lands on a healthy member and the binding rebinds to
// whoever serves. When EVERY member is cooling the filter fails open and the full ring is
// attempted: a fleet-wide blip must not become a guaranteed 502, and the attempt trace plus
// the cooling snapshot show what was parked.
func (p *combosPlugin) selectOrder(combo Combo, selector Selector, key string, preferred int) []int {
	order := selector.Order(combo, key)
	live := make([]int, 0, len(order))
	for _, index := range order {
		if !p.cooldown.Active(combo.Name, combo.Members[index]) {
			live = append(live, index)
		}
	}
	if len(live) > 0 {
		order = live
	}
	if preferred >= 0 && !p.cooldown.Active(combo.Name, combo.Members[preferred]) {
		order = withPreferred(order, preferred)
	}
	return order
}

// effectiveCooldown is the per-combo override, falling back to the plugin-wide window.
func (p *combosPlugin) effectiveCooldown(combo Combo) time.Duration {
	if combo.CooldownSeconds != nil {
		if *combo.CooldownSeconds < 0 {
			return 0
		}
		return time.Duration(*combo.CooldownSeconds) * time.Second
	}
	return p.cooldownWindow
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
