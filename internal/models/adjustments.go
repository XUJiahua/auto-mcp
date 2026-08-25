package models

type RouteFieldUpdate struct {
	Method string `yaml:"method"`
	// NewName replaces the tool's derived name.
	//
	// The name is not cosmetic: a model reads it, a host usually builds an
	// identifier out of it that clients then hold on to, and anything downstream
	// that classifies operations has only the name to go on when reads and writes
	// are both POST. So whoever curates a document has to be able to settle it
	// here, next to the selection and the descriptions, rather than only in
	// whatever screen happened to show it.
	// omitempty：这个字段是后加的，不带它会让导出的校订文件里每条都多一行
	// new_name: ""，而那份文件是给人读、也是要 diff 的。
	NewName        string `yaml:"new_name,omitempty"`
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
