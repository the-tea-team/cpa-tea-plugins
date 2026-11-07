package main

import (
	"encoding/json"
	"testing"
)

func allowOnly(models ...string) func(string) bool {
	set := make(map[string]struct{}, len(models))
	for _, model := range models {
		set[model] = struct{}{}
	}
	return func(model string) bool {
		_, ok := set[model]
		return ok
	}
}

func TestFilterModelListShapes(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		allow   func(string) bool
		changed bool
	}{
		{
			name:    "openai object list",
			body:    `{"object":"list","data":[{"id":"keep","object":"model"},{"id":"drop","object":"model"}]}`,
			allow:   allowOnly("keep"),
			changed: true,
		},
		{
			name:    "claude data with model type",
			body:    `{"data":[{"id":"keep","type":"model"},{"id":"drop","type":"model"}],"has_more":false}`,
			allow:   allowOnly("keep"),
			changed: true,
		},
		{
			name:    "gemini models with names",
			body:    `{"models":[{"name":"models/keep"},{"name":"models/drop"}]}`,
			allow:   allowOnly("models/keep"),
			changed: true,
		},
		{
			name:    "codex models with slugs",
			body:    `{"models":[{"slug":"keep"},{"slug":"drop"}]}`,
			allow:   allowOnly("keep"),
			changed: true,
		},
		{
			name:    "completion response",
			body:    `{"object":"chat.completion","choices":[{"message":{"content":"hi"}}]}`,
			allow:   allowOnly("keep"),
			changed: false,
		},
		{
			name:    "generic paginated data",
			body:    `{"data":[{"id":"keep"},{"id":"drop"}],"has_more":true}`,
			allow:   allowOnly("keep"),
			changed: false,
		},
		{
			name:    "empty list",
			body:    `{"object":"list","data":[]}`,
			allow:   allowOnly("keep"),
			changed: false,
		},
		{
			name:    "invalid json",
			body:    `{"object":"list","data":[`,
			allow:   allowOnly("keep"),
			changed: false,
		},
		{
			name:    "allow all",
			body:    `{"object":"list","data":[{"id":"keep"},{"id":"drop"}]}`,
			allow:   func(string) bool { return true },
			changed: false,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			filtered, changed := filterModelList([]byte(test.body), test.allow)
			if changed != test.changed {
				t.Fatalf("changed = %v, want %v (body %s)", changed, test.changed, filtered)
			}
			if !changed && filtered != nil {
				t.Fatalf("unchanged result returned body %s", filtered)
			}
		})
	}
}

func TestFilterModelListPreservesRawNumbers(t *testing.T) {
	body := `{"object":"list","data":[{"id":"keep","created":1700000000,"meta":{"x":1}},{"id":"drop","created":1700000001}]}`
	filtered, changed := filterModelList([]byte(body), allowOnly("keep"))
	if !changed {
		t.Fatal("list was not filtered")
	}
	var parsed struct {
		Data []map[string]json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(filtered, &parsed); err != nil {
		t.Fatalf("decode filtered body: %v", err)
	}
	if len(parsed.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(parsed.Data))
	}
	if got := string(parsed.Data[0]["created"]); got != "1700000000" {
		t.Fatalf("created = %s, want 1700000000", got)
	}
}

func TestFilterModelListKeepsEntriesWithoutID(t *testing.T) {
	body := `{"object":"list","data":[{"object":"model"},{"id":"drop","object":"model"}]}`
	filtered, changed := filterModelList([]byte(body), allowOnly("keep"))
	if !changed {
		t.Fatal("list was not filtered")
	}
	var parsed struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(filtered, &parsed); err != nil {
		t.Fatalf("decode filtered body: %v", err)
	}
	if len(parsed.Data) != 1 {
		t.Fatalf("data length = %d, want 1", len(parsed.Data))
	}
}
