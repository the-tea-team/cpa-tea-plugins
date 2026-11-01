package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"unicode"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// C13 finding, verified against CPA v8.0.51 (/root/reference/CLIProxyAPI):
//
// The host does NOT populate cliproxyexecutor.CanonicalSessionIDMetadataKey for a plugin
// executor request. The plugin-executor route is built by
// sdk/api/handlers/handlers_execution.go:198 executeWithPluginExecutor, which calls
// handlers_execution.go:284 pluginExecutorRequest. That builder fills Metadata from
// requestExecutionMetadata (Idempotency-Key, request path, pinned auth) at
// handlers_execution.go:314 and never calls cliproxysession.Enrich. Enrich is only called on
// the core Manager paths (sdk/cliproxy/auth/conductor_execution.go:125, 187, 242), which a
// plugin executor route bypasses: handlers_execution.go:226 hands the request straight to
// host.ExecutePluginExecutor. The session hierarchy helper used on this path,
// sdk/api/handlers/handlers.go:325 EnrichContextWithSessionHierarchy, only writes the session
// id into the logging context; it does not add canonical_session_id to request metadata.
// Finally internal/pluginhost/adapters_executors.go:1329 copies req.Metadata and
// opts.Metadata verbatim in buildExecutorRequest, so no canonical id appears from elsewhere.
//
// CanonicalSessionIDMetadataKey lives in sdk/cliproxy/executor (types.go:61), but that package
// imports sdk/translator, which imports the host's internal packages and a large dependency
// tree. A plugin should not pull the host in for one string, so we mirror the constant value and
// cite the source; if the host ever renames it, prior E2E runs and this cite catch it.
//
// Inbound canonical ids are hashed with the same SHA-256 as our derived keys, so only the hash
// reaches the state file or logs.
const canonicalSessionIDMetadataKey = "canonical_session_id"

// sessionKeyVersion separates our keys from anything else hashed the same way.
const sessionKeyVersion = "tea-combos-session-v1"

var legacyClaudeSessionPattern = regexp.MustCompile(`_session_([a-f0-9-]+)$`)

// deriveSessionKey returns the affinity key for one request, scoped to the combo so that two
// combos never share a binding. It hashes the derived identity with SHA-256: raw header values
// and prompt text never reach the state file or logs. The boolean is false when no identity can
// be derived, in which case the request has no affinity and behaves as the plain strategy.
//
// Precedence mirrors cliproxysession.ExtractSessionInfo for the sources we cover. The README's
// "Session identity" section lists every difference from core.
func deriveSessionKey(combo Combo, req pluginapi.ExecutorRequest) (string, bool) {
	identity := sessionIdentity(req)
	if identity == "" {
		return "", false
	}
	return hashSessionKey(combo.Name, identity), true
}

// sessionKey is the selector's session hint, used by the streaming seam. The non-stream executor
// uses Affinity.Resolve, whose key is scoped by the combo it loaded; here the requested model is
// the combo name, so the two agree. A hash is returned so no raw identity reaches a selector.
func sessionKey(req pluginapi.ExecutorRequest) string {
	comboName := strings.TrimSpace(req.Model)
	if comboName == "" {
		return ""
	}
	identity := sessionIdentity(req)
	if identity == "" {
		return ""
	}
	return hashSessionKey(comboName, identity)
}

func hashSessionKey(comboName, identity string) string {
	sum := sha256.Sum256([]byte(sessionKeyVersion + "\x00" + comboName + "\x00" + identity))
	return hex.EncodeToString(sum[:])
}

