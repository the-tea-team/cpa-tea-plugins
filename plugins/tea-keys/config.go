package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
	"gopkg.in/yaml.v3"
)

// pluginConfig is the YAML subtree under plugins.configs.tea-keys.
//
// keys entries are hashed in memory at load time; the plaintext key is never retained.
type pluginConfig struct {
	// RequireManagedKeys makes tea-keys the exclusive frontend auth provider and rejects
	// every key that is not listed here. It defaults to true: model-list filtering can
	// only be guaranteed when tea-keys sees every credential, including query strings.
	// Set it to false only while migrating keys that CPA still authenticates.
	RequireManagedKeys *bool `yaml:"require_managed_keys"`

	// Keys lists the client API keys tea-keys knows about.
	Keys []keyConfig `yaml:"keys"`
}

type keyConfig struct {
	// Name is the stable identity returned to CPA as the authenticated principal.
	// It appears in usage records and logs, so it must not be the key itself.
	Name string `yaml:"name"`

	// Key is the plaintext client API key. Use exactly one of Key and KeySHA256.
	Key string `yaml:"key"`

	// KeySHA256 is the lowercase or uppercase hex SHA-256 of the client API key.
	// A "sha256:" prefix is accepted. Use exactly one of Key and KeySHA256.
	KeySHA256 string `yaml:"key_sha256"`

	// Models is the client-facing model allowlist for this key.
	//
	//   omitted       -> no restriction (all models)
	//   []            -> no models (the key cannot use any model)
	//   ["*"]         -> no restriction
	//   ["gpt-4o"]    -> exact model
	//   ["claude-*"]  -> glob (* = any sequence, ? = one character)
	//
	// Matching also understands the claude-fable-5-dd-<reversed> model-list cloak,
	// an optional thinking suffix such as "(8192)", and the "models/" prefix used by
	// Gemini model lists.
	Models *[]string `yaml:"models"`
}

// compiledConfig is the immutable runtime form of pluginConfig.
type compiledConfig struct {
	requireManagedKeys bool
	keys               map[[sha256.Size]byte]*apiKey
	// scopes maps CPA's opaque caller-scope handle to a key. It lets the request
	// interceptor identify a key that CPA authenticated from a query-string
	// credential, where the request interceptor has no headers to read.
	scopes map[string]*apiKey
}

type apiKey struct {
	name     string
	hash     [sha256.Size]byte
	models   []string
	allowAll bool
}

func newCompiledConfig(raw []byte) (*compiledConfig, error) {
	cfg := pluginConfig{}
	if len(strings.TrimSpace(string(raw))) > 0 {
		if err := yaml.Unmarshal(raw, &cfg); err != nil {
			return nil, fmt.Errorf("parse tea-keys config: %w", err)
		}
	}

	out := &compiledConfig{
		requireManagedKeys: true,
		keys:               make(map[[sha256.Size]byte]*apiKey, len(cfg.Keys)),
		scopes:             make(map[string]*apiKey, len(cfg.Keys)*2),
	}
	if cfg.RequireManagedKeys != nil {
		out.requireManagedKeys = *cfg.RequireManagedKeys
	}
	if out.requireManagedKeys && len(cfg.Keys) == 0 {
		return nil, errors.New("require_managed_keys is true but no keys are configured")
	}

	names := make(map[string]struct{}, len(cfg.Keys))
	for i, entry := range cfg.Keys {
		key, err := compileKey(entry, i)
		if err != nil {
			return nil, err
		}
		if _, exists := out.keys[key.hash]; exists {
			return nil, fmt.Errorf("keys[%d]: duplicate key fingerprint", i)
		}
		if _, exists := names[key.name]; exists {
			return nil, fmt.Errorf("keys[%d]: duplicate name %q", i, key.name)
		}
		names[key.name] = struct{}{}
		out.keys[key.hash] = key
		if err := out.registerScope(key.name, key, i); err != nil {
			return nil, err
		}
		if plain := strings.TrimSpace(entry.Key); plain != "" {
			if err := out.registerScope(plain, key, i); err != nil {
				return nil, err
			}
		}
	}
	return out, nil
}

func (c *compiledConfig) registerScope(value string, key *apiKey, index int) error {
	scope := callerScope(value)
	if scope == "" {
		return nil
	}
	if existing, exists := c.scopes[scope]; exists && existing != key {
		return fmt.Errorf("keys[%d]: caller identity collides with key %q", index, existing.name)
	}
	c.scopes[scope] = key
	return nil
}

// callerScope mirrors CPA's sdk/cliproxy/session.CallerScope. Importing that SDK
// package would pull the core config and utility trees into this small plugin, so the
// one-line derivation is kept in step here instead.
func callerScope(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("cli-proxy-api:caller-scope:v1\x00" + value))
	return hex.EncodeToString(sum[:])
}

