package models

type RouteFieldUpdate struct {
	Method         string `yaml:"method"`
	NewDescription string `yaml:"new_description"`
}

type RouteDescription struct {
	Path    string             `yaml:"path"`
	Updates []RouteFieldUpdate `yaml:"updates"`
}

type RouteSelection struct {
	Path    string   `yaml:"path"`
	Methods []string `yaml:"methods"`
}

// ResponseTemplate reshapes an upstream response before it reaches the caller.
//
// A whole upstream response is usually much larger and noisier than the part a
// caller needs — pagination metadata, internal trace ids, dozens of null fields
// — and all of it lands in a model's context. Trimming it is a configuration
// concern rather than a code one, so it lives with the other per-route human
// edits.
type ResponseTemplate struct {
	// Body is a Go template evaluated against the parsed JSON response.
	Body string `yaml:"body,omitempty"`
	// PrependBody and AppendBody wrap the result, with or without a Body template.
	PrependBody string `yaml:"prepend_body,omitempty"`
	AppendBody  string `yaml:"append_body,omitempty"`
	// ErrorBody is used instead of Body when the upstream reports a failure. The
	// fields that explain a failure are rarely the fields that carry a result.
	ErrorBody string `yaml:"error_body,omitempty"`
}

// Configured reports whether the template does anything.
func (r *ResponseTemplate) Configured() bool {
	return r != nil && (r.Body != "" || r.PrependBody != "" || r.AppendBody != "" || r.ErrorBody != "")
}

// RouteResponseUpdate binds a response template to one method of a path.
type RouteResponseUpdate struct {
	Method   string           `yaml:"method"`
	Response ResponseTemplate `yaml:",inline"`
}

// RouteResponse groups the response templates declared for one path.
type RouteResponse struct {
	Path    string                `yaml:"path"`
	Updates []RouteResponseUpdate `yaml:"updates"`
}

type MCPAdjustments struct {
	Descriptions []RouteDescription `yaml:"descriptions,omitempty"`
	Routes       []RouteSelection   `yaml:"routes,omitempty"`
	Responses    []RouteResponse    `yaml:"responses,omitempty"`
}
