package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

const (
	testManagementBase      = "/v0/management"
	testComboCollection     = testManagementBase + "/plugins/tea-combos/combos"
	testComboCollectionItem = testComboCollection + "/fast-fleet"
)

func newManagementTestPlugin(t *testing.T) *combosPlugin {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "tea-combos.state.json"))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	return &combosPlugin{store: store, roundRobin: newRoundRobinSelector()}
}

func comboBody(t *testing.T, value any) []byte {
	t.Helper()
	raw, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal body: %v", err)
	}
	return raw
}

func callAPI(handler comboAPIHandler, method, path string, query url.Values, body []byte) pluginapi.ManagementResponse {
	if query == nil {
		query = url.Values{}
	}
	resp, _ := handler.HandleManagement(nil, pluginapi.ManagementRequest{
		Method: method,
		Path:   path,
		Query:  query,
		Body:   body,
	})
	return resp
}

func decodeManagementJSON(t *testing.T, resp pluginapi.ManagementResponse, into any) {
	t.Helper()
	if err := json.Unmarshal(resp.Body, into); err != nil {
		t.Fatalf("decode response %q: %v", string(resp.Body), err)
	}
}

func errorMessage(t *testing.T, resp pluginapi.ManagementResponse) string {
	t.Helper()
	var body struct {
		Error string `json:"error"`
	}
	decodeManagementJSON(t, resp, &body)
	return body.Error
}

func TestManagementListEmpty(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	resp := callAPI(comboAPIHandler{store: plugin.store}, http.MethodGet, testComboCollection, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	var body struct {
		Combos []Combo `json:"combos"`
	}
	decodeManagementJSON(t, resp, &body)
	if len(body.Combos) != 0 {
		t.Fatalf("combos = %#v, want empty", body.Combos)
	}
}

func TestManagementCreateGetUpdateDelete(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}

	combo := Combo{Name: "fast-fleet", Strategy: StrategyRoundRobin, Members: []Member{{Model: "good-model"}}}
	resp := callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, want 201; body=%s", resp.StatusCode, resp.Body)
	}

	resp = callAPI(handler, http.MethodGet, testComboCollection, nil, nil)
	var list struct {
		Combos []Combo `json:"combos"`
	}
	decodeManagementJSON(t, resp, &list)
	if len(list.Combos) != 1 || list.Combos[0].Name != "fast-fleet" {
		t.Fatalf("list = %#v, want fast-fleet", list.Combos)
	}

	resp = callAPI(handler, http.MethodGet, testComboCollectionItem, nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("get status = %d, want 200", resp.StatusCode)
	}
	var got Combo
	decodeManagementJSON(t, resp, &got)
	if got.Name != "fast-fleet" || !got.IsEnabled() {
		t.Fatalf("get = %#v, want enabled fast-fleet", got)
	}

	updated := Combo{Name: "fast-fleet", Strategy: StrategyFailover, Members: []Member{{Model: "bad-model"}, {Model: "good-model"}, {Model: "third-model", Provider: "mock"}}, TTL: "2h"}
	updated.SessionAffinity = boolPtr(true)
	resp = callAPI(handler, http.MethodPut, testComboCollectionItem, nil, comboBody(t, updated))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("update status = %d, want 200; body=%s", resp.StatusCode, resp.Body)
	}
	got, ok := plugin.store.Get("fast-fleet")
	if !ok {
		t.Fatal("updated combo missing from store")
	}
	if got.Strategy != StrategyFailover || len(got.Members) != 3 || got.TTL != "2h" {
		t.Fatalf("updated combo = %#v", got)
	}

	resp = callAPI(handler, http.MethodDelete, testComboCollectionItem, nil, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d, want 204", resp.StatusCode)
	}
	if _, ok := plugin.store.Get("fast-fleet"); ok {
		t.Fatal("combo still present after delete")
	}
}

func TestManagementCreateDuplicateConflict(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	combo := Combo{Name: "fast-fleet", Members: []Member{{Model: "good-model"}}}
	if resp := callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("first create status = %d", resp.StatusCode)
	}
	resp := callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo))
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("duplicate status = %d, want 409; body=%s", resp.StatusCode, resp.Body)
	}
	if message := errorMessage(t, resp); !strings.Contains(message, "already exists") {
		t.Fatalf("duplicate error = %q", message)
	}
}

