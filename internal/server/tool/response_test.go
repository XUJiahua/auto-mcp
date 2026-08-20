package tool

import (
	"net/http"
	"testing"

	"github.com/brizzai/auto-mcp/internal/models"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const upstreamBody = `{"returnCode":"000","returnMsg":"ok",
  "bussinessResponse":{"hotelName":"Grand","starRate":5,
    "internalTraceId":"abc-123","emptyField":null}}`

func callTemplated(t *testing.T, tmpl *models.ResponseTemplate, status int, body string) *mcp.CallToolResult {
	t.Helper()
	handler := NewHandler(false).CreateHandler(toolWithOutput(), tmpl, staticExecutor(status, body, nil))
	result, err := handler(t.Context(), &mcp.CallToolRequest{
		Params: &mcp.CallToolParamsRaw{Name: "queryHotelInfo"},
	})
	require.NoError(t, err)
	require.NotNil(t, result)
	return result
}

// An upstream response is often far larger and noisier than what the caller
// needs, and all of it lands in a model's context. A template trims it.
func TestResponse_TemplateShapesTheResult(t *testing.T) {
	result := callTemplated(t, &models.ResponseTemplate{
		Body: "{{ .bussinessResponse.hotelName }} ({{ .bussinessResponse.starRate }} stars)",
	}, http.StatusOK, upstreamBody)

	assert.Equal(t, "Grand (5 stars)", textOf(t, result))
	assert.NotContains(t, textOf(t, result), "internalTraceId")
}

func TestResponse_PrependAndAppendWrapTheBody(t *testing.T) {
	result := callTemplated(t, &models.ResponseTemplate{
		Body:        "{{ .bussinessResponse.hotelName }}",
		PrependBody: "Hotel: ",
		AppendBody:  " (from cache)",
	}, http.StatusOK, upstreamBody)

	assert.Equal(t, "Hotel: Grand (from cache)", textOf(t, result))
}

// Prepend and append work without a body template, for annotating a raw response.
func TestResponse_WrappingWithoutABodyTemplateKeepsTheResponse(t *testing.T) {
	result := callTemplated(t, &models.ResponseTemplate{PrependBody: "raw: "},
		http.StatusOK, `{"a":1}`)

	assert.Equal(t, `raw: {"a":1}`, textOf(t, result))
}

// A template states what the caller should see, so the untrimmed payload is not
// also sent as structured content: that would put back exactly what was removed.
func TestResponse_TemplatedResultCarriesNoStructuredContent(t *testing.T) {
	result := callTemplated(t, &models.ResponseTemplate{Body: "{{ .returnCode }}"},
		http.StatusOK, upstreamBody)

	assert.Nil(t, result.StructuredContent)
}

// Without a template the structured content is unaffected.
func TestResponse_NoTemplateKeepsStructuredContent(t *testing.T) {
	result := callTemplated(t, nil, http.StatusOK, upstreamBody)

	assert.NotNil(t, result.StructuredContent)
}

// An error response gets its own template, because the fields that explain a
// failure are not the fields that carry a result.
func TestResponse_ErrorTemplateShapesFailures(t *testing.T) {
	result := callTemplated(t, &models.ResponseTemplate{
		Body:      "{{ .returnCode }}",
		ErrorBody: "upstream refused: {{ .returnMsg }}",
	}, http.StatusBadGateway, `{"returnCode":"019","returnMsg":"signature invalid"}`)

	assert.True(t, result.IsError)
	assert.Equal(t, "upstream refused: signature invalid", textOf(t, result))
}

// A failure with no error template still reports the status and the body.
func TestResponse_ErrorWithoutATemplateIsUnchanged(t *testing.T) {
	result := callTemplated(t, &models.ResponseTemplate{Body: "{{ .returnCode }}"},
		http.StatusBadGateway, `{"returnMsg":"nope"}`)

	assert.True(t, result.IsError)
	assert.Contains(t, textOf(t, result), "502")
	assert.Contains(t, textOf(t, result), "nope")
}

// A response that is not JSON cannot be templated. The raw body is returned
// rather than an error: the template is the operator's convenience, and losing
// the response would lose the only evidence of what the upstream actually said.
func TestResponse_NonJSONResponseFallsBackToTheRawBody(t *testing.T) {
	result := callTemplated(t, &models.ResponseTemplate{Body: "{{ .returnCode }}"},
		http.StatusOK, "not json")

	assert.Equal(t, "not json", textOf(t, result))
	assert.False(t, result.IsError)
}

// A template referring to a field the response does not have must not blank the
// result silently.
func TestResponse_TemplateReferringToAMissingFieldKeepsTheRawBody(t *testing.T) {
	result := callTemplated(t, &models.ResponseTemplate{Body: "{{ .nope.deeper }}"},
		http.StatusOK, `{"a":1}`)

	assert.Equal(t, `{"a":1}`, textOf(t, result))
}
