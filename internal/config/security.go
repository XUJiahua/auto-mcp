package config

import (
	"fmt"
	"os"
	"regexp"
	"strings"
)

// Security scheme types, following OpenAPI's own vocabulary.
const (
	SchemeTypeAPIKey = "apiKey"
	SchemeTypeHTTP   = "http"
)

// HTTP authentication schemes this server can apply and verify.
const (
	HTTPSchemeBasic  = "basic"
	HTTPSchemeBearer = "bearer"
)

// Locations an API key can occupy.
const (
	SchemeInHeader = "header"
	SchemeInQuery  = "query"
)

// SecurityScheme describes how a credential is carried, without saying who uses
// it. The same scheme can serve the client-to-gateway and gateway-to-upstream
// directions, which is why the two are separate requirements referring to it.
type SecurityScheme struct {
	ID     string `mapstructure:"id" json:"id"`
	Type   string `mapstructure:"type" json:"type"`
	Scheme string `mapstructure:"scheme" json:"scheme,omitempty"`
	In     string `mapstructure:"in" json:"in,omitempty"`
	Name   string `mapstructure:"name" json:"name,omitempty"`
	// DefaultCredential is used by any requirement that does not carry its own.
	DefaultCredential string `mapstructure:"default_credential" json:"default_credential,omitempty"`
}

// SecurityRequirement selects a scheme and supplies the credential to use with
// it.
type SecurityRequirement struct {
	ID         string `mapstructure:"id" json:"id"`
	Credential string `mapstructure:"credential" json:"credential,omitempty"`
	// Passthrough forwards the caller's own credential upstream instead of using
	// one of ours. It is only meaningful in the upstream direction.
	Passthrough bool `mapstructure:"passthrough" json:"passthrough,omitempty"`
}

// SecurityScheme looks up a scheme by id.
func (c *Config) SecurityScheme(id string) (SecurityScheme, bool) {
	for _, scheme := range c.SecuritySchemes {
		if scheme.ID == id {
			return scheme, true
		}
	}
	return SecurityScheme{}, false
}

// loopbackHosts are the bind addresses that are not reachable from off the
// machine. An empty host means the default, which is loopback.
var loopbackHosts = map[string]bool{
	"":          true,
	"localhost": true,
	"127.0.0.1": true,
	"::1":       true,
	"[::1]":     true,
}

// BindsPublicly reports whether the configured host is reachable from outside
// this machine.
func (s ServerConfig) BindsPublicly() bool {
	return !loopbackHosts[strings.ToLower(strings.TrimSpace(s.Host))]
}

// listens reports whether the mode opens a socket at all. stdio does not, so no
// bind address applies to it.
func (s ServerConfig) listens() bool {
	return s.Mode == ServerModeHTTP || s.Mode == ServerModeSSE
}

// envRefPattern matches a ${VAR} reference. Only the braced form is recognised,
// so a credential that legitimately contains a dollar sign is left alone.
var envRefPattern = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

// interpolateCredential replaces ${VAR} references with environment values.
//
// An unset variable is an error rather than an empty string. An empty credential
// reaches the upstream as a missing or blank one, and the rejection that follows
// reads as a permissions problem — a long way from a typo in a variable name.
func interpolateCredential(value, where string) (string, error) {
	var missing []string
	out := envRefPattern.ReplaceAllStringFunc(value, func(match string) string {
		name := envRefPattern.FindStringSubmatch(match)[1]
		resolved, ok := os.LookupEnv(name)
		if !ok {
			missing = append(missing, name)
			return ""
		}
		return resolved
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("%s references unset environment variable(s): %s",
			where, strings.Join(missing, ", "))
	}
	return out, nil
}

// resolveSecurity interpolates every credential and checks the configuration is
// internally consistent.
func (c *Config) resolveSecurity() error {
	seen := make(map[string]bool, len(c.SecuritySchemes))
	for i := range c.SecuritySchemes {
		scheme := &c.SecuritySchemes[i]
		if scheme.ID == "" {
			return fmt.Errorf("security scheme %d has no id", i)
		}
		if seen[scheme.ID] {
			return fmt.Errorf("duplicate security scheme id %q", scheme.ID)
		}
		seen[scheme.ID] = true

		if err := validateScheme(*scheme); err != nil {
			return err
		}

		resolved, err := interpolateCredential(scheme.DefaultCredential,
			fmt.Sprintf("security scheme %q default_credential", scheme.ID))
		if err != nil {
			return err
		}
		scheme.DefaultCredential = resolved
	}

	for _, requirement := range []struct {
		name string
		ref  *SecurityRequirement
	}{
		{"downstream_security", c.DownstreamSecurity},
		{"upstream_security", c.UpstreamSecurity},
	} {
		if requirement.ref == nil {
			continue
		}
		if err := c.resolveRequirement(requirement.name, requirement.ref); err != nil {
			return err
		}
	}

	if c.DownstreamSecurity != nil && c.DownstreamSecurity.Passthrough {
		return fmt.Errorf("downstream_security cannot use passthrough: " +
			"there is nothing further upstream to pass the caller's credential to")
	}

	return c.validateExposure()
}

func (c *Config) resolveRequirement(name string, requirement *SecurityRequirement) error {
	if requirement.ID == "" {
		return fmt.Errorf("%s has no id", name)
	}
	scheme, ok := c.SecurityScheme(requirement.ID)
	if !ok {
		return fmt.Errorf("%s refers to unknown security scheme %q", name, requirement.ID)
	}

	resolved, err := interpolateCredential(requirement.Credential, name+" credential")
	if err != nil {
		return err
	}
	requirement.Credential = resolved

	if requirement.Passthrough {
		return nil
	}
	if requirement.Credential == "" && scheme.DefaultCredential == "" {
		return fmt.Errorf("%s has no credential: set its credential, "+
			"give security scheme %q a default_credential, or use passthrough",
			name, scheme.ID)
	}
	return nil
}

// validateExposure enforces that an endpoint reachable from off the machine can
// tell its callers apart.
//
// The MCP endpoint holds whatever credential the upstream requires, so an
// unauthenticated public bind lends those credentials to anyone who can reach
// the port. Loopback stays open because it is not reachable, and stdio has no
// socket at all.
func (c *Config) validateExposure() error {
	if !c.Server.listens() || !c.Server.BindsPublicly() {
		return nil
	}
	if c.DownstreamSecurity != nil {
		return nil
	}
	if c.OAuth != nil && c.OAuth.Enabled {
		return nil
	}
	return fmt.Errorf(
		"server.host %q is reachable from outside this machine but nothing authenticates callers; "+
			"configure downstream_security, enable oauth, or bind to localhost",
		c.Server.Host)
}

func validateScheme(scheme SecurityScheme) error {
	switch scheme.Type {
	case SchemeTypeAPIKey:
		if scheme.Name == "" {
			return fmt.Errorf("security scheme %q of type apiKey requires a name", scheme.ID)
		}
		if scheme.In != SchemeInHeader && scheme.In != SchemeInQuery {
			return fmt.Errorf("security scheme %q of type apiKey: in must be header or query, got %q",
				scheme.ID, scheme.In)
		}
	case SchemeTypeHTTP:
		if scheme.Scheme != HTTPSchemeBasic && scheme.Scheme != HTTPSchemeBearer {
			return fmt.Errorf("security scheme %q of type http: scheme must be basic or bearer, got %q",
				scheme.ID, scheme.Scheme)
		}
	default:
		return fmt.Errorf("security scheme %q: unsupported security scheme type %q (expected apiKey or http)",
			scheme.ID, scheme.Type)
	}
	return nil
}
