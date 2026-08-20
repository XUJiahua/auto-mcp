package requester

import (
	"net/http"
)

// RouteConfig holds the configuration for a specific route
type RouteConfig struct {
	Path        string            `json:"path"`
	Method      string            `json:"method"`
	Description string            `json:"description,omitempty"`
	Headers     map[string]string `json:"headers"`
	Parameters  map[string]string `json:"parameters"`
	// Method specific configurations
	MethodConfig MethodConfig `json:"method_config"`
}

// ParamLocation is where a tool argument belongs on the wire.
type ParamLocation string

const (
	ParamInPath   ParamLocation = "path"
	ParamInQuery  ParamLocation = "query"
	ParamInHeader ParamLocation = "header"
	ParamInCookie ParamLocation = "cookie"
)

// ParamConfig tells the request builder where one tool argument goes and how to
// serialise it.
//
// Without the location, the builder has to guess from the argument name, which
// is why path parameters used to be appended to the query string as well and
// header parameters were never sent at all.
type ParamConfig struct {
	// Name is the parameter name the upstream expects.
	Name string `json:"name"`
	// ArgName is the tool argument this parameter reads from. It differs from
	// Name only when two parameters would have collided in the tool's flat
	// argument namespace and one had to be renamed.
	ArgName string        `json:"arg_name,omitempty"`
	In      ParamLocation `json:"in"`
	// Type is the JSON Schema type, needed because an array has to be expanded
	// into repeated query keys rather than formatted as a Go value.
	Type string `json:"type,omitempty"`
	// Explode follows OpenAPI serialisation: repeated keys when true (the
	// default for query parameters), comma-joined when false.
	Explode bool `json:"explode"`
}

// Arg returns the tool argument name this parameter reads from.
func (p ParamConfig) Arg() string {
	if p.ArgName != "" {
		return p.ArgName
	}
	return p.Name
}

// MethodConfig holds method-specific configurations
type MethodConfig struct {
	// Params carries every declared parameter with its location.
	Params []ParamConfig `json:"params,omitempty"`

	// BodyContentType is the media type the spec declared for the request body.
	// The builder encodes the body to match it, so that the bytes on the wire and
	// the Content-Type header cannot disagree.
	BodyContentType string `json:"body_content_type,omitempty"`

	// For multipart/form-data
	FormFields []string `json:"form_fields,omitempty"`

	// For file uploads
	FileUpload *FileUploadConfig `json:"file_upload,omitempty"`
}

// FileUploadConfig holds configuration for file uploads
type FileUploadConfig struct {
	FieldName    string   `json:"field_name"`
	AllowedTypes []string `json:"allowed_types"`
	MaxSize      int64    `json:"max_size"`
}

// RequestResult holds the result of a request
type RequestResult struct {
	StatusCode int
	Body       []byte
	Headers    http.Header
	Error      error
}
