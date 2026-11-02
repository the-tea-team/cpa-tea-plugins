package main

import (
	"context"
	_ "embed"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// uiHTML is the one self-contained management page. It has no build step, no external
// stylesheet, font or script, and no data: the resource route must stay static.
//
//go:embed ui/index.html
var uiHTML []byte

// pluginLogo is a small inline SVG mark. A data URI means the sidebar icon needs no request to a
// host path and no second asset endpoint.
const pluginLogo = "data:image/svg+xml;base64,PHN2ZyB4bWxucz0iaHR0cDovL3d3dy53My5vcmcvMjAwMC9zdmciIHZpZXdCb3g9IjAgMCA2NCA2NCI+PHJlY3Qgd2lkdGg9IjY0IiBoZWlnaHQ9IjY0IiByeD0iMTQiIGZpbGw9IiMxZjI5MzciLz48cGF0aCBkPSJNMTkgMzJsMTMtMTRNMTkgMzJsMTMgMTQiIHN0cm9rZT0iI2Y5ZmFmYiIgc3Ryb2tlLXdpZHRoPSIzIiBzdHJva2UtbGluZWNhcD0icm91bmQiLz48Y2lyY2xlIGN4PSIxOCIgY3k9IjMyIiByPSI2IiBmaWxsPSIjZjlmYWZiIi8+PGNpcmNsZSBjeD0iMzMiIGN5PSIxOSIgcj0iNiIgZmlsbD0iIzljYTNhZiIvPjxjaXJjbGUgY3g9IjMzIiBjeT0iNDUiIHI9IjYiIGZpbGw9IiM5Y2EzYWYiLz48L3N2Zz4="

// resourceCSP keeps the page inert: no remote loads, no framing directives (CPAMP embeds the page
// in an iframe, so frame-ancestors must stay unset), and connectivity limited to the CPA origin.
const resourceCSP = "default-src 'none'; style-src 'unsafe-inline'; script-src 'unsafe-inline'; connect-src 'self'; img-src data:; base-uri 'none'; form-action 'none'"

// uiHandler serves the static resource shell. Resource routes are not management-authenticated
// (CPA's pluginResourceNoRoute skips the middleware), so this handler never reads or returns
// combo state, credentials or request headers.
type uiHandler struct{}

// HandleManagement implements pluginapi.ManagementHandler for the resource route.
func (uiHandler) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if !strings.EqualFold(strings.TrimSpace(req.Method), http.MethodGet) {
		return jsonError(http.StatusMethodNotAllowed, "method not allowed"), nil
	}
	return pluginapi.ManagementResponse{
		StatusCode: http.StatusOK,
		Headers: http.Header{
			"Content-Type":           []string{"text/html; charset=utf-8"},
			"Cache-Control":          []string{"no-store"},
			"X-Content-Type-Options": []string{"nosniff"},
			"Content-Security-Policy": []string{
				resourceCSP,
			},
		},
		Body: append([]byte(nil), uiHTML...),
	}, nil
}

// handleManagementRequest routes a management.handle ABI call to the right handler: resource
// paths go to the static shell, everything else to the authenticated CRUD API.
func (p *combosPlugin) handleManagementRequest(ctx context.Context, req pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	if strings.Contains(req.Path, "/resource/plugins/"+pluginID) {
		return uiHandler{}.HandleManagement(ctx, req)
	}
	return comboAPIHandler{store: p.store}.HandleManagement(ctx, req)
}
