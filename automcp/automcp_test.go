package automcp_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/brizzai/auto-mcp/automcp"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const petSpec = `{
  "openapi": "3.0.1",
  "info": {"title": "Pet", "version": "1.0"},
  "paths": {
    "/pet/{petId}": {"get": {
      "operationId": "getPetById",
      "summary": "Find pet by ID",
      "parameters": [{"name": "petId", "in": "path", "required": true, "schema": {"type": "string"}}],
      "responses": {"200": {"description": "OK", "content": {"application/json": {"schema": {
        "type": "object", "properties": {"name": {"type": "string"}}}}}}}}},
    "/pet": {"post": {
      "operationId": "addPet",
      "requestBody": {"required": true, "content": {"application/json": {"schema": {
        "type": "object", "required": ["name"], "properties": {"name": {"type": "string"}}}}}},
      "responses": {"200": {"description": "OK"}}}}}}`

// upstream records what the built service actually sends.
func upstream(t *testing.T) (*httptest.Server, *[]*http.Request) {
	t.Helper()
	var seen []*http.Request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = append(seen, r)
		w.Header().Set("content-type", "application/json")
		_, _ = w.Write([]byte(`{"name":"Rex","internalTrace":"noise"}`))
	}))
	t.Cleanup(srv.Close)
	return srv, &seen
}

// session registers a built service on a server and connects a client to it in
// memory, which is the shape an embedding host uses: it owns the mcp.Server.
func session(t *testing.T, service *automcp.Service) *mcp.ClientSession {
	t.Helper()
	server := mcp.NewServer(&mcp.Implementation{Name: "host", Version: "1.0.0"}, nil)
	service.Register(server)

	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	ctx := t.Context()
	go func() { _ = server.Run(ctx, serverTransport) }()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1.0.0"}, nil)
	cs, err := client.Connect(ctx, clientTransport, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// A document read from memory becomes tools on the host's own MCP server. There
// is no file path and no second process: the host stores the document wherever it
// likes and hands over a reader.
func TestBuild_RegistersToolsOnTheHostsServer(t *testing.T) {
	api, _ := upstream(t)

	service, err := automcp.Build(automcp.Options{
		Spec:    strings.NewReader(petSpec),
		BaseURL: api.URL,
	})
	require.NoError(t, err)

	tools, err := session(t, service).ListTools(t.Context(), nil)
	require.NoError(t, err)

	names := []string{}
	for _, tool := range tools.Tools {
		names = append(names, tool.Name)
	}
	assert.ElementsMatch(t, []string{"getPetById", "addPet"}, names)
}

// Calling a tool reaches the upstream at the configured address.
func TestBuild_CallsReachTheUpstream(t *testing.T) {
	api, seen := upstream(t)

	service, err := automcp.Build(automcp.Options{
		Spec:    strings.NewReader(petSpec),
		BaseURL: api.URL,
		Headers: map[string]string{"X-API-Key": "AK-live"},
	})
	require.NoError(t, err)

	result, err := session(t, service).CallTool(t.Context(), &mcp.CallToolParams{
		Name:      "getPetById",
		Arguments: map[string]any{"petId": "7"},
	})
	require.NoError(t, err)
	assert.False(t, result.IsError)

	require.Len(t, *seen, 1)
	request := (*seen)[0]
	assert.Equal(t, "/pet/7", request.URL.Path)
	assert.Equal(t, "AK-live", request.Header.Get("X-API-Key"),
		"a static header configured by the host reaches the upstream")
}

// The curation file is read from memory too, so a host that stores it in a
// database is not forced to write it to disk first.
func TestBuild_AdjustmentFromMemoryApplies(t *testing.T) {
	api, _ := upstream(t)
	const adjustment = `
routes:
  - path: /pet/{petId}
    methods: [get]
descriptions:
  - path: /pet/{petId}
    updates:
      - method: get
        new_description: "curated"
responses:
  - path: /pet/{petId}
    updates:
      - method: get
        body: "{{ .name }}"