// sessionIdentity returns a bounded, human-readable identity string. It is never persisted:
// deriveSessionKey hashes it first.
func sessionIdentity(req pluginapi.ExecutorRequest) string {
	if canonical := normalizeSessionCandidate(metadataString(req.Metadata, canonicalSessionIDMetadataKey)); canonical != "" {
		return "canonical:" + canonical
	}

	headers := req.Headers
	payload := req.Payload
	if len(payload) == 0 {
		payload = req.OriginalRequest
	}

	// Core tier 1: Anthropic / Claude Code headers.
	if sid := sessionHeaderValue(headers, "X-Claude-Code-Session-Id"); sid != "" {
		return claudeIdentity(sid, sessionHeaderValue(headers, "X-Claude-Code-Agent-Id"))
	}

	// Core tier 2: Claude Code metadata.user_id in the payload.
	if sid, agentID := claudePayloadIdentity(payload); sid != "" {
		return claudeIdentity(sid, agentID)
	}

	// Core tier 3: OpenAI / Codex CLI headers and turn metadata.
	if identity := codexIdentity(headers, payload); identity != "" {
		return identity
	}

	// Core tier 5 headers: OpenCode, Pi slot and generic conversation headers. Core's order is
	// kept exactly, including the header conversation ids before any body inspection.
	if sid := sessionHeaderValue(headers, "X-Session-ID"); sid != "" {
		return "header:" + sid
	}
	if sid := sessionHeaderValue(headers, "X-Session-Affinity"); sid != "" {
		return "affinity:" + sid
	}
	if sid := sessionHeaderValue(headers, "X-Slot-Session-Id"); sid != "" {
		return "slot:" + sid
	}
	if sid := sessionHeaderValue(headers, "X-Conversation-Id"); sid != "" {
		return "conv:" + sid
	}
	if sid := sessionHeaderValue(headers, "X-Thread-Id"); sid != "" {
		return "thread:" + sid
	}

	// Core tier 6 body fields, in core's order: prompt_cache_key, then the conversation object,
	// then legacy conversation ids.
	root, nested := sessionPayloadRoot(payload)
	if pck := sessionBodyString(root, nested, "prompt_cache_key", "promptCacheKey"); pck != "" {
		return "pck:" + pck
	}
	if conversationID := sessionConversationID(root, nested); conversationID != "" {
		return "conv:" + conversationID
	}
	if conversationID := sessionBodyString(root, nested,
		"conversation_id", "conversationId", "chat_id", "chatId",
		"metadata.conversation_id", "extra_body.conversation_id"); conversationID != "" {
		return "conv:" + conversationID
	}

	// Last resort, and the only source core does not use as a session identity: a hash of the
	// first user message. The text is returned raw here but deriveSessionKey hashes it.
	if text := firstUserMessageText(payload); text != "" {
		return "message:" + text
	}
	return ""
}

func claudeIdentity(sessionID, agentID string) string {
	if agentID != "" && agentID != "main" {
		return "claude:" + sessionID + ":agent:" + agentID
	}
	return "claude:" + sessionID
}

// claudePayloadIdentity mirrors the metadata.user_id surface of ClaudeMetadataIdentities: a JSON
// object carrying session_id, or the legacy "_session_<id>" user_id suffix.
func claudePayloadIdentity(payload []byte) (string, string) {
	root, nested := sessionPayloadRoot(payload)
	userID := sessionBodyString(root, nested, "metadata.user_id")
	if userID == "" {
		return "", ""
	}
	if strings.HasPrefix(userID, "{") {
		var doc map[string]any
		if err := json.Unmarshal([]byte(userID), &doc); err != nil {
			return "", ""
		}
		sid := normalizeSessionCandidate(anyString(doc["session_id"]))
		agentID := normalizeSessionCandidate(anyString(doc["agent_id"]))
		if agentID == "" {
			agentID = normalizeSessionCandidate(anyString(doc["subagent_id"]))
		}
		return sid, agentID
	}
	if matches := legacyClaudeSessionPattern.FindStringSubmatch(userID); len(matches) >= 2 {
		return normalizeSessionCandidate(matches[1]), ""
	}
	return "", ""
}

// codexIdentity mirrors core's Codex header tier: Session-Id/Session_id, Thread-Id, and
// X-Codex-Turn-Metadata, preferring the session id over the thread id.
func codexIdentity(headers http.Header, payload []byte) string {
	sid := sessionHeaderValue(headers, "Session-Id")
	if sid == "" {
		sid = sessionHeaderValue(headers, "Session_id")
	}
	tid := sessionHeaderValue(headers, "Thread-Id")
	if tid == "" {
		tid = sessionHeaderValue(headers, "Thread_id")
	}

	if turnMetadata := sessionHeaderValue(headers, "X-Codex-Turn-Metadata"); turnMetadata != "" {
		var turn map[string]any
		if err := json.Unmarshal([]byte(turnMetadata), &turn); err == nil {
			if sid == "" {
				sid = normalizeSessionCandidate(anyString(turn["session_id"]))
			}
			if tid == "" {
				tid = normalizeSessionCandidate(anyString(turn["thread_id"]))
			}
		}
	}

	if sid == "" && tid == "" {
		return ""
	}
	if sid != "" {
		return "codex:" + sid
	}
	return "codex:" + tid
}

// sessionPayloadRoot parses the payload once. Core treats a nested "request" object as the
// request root only when the payload has no top-level "contents" (Gemini style).
func sessionPayloadRoot(payload []byte) (map[string]any, map[string]any) {
	if len(payload) == 0 {
		return nil, nil
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return nil, nil
	}
	request, ok := root["request"].(map[string]any)
	if !ok {
		return root, nil
	}
	if _, hasContents := root["contents"]; hasContents {
		return root, nil
	}
	return root, request
}

