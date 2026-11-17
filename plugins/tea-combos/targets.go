package main

import (
	"context"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// Pin targets are a deliberately narrow projection of the shared inventory. Never pass a raw
// host auth response or snapshot through this endpoint: both have fields this UI does not need.
type pinAccountTarget struct {
	ID       string `json:"id"`
	Provider string `json:"provider"`
	Label    string `json:"label"`
}
type pinConnectionTarget struct {
	ID          string   `json:"id"`
	AccountID   string   `json:"accountId"`
	Provider    string   `json:"provider"`
	Label       string   `json:"label"`
	Models      []string `json:"models"`
	Disabled    bool     `json:"disabled"`
	Unavailable bool     `json:"unavailable"`
}
type pinTargets struct {
	Accounts    []pinAccountTarget    `json:"accounts"`
	Connections []pinConnectionTarget `json:"connections"`
}

type pinTargetsHandler struct{ source snapshotSource }

func (h pinTargetsHandler) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet) {
		r := jsonError(http.StatusMethodNotAllowed, "method not allowed")
		r.Headers.Set("Allow", http.MethodGet)
		return r, nil
	}
	unavailable := func() (pluginapi.ManagementResponse, error) {
		return jsonError(http.StatusServiceUnavailable, "Pin target inventory is unavailable or stale"), nil
	}
	if h.source == nil {
		return unavailable()
	}
	now := time.Now()
	snap, maxAge, err := h.source.freshSnapshot(now)
	if err != nil || snap.GeneratedAt.IsZero() || snap.GeneratedAt.After(now) || (maxAge > 0 && now.Sub(snap.GeneratedAt) > maxAge) {
		return unavailable()
	}
	out := pinTargets{Accounts: make([]pinAccountTarget, 0, len(snap.Accounts)), Connections: make([]pinConnectionTarget, 0, len(snap.Credentials))}
	for _, a := range snap.Accounts {
		out.Accounts = append(out.Accounts, pinAccountTarget{ID: a.ID, Provider: a.Provider, Label: a.Label})
	}
	for _, c := range snap.Credentials {
		models := append([]string{}, c.Models...)
		out.Connections = append(out.Connections, pinConnectionTarget{ID: c.ID, AccountID: c.AccountID, Provider: c.Provider, Label: c.Label, Models: models, Disabled: c.Disabled, Unavailable: c.Unavailable})
	}
	sort.Slice(out.Accounts, func(i, j int) bool { return out.Accounts[i].ID < out.Accounts[j].ID })
	sort.Slice(out.Connections, func(i, j int) bool { return out.Connections[i].ID < out.Connections[j].ID })
	return jsonResponse(http.StatusOK, out), nil
}
func (p *combosPlugin) targetSource() snapshotSource {
	if p == nil || p.accounts == nil {
		return nil
	}
	return p.accounts.src
}
