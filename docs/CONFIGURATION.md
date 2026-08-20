# ⚙️ Configuration

## Configuration Options

Auto MCP accepts configuration via **CLI flags**, **environment variables** (prefix `AUTO_MCP_`), or an optional `config.yaml`. In containerized deployments environment variables are the most convenient.

| Purpose                               | Env variable                          | Example                          |
| ------------------------------------- | ------------------------------------- | -------------------------------- |
| Select transport                      | `AUTO_MCP_SERVER_MODE`                | `stdio` or `http` or `sse`       |
| Bind port (SSE)                       | `AUTO_MCP_SERVER_PORT`                | `8080`                           |
| Upstream base URL                     | `AUTO_MCP_ENDPOINT_BASE_URL`          | `https://petstore.swagger.io/v2` |
| Extra static header                   | `AUTO_MCP_ENDPOINT_HEADERS_X_CUSTOM`  | `hello`                          |
| Log level                             | `AUTO_MCP_LOGGING_LEVEL`              | `debug`                          |
| Path to swagger file                  | `AUTO_MCP_SWAGGER_FILE`               | `/server/swagger.json`           |
| Path to adjustment file (mcp-builder) | `AUTO_MCP_ADJUSTMENT_FILE`            | `/server/adjustments.json`       |
| Enable OAuth                          | `AUTO_MCP_OAUTH_ENABLED`              | `true`                           |
| OAuth provider                        | `AUTO_MCP_OAUTH_PROVIDER`             | `github` / `google`              |
| OAuth client ID                       | `AUTO_MCP_OAUTH_CLIENT_ID`            | `your-client-id`                 |
| OAuth client secret                   | `AUTO_MCP_OAUTH_CLIENT_SECRET`        | `your-client-secret`             |
| OAuth scopes                          | `AUTO_MCP_OAUTH_SCOPES`               | `openid email profile`           |
| OAuth host (optional)                 | `AUTO_MCP_OAUTH_HOST`                 | `localhost`                      |
| OAuth port (optional)                 | `AUTO_MCP_OAUTH_PORT`                 | `8080`                           |
| Server name (display)                 | `AUTO_MCP_SERVER_NAME`                | `Auto MCP`                       |
| Server version (display)              | `AUTO_MCP_SERVER_VERSION`             | `1.0.0`                          |

Underscores replace dots in the YAML path; nested keys keep the hierarchy (e.g., `endpoint.auth_config.token` → `AUTO_MCP_ENDPOINT_AUTH_CONFIG_TOKEN`).

CLI shortcuts:

- `--mode` – overrides the transport.
- `--swagger-file` – absolute or relative path to the OpenAPI document.
- `--adjustment-file` - mcp-config-builder output filter/change route descriptions.

> Upstream authentication is configured with `security_schemes` +
> `upstream_security`; see the section below. There is no `endpoint.auth_type`.

---

## Environment Variables

Underscores replace dots and keys are upper-cased. For example, to change the port and log level when using Docker:

```bash
# Unix shell
docker run -e AUTO_MCP_SERVER_PORT=8080 \
           -v $(pwd)/swagger.json:/server/swagger.json \
           -p 8080:8080 auto-mcp:latest --mode=sse --swagger-file=/swagger.json
```

with adjustments

```bash
# Run with adjustment file
docker run --rm -i \
  -v $(pwd)/swagger.json:/server/swagger.json \
  -v $(pwd)/adjustments.json:/server/adjustments.json \
  auto-mcp:latest --mode=stdio \
  --swagger-file=/server/swagger.json \
  --adjustment-file=/server/adjustments.json
```

---

## 📝 Mounting and Overwriting `config.yaml` (Full Configuration)

Instead of passing individual CLI flags or environment variables, you can mount a complete `config.yaml` into the container to control all aspects of Auto MCP—including the Swagger and adjustment files, server mode, logging, authentication, and OAuth settings.

**Recommended for production or reproducible deployments.**

### Example Directory Structure

Suppose you have the following files in a directory (e.g., `examples/petshop/config`):

- `config.yaml` (references the other files by their paths)
- `swagger.json` (your OpenAPI/Swagger definition)
- `adjustment.yaml` (optional, for endpoint adjustments)