func TestManagementValidationFailures(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	cases := []struct {
		name string
		body string
		want string
	}{
		{"missing name", `{"members":[{"model":"good-model"}]}`, "combo name is required"},
		{"empty model", `{"name":"x","members":[{"model":""}]}`, "empty model"},
		{"bad strategy", `{"name":"x","strategy":"random","members":[{"model":"good-model"}]}`, "unknown strategy"},
		{"bad ttl", `{"name":"x","ttl":"soon","members":[{"model":"good-model"}]}`, "invalid ttl"},
		{"name with slash", `{"name":"a/b","members":[{"model":"good-model"}]}`, "must not contain"},
		{"unknown field", `{"name":"x","members":[{"model":"good-model"}],"usage":1}`, "unknown field"},
		{"trailing json", `{"name":"x","members":[{"model":"good-model"}]} {}`, "single JSON object"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callAPI(handler, http.MethodPost, testComboCollection, nil, []byte(tc.body))
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, resp.Body)
			}
			if message := errorMessage(t, resp); !strings.Contains(message, tc.want) {
				t.Fatalf("error = %q, want it to contain %q", message, tc.want)
			}
		})
	}
}

func TestManagementNotFoundAndMethodNotAllowed(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	combo := Combo{Name: "fast-fleet", Members: []Member{{Model: "good-model"}}}
	callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo))

	cases := []struct {
		name   string
		method string
		path   string
		query  url.Values
		want   int
	}{
		{"get missing", http.MethodGet, testComboCollection + "/absent", nil, http.StatusNotFound},
		{"get missing query", http.MethodGet, testComboCollection, url.Values{"name": {"absent"}}, http.StatusNotFound},
		{"put missing", http.MethodPut, testComboCollection + "/absent", nil, http.StatusNotFound},
		{"delete missing", http.MethodDelete, testComboCollection + "/absent", nil, http.StatusNotFound},
		{"put without name", http.MethodPut, testComboCollection, nil, http.StatusBadRequest},
		{"delete without name", http.MethodDelete, testComboCollection, nil, http.StatusBadRequest},
		{"patch collection", http.MethodPatch, testComboCollection, nil, http.StatusMethodNotAllowed},
		{"post item", http.MethodPost, testComboCollectionItem, nil, http.StatusMethodNotAllowed},
		{"unknown route", http.MethodGet, testManagementBase + "/plugins/tea-combos/other", nil, http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp := callAPI(handler, tc.method, tc.path, tc.query, nil)
			if resp.StatusCode != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tc.want, resp.Body)
			}
			if tc.want == http.StatusMethodNotAllowed {
				if allow := resp.Headers.Get("Allow"); allow == "" {
					t.Fatal("405 without Allow header")
				}
			}
		})
	}
}

func TestManagementPutRejectsBodyNameMismatch(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	combo := Combo{Name: "fast-fleet", Members: []Member{{Model: "good-model"}}}
	callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo))

	other := Combo{Name: "other-name", Members: []Member{{Model: "good-model"}}}
	resp := callAPI(handler, http.MethodPut, testComboCollectionItem, nil, comboBody(t, other))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body=%s", resp.StatusCode, resp.Body)
	}
	if message := errorMessage(t, resp); !strings.Contains(message, "does not match") {
		t.Fatalf("error = %q", message)
	}

	// An empty body name is allowed; the path name wins.
	anonymous := Combo{Members: []Member{{Model: "good-model"}}}
	resp = callAPI(handler, http.MethodPut, testComboCollectionItem, nil, comboBody(t, anonymous))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("empty body name status = %d, want 200; body=%s", resp.StatusCode, resp.Body)
	}
}

func TestManagementQueryNameForm(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	combo := Combo{Name: "fast-fleet", Members: []Member{{Model: "good-model"}}}
	resp := callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo))
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}

	resp = callAPI(handler, http.MethodGet, testComboCollection, url.Values{"name": {"fast-fleet"}}, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query get status = %d, want 200; body=%s", resp.StatusCode, resp.Body)
	}

	updated := Combo{Members: []Member{{Model: "good-model"}, {Model: "second-model"}}}
	resp = callAPI(handler, http.MethodPut, testComboCollection, url.Values{"name": {"fast-fleet"}}, comboBody(t, updated))
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("query put status = %d, want 200; body=%s", resp.StatusCode, resp.Body)
	}

	resp = callAPI(handler, http.MethodDelete, testComboCollection, url.Values{"name": {"fast-fleet"}}, nil)
	if resp.StatusCode != http.StatusNoContent {
		t.Fatalf("query delete status = %d, want 204", resp.StatusCode)
	}
}

