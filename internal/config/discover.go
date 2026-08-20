package config

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// specFileNames are the document names looked for inside a service directory, in
// order of preference.
var specFileNames = []string{
	"openapi.yaml", "openapi.yml", "openapi.json",
	"swagger.yaml", "swagger.yml", "swagger.json",
}

// adjustmentFileNames are the optional human-edit files looked for beside the
// document.
var adjustmentFileNames = []string{"adjustment.yaml", "adjustment.yml", "adjustments.yaml", "adjustments.yml"}

// serviceFileNames hold the per-service settings the document cannot express:
// where to send requests, and which credential to use.
var serviceFileNames = []string{"service.yaml", "service.yml"}

// serviceOverrides is the subset of ServiceConfig a per-service file may set.
// Name and swagger_file come from the directory, so they are not settable here —
// a file that could rename its own directory would make the route depend on two
// places at once.
type serviceOverrides struct {
	Endpoint         EndpointConfig       `yaml:"endpoint"`
	UpstreamSecurity *SecurityRequirement `yaml:"upstream_security"`
	AdjustmentFile   string               `yaml:"adjustment_file"`
}

// discoverServices reads a services directory, where each subdirectory is one
// service named after itself.
//
// This is what makes adding an API a matter of dropping in a directory rather
// than editing configuration. The directory name is the route, so it is
// validated as strictly as an explicitly configured name.
func discoverServices(root string) ([]ServiceConfig, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, fmt.Errorf("services_dir %q: %w", root, err)
	}

	var discovered []ServiceConfig
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		name := entry.Name()
		dir := filepath.Join(root, name)

		if !serviceNamePattern.MatchString(name) {
			return nil, fmt.Errorf("services_dir %q: service name %q must be a single path segment matching %s",
				root, name, serviceNamePattern)
		}

		spec := firstExisting(dir, specFileNames)
		if spec == "" {
			// Silently skipping would make a misnamed document look like a
			// service that has no tools.
			return nil, fmt.Errorf("service %q at %s contains no OpenAPI document (looked for %v)",
				name, dir, specFileNames)
		}

		service := ServiceConfig{
			Name:           name,
			SwaggerFile:    spec,
			AdjustmentFile: firstExisting(dir, adjustmentFileNames),
		}

		if overridesFile := firstExisting(dir, serviceFileNames); overridesFile != "" {
			overrides, err := readServiceOverrides(overridesFile)
			if err != nil {
				return nil, err
			}
			service.Endpoint = overrides.Endpoint
			service.UpstreamSecurity = overrides.UpstreamSecurity
			if overrides.AdjustmentFile != "" {
				service.AdjustmentFile = overrides.AdjustmentFile
			}
		}

		discovered = append(discovered, service)
	}

	// os.ReadDir is already sorted, but the guarantee matters enough to state:
	// the order decides nothing functional, and a stable one keeps logs and
	// tests comparable between runs.
	sort.Slice(discovered, func(i, j int) bool { return discovered[i].Name < discovered[j].Name })
	return discovered, nil
}

func readServiceOverrides(path string) (serviceOverrides, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- path comes from the configured services directory
	if err != nil {
		return serviceOverrides{}, fmt.Errorf("reading %s: %w", path, err)
	}
	var overrides serviceOverrides
	if err := yaml.Unmarshal(data, &overrides); err != nil {
		return serviceOverrides{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	return overrides, nil
}

func firstExisting(dir string, names []string) string {
	for _, name := range names {
		path := filepath.Join(dir, name)
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path
		}
	}
	return ""
}
