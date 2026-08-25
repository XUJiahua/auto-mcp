package requester

import "context"

// BuildRequest is what a URL or body builder sees.
//
// It carries both the operation as the document declares it and what the builder
// computed by default. A hook needs both: the template says which operation this
// is, the default says what would otherwise have been sent — and most hooks only
// want to adjust that default rather than construct one from nothing.
type BuildRequest struct {
	Method string
	// Path is the path template exactly as the document declares it,
	// e.g. /api/{lang}/nationality/list.
	Path string
	// URL is the fully built URL, path parameters substituted and query appended.
	URL string
	// Args are the caller's arguments for this tool.
	Args map[string]interface{}
	// Body and ContentType are what the default body builder produced.
	Body        []byte
	ContentType string
}

// URLBuilder computes the URL for a request.
//
// Returning an error stops the request. Sending a half-built one would only earn
// a rejection whose message cannot say which part was wrong.
type URLBuilder interface {
	BuildURL(ctx context.Context, req BuildRequest) (string, error)
}

// BodyBuilder computes the request body.
//
// It runs before the signer, so a signature is computed over the bytes this
// returns. The other order would sign bytes that were never sent.
type BodyBuilder interface {
	BuildBody(ctx context.Context, req BuildRequest) ([]byte, string, error)
}
