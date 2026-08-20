package tool

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/brizzai/auto-mcp/internal/requester"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func staticExecutor(status int, body string, captured *map[string]any) requester.RouteExecutor {
	return func(_ context.Context, params map[string]any) (*requester.Response, error) {
		if captured != nil {
			*captured = params
		}
		return &requester.Response{StatusCode: status, Body: []byte(body)}, nil
	}
}

func call(t *testing.T, tool *mcp.Tool, exec requester.RouteExecutor, args any) *mcp.CallToolResult {
	t.Helper()
	handler := NewHandler(false).CreateHandler(tool, exec)

	params := &mcp.CallToolParamsRaw{Name: tool.Name}
	if args != nil {
		encoded, err := json.Marshal(args)
		require.NoError(t, err)
		params.Arguments = encoded
	}

	result, err := handler(context.Background(), &mcp.CallToolRequest{Params: params})
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

func textOf(t *testing.T, result *mcp.CallToolResult) string {
	t.Helper()
	require.Len(t, result.Content, 1)
	text, ok := result.Content[0].(*mcp.TextContent)
	require.True(t, ok, "expected text content, got %T", result.Content[0])
	return text.Text
}

func toolWithOutput() *mcp.Tool {
	return &mcp.Tool{
		Name:         "queryHotelInfo",
		InputSchema:  map[string]any{"type": "object", "properties": map[string]any{}},
		OutputSchema: map[string]any{"type": "object"},
	}
}

func toolWithoutOutput() *mcp.Tool {
	return &mcp.Tool{
		Name:        "queryHotelInfo",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}
}

// A tool that advertises an outputSchema must return structured content, or it
// is telling the client to expect something that never arrives.
func TestHandler_StructuredContentAccompaniesOutputSchema(t *testing.T) {
	body := `{"returnCode":"000","bussinessResponse":{"hotelName":"Grand"}}`

	result := call(t, toolWithOutput(), staticExecutor(http.StatusOK, body, nil), map[string]any{})

	assert.False(t, result.IsError)
	assert.JSONEq(t, body, textOf(t, result), "the raw response is still returned as text")

	structured, ok := result.StructuredContent.(map[string]any)
	require.True(t, ok, "structured content must be present and be an object")
	assert.Equal(t, "000", structured["returnCode"])
}

// Without a declared outputSchema there is no contract to satisfy, so returning
// structured content would publish a shape the tool never promised.
func TestHandler_NoOutputSchemaMeansNoStructuredContent(t *testing.T) {
	result := call(t, toolWithoutOutput(), staticExecutor(http.StatusOK, `{"a":1}`, nil), map[string]any{})

	assert.Nil(t, result.StructuredContent)
	assert.Equal(t, `{"a":1}`, textOf(t, result))
}

// An upstream that contradicts its own spec must not break the call: the text
// still carries the raw response, which is what a human debugging this needs.
func TestHandler_NonJSONResponseFallsBackToTextOnly(t *testing.T) {
	result := call(t, toolWithOutput(), staticExecutor(http.StatusOK, "not json at all", nil), map[string]any{})

	assert.Nil(t, result.StructuredContent)
	assert.Equal(t, "not json at all", textOf(t, result))
	assert.False(t, result.IsError, "a spec mismatch upstream is not a protocol error")
}

// The declared schema says object, so a JSON array would violate the published
// contract even though it parses.
func TestHandler_NonObjectJSONIsNotPublishedAsStructured(t *testing.T) {
	result := call(t, toolWithOutput(), staticExecutor(http.StatusOK, `[1,2,3]`, nil), map[string]any{})

	assert.Nil(t, result.StructuredContent)
	assert.Equal(t, `[1,2,3]`, textOf(t, result))
}

// Arguments arrive as raw bytes because Server.AddTool opts out of the SDK's own
// validation; they have to reach the executor decoded.
func TestHandler_ArgumentsAreDecodedForTheExecutor(t *testing.T) {
	var captured map[string]any
	args := map[string]any{"body": map[string]any{"header": map[string]any{"sign": "SIG"}}}

	call(t, toolWithoutOutput(), staticExecutor(http.StatusOK, `{}`, &captured), args)

	body, ok := captured["body"].(map[string]any)
	require.True(t, ok, "body argument must reach the executor as a map")
	header, ok := body["header"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, "SIG", header["sign"])
}

// A tool whose parameters are all optional is called with no arguments at all.
func TestHandler_MissingArgumentsAreNotAnError(t *testing.T) {
	var captured map[string]any

	result := call(t, toolWithoutOutput(), staticExecutor(http.StatusOK, `{}`, &captured), nil)

	assert.False(t, result.IsError)
	assert.NotNil(t, captured, "the executor still receives a params map")
	assert.Empty(t, captured)
}

// Malformed arguments are reported to the caller rather than reaching upstream.
func TestHandler_MalformedArgumentsAreRejectedBeforeCalling(t *testing.T) {
	called := false
	exec := func(_ context.Context, _ map[string]any) (*requester.Response, error) {
		called = true
		return &requester.Response{StatusCode: http.StatusOK}, nil
	}
	handler := NewHandler(false).CreateHandler(toolWithoutOutput(), exec)

	result, err := handler(context.Background(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "queryHotelInfo", Arguments: json.RawMessage(`{"broken":`)},
	})

	require.NoError(t, err)
	assert.True(t, result.IsError)
	assert.False(t, called, "a request that cannot be decoded must not reach the upstream")
}

// An upstream error status is surfaced as a tool error, with the body kept.
func TestHandler_UpstreamErrorStatusBecomesToolError(t *testing.T) {
	result := call(t, toolWithOutput(),
		staticExecutor(http.StatusBadGateway, `{"msg":"upstream down"}`, nil), map[string]any{})

	assert.True(t, result.IsError)
	assert.Contains(t, textOf(t, result), "502")
	assert.Contains(t, textOf(t, result), "upstream down")
	assert.Nil(t, result.StructuredContent)
}