Your `config.yaml` should include references like:

```yaml
swagger_file: "/config/swagger.json"
adjustments_file: "/config/adjustment.yaml"
```

### Mounting the Entire Config Directory in Docker

```bash
docker run --rm -i \
  -v $(pwd)/examples/petshop/config:/config \
  ghcr.io/brizzai/auto-mcp:latest
```

- The container will use `/config/config.yaml` for all configuration.
- The `swagger_file` and `adjustments_file` paths in your config should match the mount locations.
- No need to pass `--swagger-file` or `--adjustment-file` flags if set in `config.yaml`.

This approach keeps all related files together and is ideal for local development or sharing example setups.

---

## 🗂️ Serving several APIs from one process

One `swagger_file` at the top level is the single-service form: its endpoint stays
at `/mcp`, and `stdio` works because there is only one thing to talk to.

Several APIs are listed under `services`, each becoming its own MCP endpoint at
`/mcp/{name}`:

```yaml
security_schemes:
  - id: hotel_key
    type: apiKey
    in: header
    name: X-API-Key
    default_credential: "${HOTEL_KEY}"
  - id: flight_key
    type: apiKey
    in: header
    name: X-API-Key
    default_credential: "${FLIGHT_KEY}"

services:
  - name: hotel # served at /mcp/hotel
    swagger_file: hotel.yaml
    adjustment_file: hotel-adjustments.yaml
    endpoint:
      base_url: https://hotel.example.com
    upstream_security:
      id: hotel_key
  - name: flight # served at /mcp/flight
    swagger_file: flight.yaml
    endpoint:
      base_url: https://flight.example.com
    upstream_security:
      id: flight_key
```

Each service gets its own document, its own adjustments, its own address and its
own credential. Nothing is shared but the listener and the front-door
authentication, so one upstream's credential cannot reach another's endpoint.

Security schemes stay global because a scheme describes how a credential is
carried, not which upstream it belongs to; the same scheme can be referenced by
several services with different credentials.

A few rules:

- The name is a route segment, so it must be a single path segment
  (`[a-zA-Z0-9][a-zA-Z0-9_-]*`). Names must be unique, and every service must be
  named once more than one is configured.
- A route naming no configured service is a **404**, not an empty tool list: a
  typo in the address must not look like a service with nothing in it.
- `stdio` serves a single service. It speaks to one client over one pipe, so
  there is no address to tell services apart by.

### Dropping in a service directory

`services_dir` is scanned for subdirectories, each becoming a service named after
itself. Adding an API then means adding a directory rather than editing
configuration:

```
services/
  hotel/
    openapi.yaml      # or .yml/.json, or swagger.*
    service.yaml      # optional: endpoint, upstream_security, adjustment_file
    adjustment.yaml   # optional, picked up without being named
  flight/
    openapi.json
```

```yaml
services_dir: services
```

`service.yaml` carries what the document cannot — where to send the requests and
which credential to use:

```yaml
endpoint:
  base_url: https://hotel.example.com
upstream_security:
  id: hotel_key
```

It cannot set the name or the document path: those come from the directory, and a
file able to rename its own directory would make the route depend on two places
at once. Discovered and explicitly listed services combine; a name declared in
both is an error.

A directory without an OpenAPI document is an error rather than a skip, because
skipping would make a misnamed document look like a service with no tools.

### Reloading without a restart

`SIGHUP` rescans `services_dir` and brings the running server in line with it:

```bash
kill -HUP $(pgrep auto-mcp)
```

Existing services are updated in place, keeping their `mcp.Server`, so **open
sessions stay connected** and receive `notifications/tools/list_changed` as tools
are added or removed. That is the protocol's own answer to a changing tool set,
so a reload neither disconnects clients nor leaves them holding a list that no
longer exists.

A reload that cannot be completed changes nothing: every service is rebuilt
before any of them is swapped in, so a spec that stopped parsing leaves a process
that was serving correctly still serving. The failure is logged.

## ✂️ Shaping upstream responses

