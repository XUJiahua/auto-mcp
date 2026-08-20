package config

import (
	"fmt"
	"regexp"
)

// mcpRoutePrefix is where the MCP endpoints live when several services share a
// process.
const mcpRoutePrefix = "/mcp"

// serviceNamePattern keeps a name usable as one URL path segment. The name is
// part of the address callers use, so it cannot contain a slash, and giving it
// the same character set as a hostname label keeps it copy-pasteable.
var serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// ServiceConfig is one upstream API exposed as its own MCP endpoint.
//
// Everything that differs between two upstreams lives here: the document, the
// human edits applied to it, the address, and the credential used to reach it.
// Security schemes stay global, because a scheme describes how a credential is
// carried rather than which upstream it belongs to.
type ServiceConfig struct {
	// Name is the route segment. It is empty for the single-service form, whose
	// endpoint stays at the root so its address does not depend on a name the
	// operator never chose.
	Name           string `mapstructure:"name" json:"name,omitempty"`
	SwaggerFile    string `mapstructure:"swagger_file" json:"swagger_file"`
	AdjustmentFile string `mapstructure:"adjustment_file" json:"adjustment_file,omitempty"`

	Endpoint         EndpointConfig       `mapstructure:"endpoint" json:"endpoint"`
	UpstreamSecurity *SecurityRequirement `mapstructure:"upstream_security" json:"upstream_security,omitempty"`
}

// RoutePath is the HTTP path this service is served on.
func (s ServiceConfig) RoutePath() string {
	if s.Name == "" {
		return mcpRoutePrefix
	}
	return mcpRoutePrefix + "/" + s.Name
}

// ResolvedServices returns the effective service list.
//
// A top-level swagger_file is the single-service form, kept because the common
// case is one API and because stdio has no path to route on. It is expressed as
// a one-element list so that everything downstream has only one shape to handle.
func (c *Config) ResolvedServices() []ServiceConfig {
	if len(c.Services) > 0 {
		return c.Services
	}
	if c.SwaggerFile == "" {
		return nil
	}
	return []ServiceConfig{{
		SwaggerFile:      c.SwaggerFile,
		AdjustmentFile:   c.AdjustmentFile,
		Endpoint:         c.EndpointConfig,
		UpstreamSecurity: c.UpstreamSecurity,
	}}
}

// validateServices checks the service list and resolves its credentials.
func (c *Config) validateServices() error {
	services := c.ResolvedServices()
	if len(services) == 0 {
		return fmt.Errorf("swagger file is required, please adjust the config or pass " +
			"--swagger-file or AUTO_MCP_SWAGGER_FILE environment variable")
	}

	seen := make(map[string]bool, len(services))
	for i := range c.Services {
		service := &c.Services[i]
		if service.SwaggerFile == "" {
			return fmt.Errorf("service %q has no swagger_file", service.Name)
		}
		if len(c.Services) > 1 && service.Name == "" {
			return fmt.Errorf("every service must be named when more than one is configured; " +
				"the name is the route segment callers address")
		}
		if service.Name != "" && !serviceNamePattern.MatchString(service.Name) {
			return fmt.Errorf("service name %q must be a single path segment matching %s",
				service.Name, serviceNamePattern)
		}
		if seen[service.Name] {
			return fmt.Errorf("duplicate service name %q", service.Name)
		}
		seen[service.Name] = true

		if service.UpstreamSecurity != nil {
			where := fmt.Sprintf("service %q upstream_security", service.Name)
			if err := c.resolveRequirement(where, service.UpstreamSecurity); err != nil {
				return err
			}
		}
	}

	// Checked last, so that a structurally broken list is reported as such rather
	// than as a transport mismatch. stdio speaks to one client over one pipe:
	// there is no address to tell services apart by, so serving several is a
	// configuration mistake rather than something to resolve arbitrarily.
	if c.Server.Mode == ServerModeSTDIO && len(services) > 1 {
		return fmt.Errorf("stdio serves a single service, but %d are configured; "+
			"use http or sse to expose several", len(services))
	}
	return nil
}
