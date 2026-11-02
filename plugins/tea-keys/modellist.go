package main

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// filterModelList filters a model catalog returned by /v1/models, /v1beta/models or
// the Codex client-version catalog. It returns changed=false when the body is not a
// model catalog or when every entry is allowed, so ordinary completion responses and
// untouched catalogs keep their original bytes.
func filterModelList(body []byte, allow func(model string) bool) ([]byte, bool) {
	if len(body) == 0 || allow == nil {
		return nil, false
	}

	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil || top == nil {
		return nil, false
	}
	if _, isCompletion := top["choices"]; isCompletion {
		return nil, false
	}

	listKey := modelListKey(top)
	if listKey == "" {
		return nil, false
	}

	var entries []json.RawMessage
	if err := json.Unmarshal(top[listKey], &entries); err != nil {
		return nil, false
	}
	if len(entries) == 0 {
		return nil, false
	}

	kept := make([]json.RawMessage, 0, len(entries))
	var firstID, lastID string
	dropped := false
	for _, entry := range entries {
		id := entryModelID(entry)
		if id == "" {
			// An entry without an identifier cannot be requested by name; keep it
			// rather than silently removing data we do not understand.
			kept = append(kept, entry)
			continue
		}
		if !allow(id) {
			dropped = true
			continue
		}
		if firstID == "" {
			firstID = id
		}
		lastID = id
		kept = append(kept, entry)
	}
	if !dropped {
		return nil, false
	}

	encoded, err := json.Marshal(kept)
	if err != nil {
		return nil, false
	}
	top[listKey] = encoded

	// Claude model lists carry cursor-style fields. Recompute the ones that exist
	// so a filtered list does not advertise a stale first/last entry or more pages.
	if _, exists := top["has_more"]; exists {
		top["has_more"] = json.RawMessage("false")
	}
	if _, exists := top["first_id"]; exists {
		top["first_id"] = jsonString(firstID)
	}
	if _, exists := top["last_id"]; exists {
		top["last_id"] = jsonString(lastID)
	}
	// A provider cursor would paginate over the unfiltered catalog; drop it.
	delete(top, "nextPageToken")

	filtered, err := json.Marshal(top)
	if err != nil {
		return nil, false
	}
	return filtered, true
}

func modelListKey(top map[string]json.RawMessage) string {
	if object, ok := top["object"]; ok && strings.EqualFold(rawJSONStringValue(object), "list") {
		if _, hasData := top["data"]; hasData {
			return "data"
		}
	}
	if data, exists := top["data"]; exists && entriesLookLikeModels(data) {
		return "data"
	}
	if models, exists := top["models"]; exists && entriesLookLikeModels(models) {
		return "models"
	}
	return ""
}

func entriesLookLikeModels(raw json.RawMessage) bool {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil || len(entries) == 0 {
		return false
	}
	for _, entry := range entries {
		var meta struct {
			Object string `json:"object"`
			Type   string `json:"type"`
			Slug   string `json:"slug"`
			Name   string `json:"name"`
		}
		if err := json.Unmarshal(entry, &meta); err != nil {
			continue
		}
		if strings.EqualFold(meta.Object, "model") || strings.EqualFold(meta.Type, "model") {
			return true
		}
		if meta.Slug != "" || strings.HasPrefix(meta.Name, "models/") {
			return true
		}
	}
	return false
}

func entryModelID(raw json.RawMessage) string {
	var entry struct {
		ID   string `json:"id"`
		Slug string `json:"slug"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(raw, &entry); err != nil {
		return ""
	}
	if entry.ID != "" {
		return entry.ID
	}
	if entry.Slug != "" {
		return entry.Slug
	}
	return entry.Name
}

func rawJSONStringValue(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return value
}

func jsonString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func responseWithModelList(headers http.Header, body []byte) (http.Header, []byte) {
	out := make(http.Header, len(headers)+1)
	for key, values := range headers {
		out[key] = append([]string(nil), values...)
	}
	out.Set("Content-Length", strconv.Itoa(len(body)))
	return out, body
}