// sessionBodyString reads the first non-empty candidate path, checking the top-level root and
// then the nested request root, exactly like core's hasNestedReq handling.
func sessionBodyString(root, nested map[string]any, paths ...string) string {
	for _, path := range paths {
		if value := normalizeSessionCandidate(anyString(bodyPath(root, path))); value != "" {
			return value
		}
		if value := normalizeSessionCandidate(anyString(bodyPath(nested, path))); value != "" {
			return value
		}
	}
	return ""
}

func bodyPath(root map[string]any, path string) any {
	if root == nil {
		return nil
	}
	current := any(root)
	for _, segment := range strings.Split(path, ".") {
		object, ok := current.(map[string]any)
		if !ok {
			return nil
		}
		current, ok = object[segment]
		if !ok {
			return nil
		}
	}
	return current
}

// sessionConversationID accepts either {"conversation":{"id":"..."}} or a string conversation.
func sessionConversationID(root, nested map[string]any) string {
	for _, source := range []map[string]any{root, nested} {
		conversation, ok := bodyPath(source, "conversation").(map[string]any)
		if ok {
			if id := normalizeSessionCandidate(anyString(conversation["id"])); id != "" {
				return id
			}
		}
		if text := normalizeSessionCandidate(anyString(bodyPath(source, "conversation"))); text != "" {
			return text
		}
	}
	return ""
}

// firstUserMessageText extracts the first user message for the formats CPA carries. It is a
// deliberately simple extractor: it is only the last-resort identity and the result is hashed.
func firstUserMessageText(payload []byte) string {
	if len(payload) == 0 {
		return ""
	}
	var root map[string]any
	if err := json.Unmarshal(payload, &root); err != nil {
		return ""
	}
	nested := root
	if candidate, ok := root["request"].(map[string]any); ok {
		if _, hasContents := root["contents"]; !hasContents {
			nested = candidate
		}
	}

	for _, source := range []map[string]any{root, nested} {
		if text := firstMessageText(source); text != "" {
			return text
		}
		if text := firstContentsText(source); text != "" {
			return text
		}
		if text := firstInputText(source); text != "" {
			return text
		}
	}
	return ""
}

func firstMessageText(root map[string]any) string {
	messages, _ := bodyPath(root, "messages").([]any)
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(anyString(message["role"])) != "user" {
			continue
		}
		if text := contentText(message["content"]); text != "" {
			return text
		}
	}
	return ""
}

func firstContentsText(root map[string]any) string {
	contents, _ := bodyPath(root, "contents").([]any)
	for _, raw := range contents {
		content, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		if strings.ToLower(anyString(content["role"])) != "user" {
			continue
		}
		if text := contentText(content["parts"]); text != "" {
			return text
		}
	}
	return ""
}

func firstInputText(root map[string]any) string {
	switch input := bodyPath(root, "input").(type) {
	case string:
		return input
	case []any:
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if strings.ToLower(anyString(item["role"])) != "user" {
				continue
			}
			if text := contentText(item["content"]); text != "" {
				return text
			}
		}
	}
	return ""
}

// contentText flattens a string or a list of content parts into their text.
func contentText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var builder strings.Builder
		for _, raw := range typed {
			part, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			if text := anyString(part["text"]); text != "" {
				builder.WriteString(text)
			}
		}
		return builder.String()
	}
	return ""
}

func anyString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case json.Number:
		return typed.String()
	case float64:
		return strconv.FormatFloat(typed, 'f', -1, 64)
	}
	return ""
}

func metadataString(metadata map[string]any, key string) string {
	if metadata == nil {
		return ""
	}
	return anyString(metadata[key])
}

// sessionHeaderValue mirrors core's header lookup: canonical lookup first, then a
// case-insensitive scan so unusual clients still match.
func sessionHeaderValue(headers http.Header, name string) string {
	if headers == nil {
		return ""
	}
	if value := normalizeSessionCandidate(headers.Get(name)); value != "" {
		return value
	}
	for key, values := range headers {
		if !strings.EqualFold(key, name) {
			continue
		}
		for _, value := range values {
			if normalized := normalizeSessionCandidate(value); normalized != "" {
				return normalized
			}
		}
	}
	return ""
}

// normalizeSessionCandidate mirrors core's NormalizeExplicitID: no control characters, trimmed,
// non-empty and bounded to 256 bytes so state keys and logs stay bounded.
func normalizeSessionCandidate(raw string) string {
	for _, r := range raw {
		if unicode.IsControl(r) {
			return ""
		}
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || len(raw) > 256 {
		return ""
	}
	return raw
}