The adjustment file already selects routes and rewrites descriptions; it also
carries per-route response templates. A whole upstream response is usually much
larger and noisier than the part a caller needs — pagination metadata, internal
trace ids, dozens of null fields — and all of it lands in a model's context.

```yaml
responses:
  - path: /api/hotel
    updates:
      - method: GET
        prepend_body: "Hotel: "
        body: "{{ .bussinessResponse.hotelName }} ({{ .bussinessResponse.starRate }} stars)"
        append_body: ""
        error_body: "upstream refused: {{ .returnMsg }}"
```

`body` is a Go template evaluated against the parsed JSON response.
`prepend_body` and `append_body` wrap the result and work with or without a
`body` template. `error_body` replaces `body` when the upstream reports a
failure, since the fields that explain a failure are rarely the ones that carry
a result.

A template that cannot be applied — malformed syntax, a response that is not
JSON, a reference to a field that is not there — leaves the response untouched
and logs why. Discarding the response would discard the only evidence of what
the upstream actually said.

When a template is configured the result carries no `structuredContent`: the
template states what the caller should see, and sending the untrimmed payload
alongside it would put back exactly what was removed.

## 🔑 Authenticating callers and upstreams

Credentials are described once as **security schemes** and then referred to by
whichever direction uses them:

```yaml
security_schemes:
  - id: caller # authenticates the MCP client calling this server
    type: http
    scheme: bearer # basic | bearer
    default_credential: "${AUTO_MCP_DOWNSTREAM_TOKEN}"
  - id: upstream_key # authenticates this server to the API it proxies
    type: apiKey
    in: header # header | query
    name: X-API-Key
    default_credential: "${UPSTREAM_API_KEY}"

downstream_security: # client → auto-mcp
  id: caller
upstream_security: # auto-mcp → upstream API
  id: upstream_key
```

`${VAR}` is read from the environment, so credentials need not be committed to a
file. A referenced variable that is not set is a startup error rather than an
empty credential, because an empty credential reaches the upstream as a
permissions failure that points nowhere near the cause.

Two rules are enforced at startup:

- **A host reachable from outside this machine must authenticate its callers.**
  `server.host` other than `localhost`/`127.0.0.1`/`::1` requires either
  `downstream_security` or `oauth.enabled`. The MCP endpoint holds whatever
  credential the upstream requires, so an open port lends those credentials to
  anyone who can reach it. `stdio` has no socket and is exempt.
- **A requirement must have a credential to use**, either its own `credential`,
  the scheme's `default_credential`, or `passthrough`.

`upstream_security` also accepts `passthrough: true`, which forwards the
caller's own credential to the upstream instead of one of ours. It never falls
back to the configured credential: sending the platform's identity on behalf of
a caller who presented none is the failure that would hide.

An upstream that needs no credential is configured by saying nothing: omit
`upstream_security`.

## Example config.yaml

```yaml
server:
  mode: http # Server mode: http, stdio, or sse
  port: 8080 # Port to bind (for http/sse)
  host: "0.0.0.0" # Host to bind
  timeout: 30s # Request timeout (e.g., 30s, 1m)
  name: "Auto MCP" # Server display name
  version: "1.0.0" # Server version string

logging:
  level: "info" # Log level: debug, info, warn, error
  format: "json" # Log format: json or console
  color: true # Enable color in logs (console only)
  disable_stacktrace: false # Disable stacktraces in logs
  output_path: "logs/auto-mcp.log" # Log file path
  append_to_file: true # Append to log file if true
  disable_console: false # Disable console logging if true

endpoint:
  base_url: "https://petstore.swagger.io/v2" # Upstream API base URL
  # auth_config:           # (optional) Auth config map, e.g. {token: "..."}
  # headers:               # (optional) Extra headers map, e.g. {X-Api-Key: "..."}

oauth:
  enabled: false # Enable OAuth2 authentication
  provider: github # OAuth provider (github, google)
  client_id: "" # OAuth client ID
  client_secret: "" # OAuth client secret
  scopes: "" # OAuth scopes (space-separated)
  allow_origins: [] # List of allowed CORS origins

swagger_file: "/config/swagger.json" # Path to OpenAPI/Swagger file
adjustments_file: "/config/adjustment.yaml" # Path to adjustments file
```
