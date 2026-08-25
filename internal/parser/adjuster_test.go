package parser

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"

	"github.com/brizzai/auto-mcp/internal/models"
	"github.com/stretchr/testify/assert"
)

func TestAdjuster_ExistsInMCP(t *testing.T) {
	tests := []struct {
		name     string
		adjuster *Adjuster
		route    string
		method   string
		want     bool
	}{
		{
			name: "Route and method are in selected routes",
			adjuster: &Adjuster{
				adjustments: &models.MCPAdjustments{
					Routes: []models.RouteSelection{
						{
							Path:    "/api/users",
							Methods: []string{"GET", "POST"},
						},
					},
				},
			},
			route:  "/api/users",
			method: "GET",
			want:   true, // Exists in MCP
		},
		{
			name: "Route exists but method is not in selected routes",
			adjuster: &Adjuster{
				adjustments: &models.MCPAdjustments{
					Routes: []models.RouteSelection{
						{
							Path:    "/api/users",
							Methods: []string{"GET", "POST"},
						},
					},
				},
			},
			route:  "/api/users",
			method: "DELETE",
			want:   false, // Doesn't exist in MCP
		},
		{
			name: "Route does not exist in selected routes",
			adjuster: &Adjuster{
				adjustments: &models.MCPAdjustments{
					Routes: []models.RouteSelection{
						{
							Path:    "/api/users",
							Methods: []string{"GET", "POST"},
						},
					},
				},
			},
			route:  "/api/products",
			method: "GET",
			want:   false, // Doesn't exist in MCP
		},
		{
			name: "SelectedRoutes is empty",
			adjuster: &Adjuster{
				adjustments: &models.MCPAdjustments{
					Routes: []models.RouteSelection{},
				},
			},
			route:  "/api/users",
			method: "GET",
			want:   true, // Everything exists when no filtering
		},
		{
			name: "Adjustments is nil",
			adjuster: &Adjuster{
				adjustments: nil,
			},
			route:  "/api/users",
			method: "GET",
			want:   true, // Everything exists when no filtering
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.adjuster.ExistsInMCP(tt.route, tt.method)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestAdjuster_GetDescription(t *testing.T) {
	originalDesc := "Original description"
	newDesc := "New description"

	tests := []struct {
		name     string
		adjuster *Adjuster
		route    string
		method   string
		origDesc string
		want     string
	}{
		{
			name: "Route and method have updated description",
			adjuster: &Adjuster{
				adjustments: &models.MCPAdjustments{
					Descriptions: []models.RouteDescription{
						{
							Path: "/api/users",
							Updates: []models.RouteFieldUpdate{
								{
									Method:         "GET",
									NewDescription: newDesc,
								},
							},
						},
					},
				},
			},
			route:    "/api/users",
			method:   "GET",
			origDesc: originalDesc,
			want:     newDesc,
		},
		{
			name: "Route exists but method has no updated description",
			adjuster: &Adjuster{
				adjustments: &models.MCPAdjustments{
					Descriptions: []models.RouteDescription{
						{
							Path: "/api/users",
							Updates: []models.RouteFieldUpdate{
								{
									Method:         "POST",
									NewDescription: newDesc,
								},
							},
						},
					},
				},
			},
			route:    "/api/users",
			method:   "GET",
			origDesc: originalDesc,
			want:     originalDesc,
		},
		{
			name: "Route does not exist in UpdateDescriptions",
			adjuster: &Adjuster{
				adjustments: &models.MCPAdjustments{
					Descriptions: []models.RouteDescription{
						{
							Path: "/api/users",
							Updates: []models.RouteFieldUpdate{
								{
									Method:         "GET",
									NewDescription: newDesc,
								},
							},
						},
					},
				},
			},
			route:    "/api/products",
			method:   "GET",
			origDesc: originalDesc,
			want:     originalDesc,
		},
		{
			name: "UpdateDescriptions is empty",
			adjuster: &Adjuster{
				adjustments: &models.MCPAdjustments{
					Descriptions: []models.RouteDescription{},
				},
			},
			route:    "/api/users",
			method:   "GET",
			origDesc: originalDesc,
			want:     originalDesc,
		},
		{
			name: "Adjustments is nil",
			adjuster: &Adjuster{
				adjustments: nil,
			},
			route:    "/api/users",
			method:   "GET",
			origDesc: originalDesc,
			want:     originalDesc,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.adjuster.GetDescription(tt.route, tt.method, tt.origDesc)
			assert.Equal(t, tt.want, got)
		})
	}
}

// The method is matched without regard to case. OpenAPI spells its own path-item
// keys in lower case, the bundled TUI writes them in upper case, and a mismatch
// used to mean the route quietly did not exist — a filter that silently selects
// nothing is indistinguishable from a document with no routes.
func TestAdjuster_MethodMatchingIgnoresCase(t *testing.T) {
	for _, written := range []string{"get", "GET", "Get"} {
		t.Run(written, func(t *testing.T) {
			a := NewAdjuster()
			require.NoError(t, a.LoadReader(strings.NewReader(
				"routes:\n  - path: /pet\n    methods: ["+written+"]\n"+
					"descriptions:\n  - path: /pet\n    updates:\n      - method: "+written+
					"\n        new_description: curated\n")))

			assert.True(t, a.ExistsInMCP("/pet", "GET"), "route selection must match")
			assert.Equal(t, "curated", a.GetDescription("/pet", "GET", "original"),
				"description override must match")
		})
	}
}

// A method that was not selected is still excluded.
func TestAdjuster_UnselectedMethodStaysExcluded(t *testing.T) {
	a := NewAdjuster()
	require.NoError(t, a.LoadReader(strings.NewReader("routes:\n  - path: /pet\n    methods: [get]\n")))

	assert.True(t, a.ExistsInMCP("/pet", "GET"))
	assert.False(t, a.ExistsInMCP("/pet", "POST"))
}
