package tool

import (
	"bytes"
	"encoding/json"
	"strings"
	"text/template"

	"github.com/brizzai/auto-mcp/internal/logger"
	"github.com/brizzai/auto-mcp/internal/models"
	"go.uber.org/zap"
)

// responseShaper applies a configured response template.
//
// Templates are parsed once, when the tool is registered, so a malformed one is
// reported at startup rather than on every call. A template that cannot be
// applied — bad syntax, a non-JSON response, a reference to a field that is not
// there — falls back to the untouched response body. The template is the
// operator's convenience; discarding the response would discard the only
// evidence of what the upstream actually said.
type responseShaper struct {
	toolName string
	prepend  string
	append   string
	body     *template.Template
	errBody  *template.Template
}

func newResponseShaper(toolName string, config *models.ResponseTemplate) *responseShaper {
	if !config.Configured() {
		return nil
	}
	shaper := &responseShaper{
		toolName: toolName,
		prepend:  config.PrependBody,
		append:   config.AppendBody,
		body:     parseTemplate(toolName, "body", config.Body),
		errBody:  parseTemplate(toolName, "error_body", config.ErrorBody),
	}
	return shaper
}

func parseTemplate(toolName, field, text string) *template.Template {
	if text == "" {
		return nil
	}
	parsed, err := template.New(field).Option("missingkey=error").Parse(text)
	if err != nil {
		logger.Error("Ignoring malformed response template",
			zap.String("tool", toolName), zap.String("field", field), zap.Error(err))
		return nil
	}
	return parsed
}

// shape renders the success view. The second result reports whether the template
// produced anything; false means the caller should fall back.
func (s *responseShaper) shape(body []byte) (string, bool) {
	if s == nil {
		return "", false
	}
	rendered, ok := s.render(s.body, body)
	if !ok {
		return "", false
	}
	return s.prepend + rendered + s.append, true
}

// shapeError renders the failure view. Prepend and append are not applied: they
// describe a result, and this is not one.
func (s *responseShaper) shapeError(body []byte) (string, bool) {
	if s == nil || s.errBody == nil {
		return "", false
	}
	return s.render(s.errBody, body)
}

// render evaluates a template against the parsed response. A nil template means
// "no transformation", which still allows prepend/append to wrap the raw body.
func (s *responseShaper) render(tmpl *template.Template, body []byte) (string, bool) {
	if tmpl == nil {
		if s.prepend == "" && s.append == "" {
			return "", false
		}
		return string(body), true
	}

	var data any
	if err := json.Unmarshal(body, &data); err != nil {
		logger.Debug("Response is not JSON; leaving it untemplated",
			zap.String("tool", s.toolName))
		return "", false
	}

	var out bytes.Buffer
	if err := tmpl.Execute(&out, data); err != nil {
		// Usually a field the response does not carry. Blanking the result would
		// hide both the response and the mistake.
		logger.Warn("Response template could not be applied; returning the response unchanged",
			zap.String("tool", s.toolName), zap.Error(err))
		return "", false
	}
	return strings.TrimSpace(out.String()), true
}