func TestManagementOversizeBodyRejected(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	body := append([]byte(`{"name":"x"`), bytes.Repeat([]byte(" "), managementBodyLimit)...)
	resp := callAPI(handler, http.MethodPost, testComboCollection, nil, body)
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body=%s", resp.StatusCode, resp.Body)
	}
}

func TestManagementStoreWriteFailureSurfaces(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatalf("OpenStore() error = %v", err)
	}
	// Replace the state directory with a regular file so the atomic write cannot prepare its
	// temp file. The handler must report the real failure instead of pretending success.
	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("remove dir: %v", err)
	}
	if err := os.WriteFile(dir, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("create blocking file: %v", err)
	}

	handler := comboAPIHandler{store: store}
	combo := Combo{Name: "fast-fleet", Members: []Member{{Model: "good-model"}}}
	resp := callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo))
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body=%s", resp.StatusCode, resp.Body)
	}
	if message := errorMessage(t, resp); !strings.Contains(message, "combo state") {
		t.Fatalf("error = %q, want the real write failure", message)
	}
}

func TestManagementEditsVisibleToRouteModel(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	combo := Combo{Name: "fast-fleet", Members: []Member{{Model: "good-model"}}}
	if resp := callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo)); resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d", resp.StatusCode)
	}

	route, err := plugin.RouteModel(nil, pluginapi.ModelRouteRequest{RequestedModel: "fast-fleet"})
	if err != nil {
		t.Fatalf("RouteModel() error = %v", err)
	}
	if !route.Handled || route.Target != pluginID {
		t.Fatalf("route = %#v, want handled by %s without restart", route, pluginID)
	}

	if resp := callAPI(handler, http.MethodDelete, testComboCollectionItem, nil, nil); resp.StatusCode != http.StatusNoContent {
		t.Fatalf("delete status = %d", resp.StatusCode)
	}
	route, err = plugin.RouteModel(nil, pluginapi.ModelRouteRequest{RequestedModel: "fast-fleet"})
	if err != nil {
		t.Fatalf("RouteModel() after delete error = %v", err)
	}
	if route.Handled {
		t.Fatalf("route = %#v, want unhandled after delete", route)
	}
}

func TestManagementNeverEchoesAuthorizationHeader(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	const secret = "Bearer super-secret-management-key"
	combo := Combo{Name: "fast-fleet", Members: []Member{{Model: "good-model"}}}
	resp, _ := handler.HandleManagement(nil, pluginapi.ManagementRequest{
		Method:  http.MethodPost,
		Path:    testComboCollection,
		Headers: http.Header{"Authorization": []string{secret}},
		Query:   url.Values{},
		Body:    comboBody(t, combo),
	})
	if bytes.Contains(resp.Body, []byte(secret)) || bytes.Contains(resp.Body, []byte("super-secret")) {
		t.Fatalf("response echoed the authorization header: %s", resp.Body)
	}
	for key, values := range resp.Headers {
		for _, value := range values {
			if strings.Contains(strings.ToLower(key), "authorization") || strings.Contains(value, "super-secret") {
				t.Fatalf("response header %s echoed credentials", key)
			}
		}
	}
}