func compileKey(entry keyConfig, index int) (*apiKey, error) {
	plain := strings.TrimSpace(entry.Key)
	encoded := strings.TrimSpace(entry.KeySHA256)
	if (plain == "") == (encoded == "") {
		return nil, fmt.Errorf("keys[%d]: set exactly one of key and key_sha256", index)
	}

	var hash [sha256.Size]byte
	if plain != "" {
		hash = sha256.Sum256([]byte(plain))
	} else {
		parsed, err := parseSHA256(encoded)
		if err != nil {
			return nil, fmt.Errorf("keys[%d]: %w", index, err)
		}
		hash = parsed
	}

	name := strings.TrimSpace(entry.Name)
	if name == "" {
		name = "key-" + hex.EncodeToString(hash[:4])
	}

	key := &apiKey{name: name, hash: hash}
	if entry.Models != nil {
		patterns := make([]string, 0, len(*entry.Models))
		for j, pattern := range *entry.Models {
			pattern = strings.TrimSpace(pattern)
			if pattern == "" {
				return nil, fmt.Errorf("keys[%d].models[%d]: pattern is empty", index, j)
			}
			if pattern == "*" {
				if len(*entry.Models) != 1 {
					return nil, fmt.Errorf("keys[%d].models: \"*\" must be the only pattern", index)
				}
				key.allowAll = true
				patterns = nil
				break
			}
			patterns = append(patterns, pattern)
		}
		if !key.allowAll {
			key.models = patterns
		}
	} else {
		key.allowAll = true
	}
	return key, nil
}

func parseSHA256(value string) ([sha256.Size]byte, error) {
	var out [sha256.Size]byte
	value = strings.TrimSpace(value)
	if len(value) >= len("sha256:") && strings.EqualFold(value[:len("sha256:")], "sha256:") {
		value = value[len("sha256:"):]
	}
	raw, err := hex.DecodeString(value)
	if err != nil {
		return out, fmt.Errorf("key_sha256 is not valid hex: %w", err)
	}
	if len(raw) != sha256.Size {
		return out, fmt.Errorf("key_sha256 must be %d hex characters, got %d", sha256.Size*2, len(value))
	}
	copy(out[:], raw)
	return out, nil
}

// lookupString returns the key matching an already-trimmed credential value.
func (c *compiledConfig) lookupString(value string) *apiKey {
	value = strings.TrimSpace(value)
	if c == nil || value == "" {
		return nil
	}
	return c.keys[sha256.Sum256([]byte(value))]
}

// keyForHeaders returns the managed key presented by the request, if any.
//
// It accepts the same header forms CPA's own config provider accepts. Query-string
// credentials are deliberately not accepted here: the model-list response interceptor
// only receives headers, so a query key could bypass list filtering. Authenticate()
// rejects managed query keys for that reason.
func (c *compiledConfig) keyForHeaders(headers http.Header) *apiKey {
	if c == nil {
		return nil
	}
	for _, candidate := range headerCredentialCandidates(headers) {
		if key := c.lookupString(candidate); key != nil {
			return key
		}
	}
	return nil
}

// keyForIntercept identifies the managed key for an execution request. It prefers
// the request headers and falls back to CPA's opaque caller scope, which is present
// even when CPA authenticated a query-string credential.
func (c *compiledConfig) keyForIntercept(req pluginapi.RequestInterceptRequest) *apiKey {
	if c == nil {
		return nil
	}
	if key := c.keyForHeaders(req.Headers); key != nil {
		return key
	}
	scope := strings.TrimSpace(req.CallerScope)
	if scope == "" && req.Metadata != nil {
		if value, ok := req.Metadata[pluginapi.CallerScopeMetadataKey].(string); ok {
			scope = strings.TrimSpace(value)
		}
	}
	if scope == "" {
		return nil
	}
	return c.scopes[scope]
}

func (k *apiKey) allows(model string) bool {
	if k == nil || k.allowAll {
		return true
	}
	if len(k.models) == 0 {
		return false
	}
	for _, candidate := range modelCandidates(model) {
		for _, pattern := range k.models {
			if globMatch(pattern, candidate) {
				return true
			}
		}
	}
	return false
}

func headerCredentialCandidates(headers http.Header) []string {
	if headers == nil {
		return nil
	}
	var out []string
	add := func(value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		for _, existing := range out {
			if existing == value {
				return
			}
		}
		out = append(out, value)
	}
	add(extractBearerToken(headers.Get("Authorization")))
	add(headers.Get("X-Goog-Api-Key"))
	add(headers.Get("X-Api-Key"))
	return out
}

func queryCredentialCandidates(query map[string][]string) []string {
	if query == nil {
		return nil
	}
	var out []string
	for _, name := range []string{"key", "auth_token"} {
		for _, value := range query[name] {
			value = strings.TrimSpace(value)
			if value == "" {
				continue
			}
			duplicate := false
			for _, existing := range out {
				if existing == value {
					duplicate = true
					break
				}
			}
			if !duplicate {
				out = append(out, value)
			}
		}
	}
	return out
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return strings.TrimSpace(header)
	}
	return strings.TrimSpace(parts[1])
}