`

	service, err := automcp.Build(automcp.Options{
		Spec:       strings.NewReader(petSpec),
		Adjustment: strings.NewReader(adjustment),
		BaseURL:    api.URL,
	})
	require.NoError(t, err)

	cs := session(t, service)
	tools, err := cs.ListTools(t.Context(), nil)
	require.NoError(t, err)
	require.Len(t, tools.Tools, 1, "the route filter applied")
	assert.Equal(t, "getPetById", tools.Tools[0].Name)
	assert.Equal(t, "curated", tools.Tools[0].Description)

	result, err := cs.CallTool(t.Context(), &mcp.CallToolParams{
		Name: "getPetById", Arguments: map[string]any{"petId": "7"}})
	require.NoError(t, err)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok)
	assert.Equal(t, "Rex", text.Text, "the response template applied")
}

// Tools are also inspectable without a server, which is what an onboarding UI
// needs in order to preview a document before committing to it.
func TestBuild_ToolsAndSizeAreInspectable(t *testing.T) {
	api, _ := upstream(t)

	service, err := automcp.Build(automcp.Options{Spec: strings.NewReader(petSpec), BaseURL: api.URL})
	require.NoError(t, err)

	assert.Len(t, service.Tools(), 2)
	assert.Positive(t, service.SchemaBytes(), "the published size is reportable before serving")

	for _, tool := range service.Tools() {
		assert.NotNil(t, tool.Tool)
		assert.NotNil(t, tool.Handler)
	}
}

// A document that does not conform is refused, with the reason, so an upload can
// be rejected while the person who uploaded it is still looking at the screen.
func TestBuild_NonConformantSpecIsRefused(t *testing.T) {
	const broken = `{"openapi":"3.1.0","info":{"title":"B","version":"1"},
      "paths":{"/x":{"get":{"operationId":"x",
        "responses":{"200":{"description":"OK"}},
        "parameters":[{"name":"q","in":"query","schema":{"types":["string"]}}]}}}}`

	_, err := automcp.Build(automcp.Options{Spec: strings.NewReader(broken), BaseURL: "http://x"})

	require.Error(t, err)
	assert.Contains(t, err.Error(), "conform")
}

func TestBuild_Validation(t *testing.T) {
	t.Run("no spec", func(t *testing.T) {
		_, err := automcp.Build(automcp.Options{BaseURL: "http://x"})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "spec")
	})

	t.Run("no base url", func(t *testing.T) {
		_, err := automcp.Build(automcp.Options{Spec: strings.NewReader(petSpec)})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "base URL")
	})

	t.Run("unparseable spec", func(t *testing.T) {
		_, err := automcp.Build(automcp.Options{Spec: strings.NewReader("{not json"), BaseURL: "http://x"})
		require.Error(t, err)
	})
}

// The timeout is the host's to set, since it is the host that knows what its
// callers will tolerate.
func TestBuild_TimeoutIsHonoured(t *testing.T) {
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{}`))
	}))
	t.Cleanup(slow.Close)

	service, err := automcp.Build(automcp.Options{
		Spec:    strings.NewReader(petSpec),
		BaseURL: slow.URL,
		Timeout: 50 * time.Millisecond,
	})
	require.NoError(t, err)

	_, err = session(t, service).CallTool(t.Context(), &mcp.CallToolParams{
		Name: "getPetById", Arguments: map[string]any{"petId": "7"}})
	require.Error(t, err, "a request beyond the timeout fails rather than hanging")
}

// Two services built from different documents stay separate, which is what lets
// one host serve many upstreams.
func TestBuild_ServicesAreIndependent(t *testing.T) {
	first, firstSeen := upstream(t)
	second, secondSeen := upstream(t)

	a, err := automcp.Build(automcp.Options{Spec: strings.NewReader(petSpec), BaseURL: first.URL})
	require.NoError(t, err)
	b, err := automcp.Build(automcp.Options{Spec: strings.NewReader(petSpec), BaseURL: second.URL})
	require.NoError(t, err)

	_, err = session(t, a).CallTool(t.Context(), &mcp.CallToolParams{
		Name: "getPetById", Arguments: map[string]any{"petId": "1"}})
	require.NoError(t, err)

	assert.Len(t, *firstSeen, 1)
	assert.Empty(t, *secondSeen, "one service's calls do not reach the other's upstream")
	_ = b

	// Addressed by name rather than by position: the order is stable but it is
	// not part of the contract.
	var byPath *automcp.Tool
	for _, tool := range a.Tools() {
		if tool.Tool.Name == "getPetById" {
			byPath = &tool
			break
		}
	}
	require.NotNil(t, byPath)
	encoded, err := json.Marshal(byPath.Tool.InputSchema)
	require.NoError(t, err)
	assert.Contains(t, string(encoded), "petId")
}
