package main

import (
	"os"
	"strings"
	"testing"
)

func TestDashboardContract(t *testing.T) {
	b, err := os.ReadFile("ui/index.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	for _, want := range []string{"Capacity &amp; resets", "Asia/Riyadh", "/v0/management/plugins/tea-quota-router/", "textContent", "credentials: 'same-origin'", "weighted remaining", "Review changes"} {
		if !strings.Contains(s, want) {
			t.Errorf("dashboard missing %q", want)
		}
	}
	for _, forbidden := range []string{"innerHTML", "localStorage", "sessionStorage"} {
		if strings.Contains(s, forbidden) {
			t.Errorf("dashboard contains forbidden %q", forbidden)
		}
	}
}
