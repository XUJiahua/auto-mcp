package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/pflag"
	"github.com/spf13/viper"
)

// Version information - set by GoReleaser during build
var (
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

// GetVersionInfo returns a formatted version string
func GetVersionInfo() string {
	return fmt.Sprintf("auto-mcp version %s, commit %s, built at %s", version, commit, date)
}

type Config struct {
	Server         ServerConfig   `mapstructure:"server"`
	Logging        LoggingConfig  `mapstructure:"logging"`
	EndpointConfig EndpointConfig `mapstructure:"endpoint"`
	SwaggerFile    string         `mapstructure:"swagger_file"`
	AdjustmentFile string         `mapstructure:"adjustment_file"`
	OAuth          *OAuthConfig   `mapstructure:"oauth"`

	// SecuritySchemes describe the credentials this deployment knows about;
	// the two requirements below say which direction each is used in.
	SecuritySchemes []SecurityScheme `mapstructure:"security_schemes"`
	// DownstreamSecurity authenticates the MCP client calling this server.
	DownstreamSecurity *SecurityRequirement `mapstructure:"downstream_security"`
	// UpstreamSecurity authenticates this server to the API it proxies. It is
	// the single-service form of ServiceConfig.UpstreamSecurity.
	UpstreamSecurity *SecurityRequirement `mapstructure:"upstream_security"`

	// Services exposes several upstreams from one process, each on its own route.
	Services []ServiceConfig `mapstructure:"services"`
}

// EndpointConfig describes the API being proxied. Authentication to it is
// configured with upstream_security rather than here, so that the credential is
// described in the same vocabulary as the one callers present.
type EndpointConfig struct {
	BaseURL string            `json:"base_url" mapstructure:"base_url"`
	Headers map[string]string `json:"headers" mapstructure:"headers"`
}

type ServerMode string

const (
	ServerModeSSE   ServerMode = "sse"
	ServerModeSTDIO ServerMode = "stdio"
	ServerModeHTTP  ServerMode = "http"
)

// valid reports whether the mode is one this server can serve.
func (m ServerMode) valid() bool {
	switch m {
	case ServerModeSSE, ServerModeSTDIO, ServerModeHTTP:
		return true
	default:
		return false
	}
}

type ServerConfig struct {
	Port    int        `mapstructure:"port"`
	Host    string     `mapstructure:"host"`
	Timeout string     `mapstructure:"timeout"`
	Mode    ServerMode `mapstructure:"mode"`
	Name    string     `mapstructure:"name"`
	Version string     `mapstructure:"version"`
}

type LoggingConfig struct {
	Level             string `mapstructure:"level"`
	Format            string `mapstructure:"format"`
	Color             bool   `mapstructure:"color"`
	DisableStacktrace bool   `mapstructure:"disable_stacktrace"`
	OutputPath        string `mapstructure:"output_path"`
	AppendToFile      bool   `mapstructure:"append_to_file"`
	DisableConsole    bool   `mapstructure:"disable_console"`
}

type OAuthConfig struct {
	Enabled      bool     `mapstructure:"enabled" `
	Provider     string   `mapstructure:"provider"` // github or google
	ClientID     string   `mapstructure:"client_id"`
	ClientSecret string   `mapstructure:"client_secret"`
	Scopes       []string `mapstructure:"scopes"`
	AllowOrigins []string `mapstructure:"allow_origins"`
}

// InitFlags initializes command line flags (without parsing)
func InitFlags() {
	pflag.String("mode", string(ServerModeSTDIO), "Server mode (stdio|sse|http)")
	pflag.String("swagger-file", "", "Path to the swagger file")
	pflag.String("adjustment-file", "", "Path to the adjustment file")
	// Note: no pflag.Parse() here as it's called in main.go
}

// bindFlagsToConfigKeys ties each flag to the configuration key it sets.
//
// Binding by key rather than reading the flag afterwards is what makes the
// documented precedence work. The --mode flag carries a non-empty default, so
// applying it unconditionally overrode every other source: server.mode in a
// config file and AUTO_MCP_SERVER_MODE both had no effect, and the process came
// up in stdio unless --mode was passed explicitly. viper only prefers a bound
// flag when it was actually changed, and falls back to its default last.
func bindFlagsToConfigKeys() error {
	bindings := map[string]string{
		"server.mode":     "mode",
		"swagger_file":    "swagger-file",
		"adjustment_file": "adjustment-file",
	}
	for key, name := range bindings {
		flag := pflag.CommandLine.Lookup(name)
		if flag == nil {
			continue
		}
		if err := viper.BindPFlag(key, flag); err != nil {
			return err
		}
	}
	return nil
}

// bindDocumentedEnvKeys registers the keys that docs/CONFIGURATION.md promises
// are settable by environment variable but that have no default to make them
// known to viper.
//
// AutomaticEnv alone is not enough: Unmarshal only consults the keys viper knows
// about, so an environment variable for a key that appears in neither the
// defaults, a config file, nor a bound flag is read by Get but never reaches the
// struct. AUTO_MCP_ENDPOINT_BASE_URL was in exactly that position.
func bindDocumentedEnvKeys() {
	for _, key := range []string{
		"endpoint.base_url",
		"oauth.enabled",
		"oauth.provider",
		"oauth.client_id",
		"oauth.client_secret",
		"oauth.scopes",
		"oauth.host",
		"oauth.port",
	} {
		// BindEnv only fails when given no key at all.
		_ = viper.BindEnv(key)
	}
}

// setDefaults registers values that make a flags-only or environment-only start
// produce a working process. Without them, tolerating a missing config file
// would only move the failure: the server would bind port 0 under an empty name.
func setDefaults() {
	viper.SetDefault("server.mode", string(ServerModeSTDIO))
	viper.SetDefault("server.port", 8080)
	// Loopback by default. The MCP endpoint has no authentication of its own
	// (see docs/KNOWN_ISSUES.md), so a process started with no configuration at
	// all must not be reachable from off the machine; exposing it is an explicit
	// decision the operator makes by setting a host.
	viper.SetDefault("server.host", "localhost")
	viper.SetDefault("server.timeout", "30s")
	viper.SetDefault("server.name", "Auto MCP")
	viper.SetDefault("server.version", "1.0.0")

	viper.SetDefault("logging.level", "info")
	viper.SetDefault("logging.format", "json")
	viper.SetDefault("logging.color", true)

}

func Load() (*Config, error) {
	viper.Reset() // Ensure clean state

	viper.SetEnvPrefix("AUTO_MCP")
	viper.SetEnvKeyReplacer(strings.NewReplacer(".", "_", "-", "_"))
	viper.AutomaticEnv()

	if err := viper.BindPFlags(pflag.CommandLine); err != nil {
		return nil, err
	}
	if err := bindFlagsToConfigKeys(); err != nil {
		return nil, err
	}
	bindDocumentedEnvKeys()

	setDefaults()

	// Load ./config.yaml first
	viper.SetConfigName("config")
	viper.SetConfigType("yaml")
	viper.AddConfigPath(".")

	viper.AddConfigPath("/etc/auto-mcp")

	// A missing config.yaml is not an error: flags and AUTO_MCP_* environment
	// variables are documented as complete configuration paths on their own, and
	// a container image has no reason to carry a config file. A file that exists
	// but cannot be parsed is still fatal — silently falling back to defaults
	// would start a server that ignores the operator's stated intent.
	if err := viper.ReadInConfig(); err != nil {
		var notFound viper.ConfigFileNotFoundError
		if !errors.As(err, &notFound) && !os.IsNotExist(err) {
			return nil, err
		}
	}

	//Loading additionals config files
	if _, err := os.Stat("/config/config.yaml"); err == nil {
		viper.SetConfigFile("/config/config.yaml")
		// Merge /config/config.yaml (overrides overlapping keys)
		if err := viper.MergeInConfig(); err != nil {
			// It's OK if this file doesn't exist, only error if it's another problem
			if _, ok := err.(viper.ConfigFileNotFoundError); !ok {
				return nil, err
			}
		}
	}

	var config Config
	if err := viper.Unmarshal(&config); err != nil {
		return nil, err
	}
	// The flags are bound to their config keys, so viper has already applied the
	// precedence explicit flag > environment > file > default.

	if !config.Server.Mode.valid() {
		return nil, fmt.Errorf("unsupported server mode %q, expected one of stdio, sse, http", config.Server.Mode)
	}

	if err := config.resolveSecurity(); err != nil {
		return nil, err
	}

	if config.OAuth != nil && len(config.OAuth.Scopes) == 1 {
		if strings.Contains(config.OAuth.Scopes[0], " ") {
			config.OAuth.Scopes = strings.Fields(config.OAuth.Scopes[0])
		}
	}

	return &config, nil
}
