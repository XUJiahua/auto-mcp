// Package tool provides tool handling functionality for the MCP server.
package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/brizzai/auto-mcp/internal/auth/middleware"
	"github.com/brizzai/auto-mcp/internal/logger"
	"github.com/brizzai/auto-mcp/internal/models"
	"github.com/brizzai/auto-mcp/internal/requester"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"go.uber.org/zap"
)

// Handler manages tool execution and authentication.
type Handler struct {
	auth *bool // nil if auth is disabled, non-nil if enabled
}

// NewHandler creates a new tool handler.
func NewHandler(authEnabled bool) *Handler {
	if authEnabled {
		enabled := true
		return &Handler{auth: &enabled}
	}
	return &Handler{auth: nil}
}

// CreateHandler creates a handler function for a specific tool.
// It handles authentication validation and request execution.
func (h *Handler) CreateHandler(tool *mcp.Tool, response *models.ResponseTemplate, executor requester.RouteExecutor) mcp.ToolHandler {
	shaper := newResponseShaper(tool.Name, response)
	return func(ctx context.Context, request *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		// Validate authentication if enabled
		if h.auth != nil {
			authInfo, ok := ctx.Value(middleware.AuthContextKey).(*middleware.AuthInfo)
			if !ok {
				logger.Error("Failed to get auth info from context",
					zap.String("tool", tool.Name),
					zap.Any("context_keys", ctx.Value(middleware.AuthContextKey)),
				)
				return errorResult("Unauthorized: No active user info in context"), nil
			}
			logger.Debug("Authenticated tool call",
				zap.String("tool", tool.Name),
				zap.String("user", authInfo.UserID),
			)
		}

		params, err := decodeArguments(request)
		if err != nil {
			return errorResult(fmt.Sprintf("Invalid arguments for tool %s: %v", tool.Name, err)), nil
		}

		resp, err := executor(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("failed to execute request for tool %s: %w", tool.Name, err)
		}

		// Handle error responses
		if resp.StatusCode >= http.StatusBadRequest {
			if shaped, ok := shaper.shapeError(resp.Body); ok {
				return errorResult(shaped), nil
			}
			return errorResult(fmt.Sprintf("HTTP Error %d: %s", resp.StatusCode, string(resp.Body))), nil
		}

		if shaped, ok := shaper.shape(resp.Body); ok {
			// A template states what the caller should see. Sending the untrimmed
			// payload as structured content alongside it would put back exactly
			// what the operator removed.
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: shaped}}}, nil
		}
		return successResult(tool, resp.Body), nil
	}
}

// decodeArguments unmarshals the raw tool arguments.
//
// The SDK hands over the bytes as received rather than a decoded map, because a
// handler registered through Server.AddTool opts out of the SDK's own schema
// validation. Absent arguments are a valid call, not an error: a tool whose
// every parameter is optional takes none.
func decodeArguments(request *mcp.CallToolRequest) (map[string]any, error) {
	if request == nil || request.Params == nil || len(request.Params.Arguments) == 0 {
		return map[string]any{}, nil
	}
	var params map[string]any
	if err := json.Unmarshal(request.Params.Arguments, &params); err != nil {
		return nil, err
	}
	if params == nil {
		params = map[string]any{}
	}
	return params, nil
}

// successResult returns the upstream response as text, plus structured content
// when the tool declares an output schema.
//
// The pairing is required rather than a nicety: a tool that advertises an
// outputSchema and then returns only text is telling the client to expect
// structured content that never arrives, and a client that validates the
// contract will reject the call. The text is always included as well, so
// clients that ignore structured content are unaffected.
func successResult(tool *mcp.Tool, body []byte) *mcp.CallToolResult {
	result := &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(body)}},
	}
	if tool.OutputSchema == nil || len(body) == 0 {
		return result
	}
	var structured any
	if err := json.Unmarshal(body, &structured); err != nil {
		// The upstream contradicted its own spec. The text content still carries
		// the raw response, which is what a human debugging this needs to see.
		logger.Debug("Upstream response is not JSON; omitting structured content",
			zap.String("tool", tool.Name))
		return result
	}
	// The declared schema is an object (see parser.responseSchema), so anything
	// else would violate the contract we published.
	if _, ok := structured.(map[string]any); !ok {
		logger.Debug("Upstream response is not a JSON object; omitting structured content",
			zap.String("tool", tool.Name))
		return result
	}
	result.StructuredContent = structured
	return result
}

func errorResult(message string) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: message}},
	}
}
