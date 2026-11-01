package main

import (
	"strings"
	"testing"
)

// TestEmbeddedUIPreservesDefaultAffinity guards the editor's affinity default. A combo that omits
// sessionAffinity is affinity-enabled (Combo.IsEnabled/default rules), so the page must show it as
// on and must not send false merely because an operator opened and saved an existing combo.
func TestEmbeddedUIPreservesDefaultAffinity(t *testing.T) {
	page := string(uiHTML)
	if !strings.Contains(page, `<input id="combo-affinity" name="sessionAffinity" type="checkbox" checked>`) {
		t.Fatal("new-combo editor must default session affinity to checked")
	}
	if !strings.Contains(page, "combo.sessionAffinity === false") {
		t.Fatal("editor must treat an omitted sessionAffinity as enabled")
	}
}
