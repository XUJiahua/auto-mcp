package server

import (
	"encoding/json"
	"fmt"
)

// SchemaSize reports how much schema one service publishes.
//
// A tool's inputSchema travels to the client on every tools/list and lands in a
// model's context, so its size is a running cost rather than a one-off. Reporting
// it at startup turns that cost into a number someone can act on, instead of
// something noticed later as latency and tokens.
type SchemaSize struct {
	Service      string
	Tools        int
	TotalBytes   int
	LargestTool  string
	LargestBytes int
}

// SchemaSizes returns the published schema size of each service, ordered by
// service name.
func (s *Server) SchemaSizes() []SchemaSize {
	s.mu.RLock()
	defer s.mu.RUnlock()

	sizes := make([]SchemaSize, 0, len(s.schemaSizes))
	for _, name := range s.serviceNamesLocked() {
		sizes = append(sizes, s.schemaSizes[name])
	}
	return sizes
}

// measureSchemas totals the tool schemas of one service and enforces the
// configured per-tool limit.
//
// The limit is checked here, while the tools are being built, so that a
// pathological document is refused at startup rather than at the first call —
// and, because a reload builds everything before swapping anything in, a spec
// that grew past the limit cannot take down a process that is serving.
func measureSchemas(service string, tools []*registeredTool, maxKiB int) (SchemaSize, error) {
	size := SchemaSize{Service: service, Tools: len(tools)}
	limit := maxKiB * 1024

	for _, registered := range tools {
		encoded, err := json.Marshal(registered.tool.InputSchema)
		if err != nil {
			return SchemaSize{}, fmt.Errorf("service %q: tool %q has an unserialisable input schema: %w",
				service, registered.tool.Name, err)
		}
		bytes := len(encoded)

		if limit > 0 && bytes > limit {
			return SchemaSize{}, fmt.Errorf(
				"service %q: tool %q has a %d KiB input schema, over the %d KiB max_tool_schema_kib; "+
					"trim the operation with an adjustment file or raise the limit",
				service, registered.tool.Name, bytes/1024, maxKiB)
		}

		size.TotalBytes += bytes
		if bytes > size.LargestBytes {
			size.LargestBytes = bytes
			size.LargestTool = registered.tool.Name
		}
	}
	return size, nil
}
