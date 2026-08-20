package tests

import (
	"context"
	"net/http"
	"testing"

	"github.com/brizzai/auto-mcp/internal/config"
	"github.com/brizzai/auto-mcp/internal/requester"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockAuthManager struct {
	applyAuthFunc func(*http.Request) error
}

func (m *mockAuthManager) ApplyAuth(req *http.Request) error {
	return m.applyAuthFunc(req)
}

func TestHTTPRequestBuilder_BuildRequest(t *testing.T) {
	tests := []struct {
		name         string
		route        string
		params       map[string]interface{}
		config       *config.EndpointConfig
		authManager  requester.AuthManager
		routeConfig  *requester.RouteConfig
		wantErr      bool
		checkRequest func(t *testing.T, req *requester.Request)
	}{
		{
			name:  "Simple GET Request",
			route: "test-route",
			params: map[string]interface{}{
				"query": "test",
			},
			config: &config.EndpointConfig{
				BaseURL: "http://api.example.com",
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
			routeConfig: &requester.RouteConfig{
				Method: "GET",
				Path:   "/test-route",
				// 声明这个查询参数。此前这里是空的，用例靠的是"未声明的参数
				// 也会被塞进查询串"那条旧行为 —— 它把调用方的笔误变成一个
				// 悄悄跑偏的请求，所以那条行为被去掉了。
				MethodConfig: requester.MethodConfig{Params: []requester.ParamConfig{
					{Name: "query", In: requester.ParamInQuery, Type: "string", Explode: true},
				}},
			},
			authManager: &mockAuthManager{
				applyAuthFunc: func(req *http.Request) error {
					req.Header.Set("Authorization", "Bearer test-token")
					return nil
				},
			},
			wantErr: false,
			checkRequest: func(t *testing.T, req *requester.Request) {
				assert.Equal(t, "http://api.example.com/test-route?query=test", req.HttpRequest.URL.String())
				assert.Equal(t, "GET", req.HttpRequest.Method)
				assert.Equal(t, "application/json", req.HttpRequest.Header.Get("Content-Type"))
				assert.Equal(t, "Bearer test-token", req.HttpRequest.Header.Get("Authorization"))
			},
		},
		{
			name:  "POST Request with Body",
			route: "create-resource",
			params: map[string]interface{}{
				"body": map[string]interface{}{
					"name": "test",
				},
			},
			config: &config.EndpointConfig{
				BaseURL: "http://api.example.com",
				Headers: map[string]string{
					"Content-Type": "application/json",
				},
			},
			routeConfig: &requester.RouteConfig{
				Method: "POST",
				Path:   "/create-resource",
				// 声明请求体的媒体类型。文档声明了体，这里才发体 —— 此前空着
				// 也能发，于是"文档没声明"与"fixture 忘了写"分不开。
				MethodConfig: requester.MethodConfig{BodyContentType: "application/json"},
			},
			authManager: &mockAuthManager{
				applyAuthFunc: func(req *http.Request) error {
					return nil
				},
			},
			wantErr: false,
			checkRequest: func(t *testing.T, req *requester.Request) {
				assert.Equal(t, "http://api.example.com/create-resource", req.HttpRequest.URL.String())
				assert.Equal(t, "POST", req.HttpRequest.Method)
				assert.Equal(t, "application/json", req.HttpRequest.Header.Get("Content-Type"))
			},
		},
		{
			name:   "Invalid Route",
			route:  "invalid-route",
			params: map[string]interface{}{},
			config: &config.EndpointConfig{
				BaseURL: "http://api.example.com",
			},
			routeConfig: nil,
			authManager: &mockAuthManager{
				applyAuthFunc: func(req *http.Request) error {
					return nil
				},
			},
			wantErr:      true,
			checkRequest: func(t *testing.T, req *requester.Request) {},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			builder := requester.NewHTTPRequestBuilder(requester.HTTPRequestBuilderParams{
				EndpointConfig: tt.config,
				AuthManager:    tt.authManager,
				RouteConfig:    tt.routeConfig,
			})

			req, err := builder.BuildRequest(context.Background(), tt.params)
			if tt.wantErr {
				assert.Error(t, err)
				return
			}

			require.NoError(t, err)
			tt.checkRequest(t, req)
		})
	}
}