func TestResourceHandlerServesStaticShell(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	handler := comboAPIHandler{store: plugin.store}
	combo := Combo{Name: "secret-combo-name", Members: []Member{{Model: "good-model"}}}
	callAPI(handler, http.MethodPost, testComboCollection, nil, comboBody(t, combo))

	for i := 0; i < 3; i++ {
		resp, err := uiHandler{}.HandleManagement(nil, pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/resource/plugins/tea-combos", Headers: http.Header{"Authorization": []string{"Bearer should-not-appear"}}})
		if err != nil {
			t.Fatalf("ui handler error = %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("status = %d, want 200", resp.StatusCode)
		}
		if !bytes.Equal(resp.Body, uiHTML) {
			t.Fatal("resource body is not byte-identical to the embedded document")
		}
		if got := resp.Headers.Get("Content-Type"); got != "text/html; charset=utf-8" {
			t.Fatalf("Content-Type = %q", got)
		}
		if got := resp.Headers.Get("Cache-Control"); got != "no-store" {
			t.Fatalf("Cache-Control = %q", got)
		}
		if got := resp.Headers.Get("X-Content-Type-Options"); got != "nosniff" {
			t.Fatalf("X-Content-Type-Options = %q", got)
		}
		if got := resp.Headers.Get("Content-Security-Policy"); got != resourceCSP {
			t.Fatalf("Content-Security-Policy = %q", got)
		}
		if strings.Contains(resourceCSP, "frame-ancestors") {
			t.Fatal("CSP must not block CPAMP's iframe")
		}
		if bytes.Contains(resp.Body, []byte("secret-combo-name")) {
			t.Fatal("resource response leaked combo state")
		}
		if bytes.Contains(resp.Body, []byte("should-not-appear")) {
			t.Fatal("resource response leaked a request header")
		}
	}

	resp, _ := uiHandler{}.HandleManagement(nil, pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/resource/plugins/tea-combos"})
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("non-GET resource status = %d, want 405", resp.StatusCode)
	}
}

func TestResourceHTMLHasNoExternalURLs(t *testing.T) {
	for _, forbidden := range []string{"http://", "https://", "//cdn"} {
		if bytes.Contains(uiHTML, []byte(forbidden)) {
			t.Fatalf("embedded HTML contains %q; the page must be self-contained", forbidden)
		}
	}
	if len(uiHTML) == 0 {
		t.Fatal("embedded HTML is empty")
	}
}

func TestPluginMetadataLogoIsInlineSVG(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	logo := plugin.plugin().Metadata.Logo
	const prefix = "data:image/svg+xml;base64,"
	if !strings.HasPrefix(logo, prefix) {
		t.Fatalf("Logo = %q, want an inline SVG data URI", logo)
	}
	if len(logo) > 2048 {
		t.Fatalf("Logo is %d bytes, want under 2KB", len(logo))
	}
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(logo, prefix))
	if err != nil {
		t.Fatalf("Logo base64: %v", err)
	}
	if !bytes.HasPrefix(raw, []byte("<svg")) || !bytes.HasSuffix(bytes.TrimSpace(raw), []byte("</svg>")) {
		t.Fatalf("Logo payload is not an SVG document: %q", raw)
	}
}

func TestManagementRegistrationShape(t *testing.T) {
	plugin := newManagementTestPlugin(t)
	resp, err := plugin.RegisterManagement(nil, pluginapi.ManagementRegistrationRequest{BasePath: testManagementBase})
	if err != nil {
		t.Fatalf("RegisterManagement() error = %v", err)
	}
	if len(resp.Resources) != 1 {
		t.Fatalf("resources = %d, want 1", len(resp.Resources))
	}
	resource := resp.Resources[0]
	if resource.Path != "/combos" || resource.Menu != "Combos" || resource.Description != "Named model combos, strategies and session affinity" {
		t.Fatalf("resource = %#v", resource)
	}
	if resource.Handler == nil {
		t.Fatal("resource has no handler")
	}
	if len(resp.Routes) != 4 {
		t.Fatalf("routes = %d, want 4", len(resp.Routes))
	}
	methods := map[string]bool{}
	for _, route := range resp.Routes {
		methods[route.Method] = true
		if route.Path != testComboCollection {
			t.Fatalf("route path = %q, want %q from the host BasePath", route.Path, testComboCollection)
		}
		if route.Handler == nil {
			t.Fatal("route has no handler")
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete} {
		if !methods[method] {
			t.Fatalf("missing %s route", method)
		}
	}

	withoutBase, err := plugin.RegisterManagement(nil, pluginapi.ManagementRegistrationRequest{})
	if err == nil || len(withoutBase.Routes) != 0 {
		t.Fatalf("RegisterManagement() without BasePath = %#v, %v; want an error", withoutBase, err)
	}
}

// --- ABI wiring ---

type abiTestEnvelope struct {
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

func registerTestPlugin(t *testing.T) *combosPlugin {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "tea-combos.state.json")
	config, err := json.Marshal(abiLifecycleRequest{ConfigYAML: []byte("statePath: " + statePath + "\n")})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	raw, err := abiHandleMethod(pluginabi.MethodPluginRegister, config)
	if err != nil {
		t.Fatalf("plugin.register error = %v", err)
	}
	var env abiTestEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("plugin.register envelope = %s (%v)", raw, err)
	}
	plugin := currentPlugin.Load()
	if plugin == nil {
		t.Fatal("plugin.register did not store the plugin")
	}
	return plugin
}

func TestABIManagementRegisterShape(t *testing.T) {
	registerTestPlugin(t)
	request, _ := json.Marshal(pluginapi.ManagementRegistrationRequest{BasePath: testManagementBase})
	raw, err := abiHandleMethod(pluginabi.MethodManagementRegister, request)
	if err != nil {
		t.Fatalf("management.register error = %v", err)
	}
	var env abiTestEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("management.register envelope = %s (%v)", raw, err)
	}
	var resp struct {
		Routes []struct {
			Method string `json:"Method"`
			Path   string `json:"Path"`
		} `json:"routes"`
		Resources []struct {
			Path        string `json:"Path"`
			Menu        string `json:"Menu"`
			Description string `json:"Description"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if len(resp.Routes) != 4 || len(resp.Resources) != 1 {
		t.Fatalf("routes/resources = %d/%d, want 4/1", len(resp.Routes), len(resp.Resources))
	}
	if resp.Resources[0].Menu != "Combos" || resp.Resources[0].Path != "/combos" {
		t.Fatalf("resource = %#v", resp.Resources[0])
	}
	for _, route := range resp.Routes {
		if route.Path != testComboCollection {
			t.Fatalf("route path = %q", route.Path)
		}
	}
	if logo := currentPlugin.Load().plugin().Metadata.Logo; !strings.HasPrefix(logo, "data:image/svg+xml") {
		t.Fatalf("registration metadata lacks the logo: %q", logo)
	}
}

func TestABIManagementHandleCRUDAndResource(t *testing.T) {
	registerTestPlugin(t)
	collection := testComboCollection

	create := map[string]any{"Method": http.MethodPost, "Path": collection, "Query": map[string][]string{}, "Body": []byte(`{"name":"abi-combo","members":[{"model":"good-model"}]}`)}
	raw, err := abiHandleMethod(pluginabi.MethodManagementHandle, comboBody(t, create))
	if err != nil {
		t.Fatalf("management.handle create error = %v", err)
	}
	var env abiTestEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("create envelope = %s (%v)", raw, err)
	}
	var created pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &created); err != nil {
		t.Fatalf("decode create response: %v", err)
	}
	if created.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d; body=%s", created.StatusCode, created.Body)
	}

	list := map[string]any{"Method": http.MethodGet, "Path": collection, "Query": map[string][]string{}, "Headers": map[string][]string{"Authorization": {"Bearer abi-secret-key"}}}
	raw, err = abiHandleMethod(pluginabi.MethodManagementHandle, comboBody(t, list))
	if err != nil {
		t.Fatalf("management.handle list error = %v", err)
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("list envelope = %s (%v)", raw, err)
	}
	var listed pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &listed); err != nil {
		t.Fatalf("decode list response: %v", err)
	}
	if listed.StatusCode != http.StatusOK || !bytes.Contains(listed.Body, []byte("abi-combo")) {
		t.Fatalf("list = %d %s", listed.StatusCode, listed.Body)
	}
	if bytes.Contains(listed.Body, []byte("abi-secret-key")) {
		t.Fatal("management.handle echoed the authorization header")
	}

	resource := map[string]any{"Method": http.MethodGet, "Path": "/v0/resource/plugins/tea-combos"}
	raw, err = abiHandleMethod(pluginabi.MethodManagementHandle, comboBody(t, resource))
	if err != nil {
		t.Fatalf("management.handle resource error = %v", err)
	}
	if err := json.Unmarshal(raw, &env); err != nil || !env.OK {
		t.Fatalf("resource envelope = %s (%v)", raw, err)
	}
	var shell pluginapi.ManagementResponse
	if err := json.Unmarshal(env.Result, &shell); err != nil {
		t.Fatalf("decode resource response: %v", err)
	}
	if !bytes.Equal(shell.Body, uiHTML) {
		t.Fatal("ABI resource response is not the embedded shell")
	}
	if bytes.Contains(shell.Body, []byte("abi-combo")) {
		t.Fatal("ABI resource response leaked combo state")
	}
}

func TestABIManagementBeforeRegistrationFailsClosed(t *testing.T) {
	currentPlugin.Store(nil)
	raw, err := abiHandleMethod(pluginabi.MethodManagementRegister, []byte(`{"BasePath":"/v0/management"}`))
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	var env abiTestEnvelope
	if err := json.Unmarshal(raw, &env); err != nil || env.OK || env.Error == nil || env.Error.Code != "not_ready" {
		t.Fatalf("envelope = %s, want not_ready", raw)
	}
}
