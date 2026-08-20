// Package security applies and verifies the credentials described by
// configured security schemes.
//
// The same scheme can be used in either direction, so one engine serves both:
// Verify checks the credential an MCP client presents to this server, and Apply
// attaches the credential this server presents to the API it proxies. Keeping
// them together is what makes the two directions describable in the same
// vocabulary, and keeps a scheme from meaning one thing inbound and another
// outbound.
package security

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/brizzai/auto-mcp/internal/config"
)

// ErrUnauthenticated reports that an incoming request did not present the
// credential the requirement calls for. It carries no detail about what was
// expected: an error that explains the difference is an oracle.
var ErrUnauthenticated = errors.New("unauthenticated")

// Engine resolves requirements against the configured schemes.
type Engine struct {
	schemes map[string]config.SecurityScheme
}

// New builds an engine over the given schemes.
func New(schemes []config.SecurityScheme) *Engine {
	byID := make(map[string]config.SecurityScheme, len(schemes))
	for _, scheme := range schemes {
		byID[scheme.ID] = scheme
	}
	return &Engine{schemes: byID}
}

type passthroughKey struct{}

// WithPassthroughCredential carries the caller's own credential so that an
// upstream requirement configured for passthrough can forward it.
func WithPassthroughCredential(ctx context.Context, credential string) context.Context {
	return context.WithValue(ctx, passthroughKey{}, credential)
}

// PassthroughCredential returns the caller's credential, if one was recorded.
func PassthroughCredential(ctx context.Context) string {
	credential, _ := ctx.Value(passthroughKey{}).(string)
	return credential
}

// Apply attaches the outgoing credential described by the requirement.
//
// A nil requirement means this deployment has not configured outbound
// authentication, which is a valid choice for an upstream that needs none.
func (e *Engine) Apply(req *http.Request, requirement *config.SecurityRequirement) error {
	if requirement == nil {
		return nil
	}
	scheme, ok := e.schemes[requirement.ID]
	if !ok {
		return fmt.Errorf("unknown security scheme %q", requirement.ID)
	}

	credential := requirement.Credential
	if requirement.Passthrough {
		// Falling back to our own credential here would send the platform's
		// identity on behalf of a caller who presented none.
		credential = PassthroughCredential(req.Context())
		if credential == "" {
			return fmt.Errorf("security scheme %q is configured for passthrough "+
				"but the caller presented no credential", scheme.ID)
		}
	} else if credential == "" {
		credential = scheme.DefaultCredential
	}
	if credential == "" {
		return fmt.Errorf("security scheme %q has no credential to apply", scheme.ID)
	}

	switch scheme.Type {
	case config.SchemeTypeAPIKey:
		switch scheme.In {
		case config.SchemeInHeader:
			req.Header.Set(scheme.Name, credential)
		case config.SchemeInQuery:
			query := req.URL.Query()
			query.Set(scheme.Name, credential)
			req.URL.RawQuery = query.Encode()
		default:
			return fmt.Errorf("security scheme %q has unusable location %q", scheme.ID, scheme.In)
		}
	case config.SchemeTypeHTTP:
		switch scheme.Scheme {
		case config.HTTPSchemeBearer:
			req.Header.Set("Authorization", "Bearer "+credential)
		case config.HTTPSchemeBasic:
			// A passthrough credential is already an encoded header value; a
			// configured one is written as user:pass for legibility.
			if user, pass, found := strings.Cut(credential, ":"); found {
				req.SetBasicAuth(user, pass)
			} else {
				req.Header.Set("Authorization", "Basic "+credential)
			}
		default:
			return fmt.Errorf("security scheme %q has unsupported http scheme %q", scheme.ID, scheme.Scheme)
		}
	default:
		return fmt.Errorf("security scheme %q has unsupported type %q", scheme.ID, scheme.Type)
	}
	return nil
}

// Verify checks an incoming request against the requirement and returns the
// credential it presented, so that a passthrough upstream requirement can reuse
// it.
//
// A nil requirement accepts everything. That is only reachable when nothing is
// exposed: a publicly bound server without downstream authentication is refused
// at startup.
func (e *Engine) Verify(req *http.Request, requirement *config.SecurityRequirement) (string, error) {
	if requirement == nil {
		return "", nil
	}
	scheme, ok := e.schemes[requirement.ID]
	if !ok {
		return "", fmt.Errorf("unknown security scheme %q", requirement.ID)
	}

	expected := requirement.Credential
	if expected == "" {
		expected = scheme.DefaultCredential
	}
	if expected == "" {
		return "", fmt.Errorf("security scheme %q has no credential to verify against", scheme.ID)
	}

	presented, err := extractCredential(req, scheme)
	if err != nil {
		return "", err
	}
	if subtle.ConstantTimeCompare([]byte(presented), []byte(expected)) != 1 {
		return "", ErrUnauthenticated
	}
	return presented, nil
}

// extractCredential reads the credential a request carries for the scheme,
// stripping whatever framing the scheme prescribes.
func extractCredential(req *http.Request, scheme config.SecurityScheme) (string, error) {
	switch scheme.Type {
	case config.SchemeTypeAPIKey:
		switch scheme.In {
		case config.SchemeInHeader:
			return required(req.Header.Get(scheme.Name))
		case config.SchemeInQuery:
			return required(req.URL.Query().Get(scheme.Name))
		default:
			return "", fmt.Errorf("security scheme %q has unusable location %q", scheme.ID, scheme.In)
		}
	case config.SchemeTypeHTTP:
		header := req.Header.Get("Authorization")
		prefix := "Bearer "
		if scheme.Scheme == config.HTTPSchemeBasic {
			prefix = "Basic "
		}
		// The prefix is part of the scheme, so a value without it is not a
		// credential for this scheme even if the bytes after it would match.
		if len(header) < len(prefix) || !strings.EqualFold(header[:len(prefix)], prefix) {
			return "", ErrUnauthenticated
		}
		value := strings.TrimSpace(header[len(prefix):])
		if scheme.Scheme == config.HTTPSchemeBasic {
			if decoded, err := base64.StdEncoding.DecodeString(value); err == nil {
				value = string(decoded)
			}
		}
		return required(value)
	default:
		return "", fmt.Errorf("security scheme %q has unsupported type %q", scheme.ID, scheme.Type)
	}
}

func required(value string) (string, error) {
	if value == "" {
		return "", ErrUnauthenticated
	}
	return value, nil
}
