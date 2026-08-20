# Known issues

针对「放一份商户 OpenAPI 就得到一条 MCP 路由」这个目标，下面是已定位、未解决的问题。
每条都带定位到文件的锚点。已解决的部分见 PR #1。

MCP 库已从 `github.com/mark3labs/mcp-go` 换成官方 `github.com/modelcontextprotocol/go-sdk`，
本文件中受此影响的条目已相应更新。

## 1. 多租户: 一份 spec 一条路由(未做)

现在的架构是「一个进程一份 spec」。三处挡着:

- `cmd/auto-mcp/main.go` 把 `config.EndpointConfig` 作为 **fx 单例**提供
  (`fx.Provide(func() *config.EndpointConfig { ... })`)，全进程共享一个 `BaseURL`、
  一种 auth、一个 token。
- `internal/server/server.go` 的 `Server` 只持有一个 `*mcpserver.MCPServer`，
  `setupTools()` 从 `cfg.SwaggerFile` 读**一份** spec。
- `internal/server/handler/http.go` 把 MCP handler 挂在 `mux.Handle("/")`，
  没有按商户区分的路径。

改动面比看上去集中:

- `NewHTTPRequester` / `NewHTTPAuthManager` / `NewHTTPRequestBuilder` 三个构造函数
  **已经是参数化的**，只是被 fx 喂了单例值。
- **按请求选 server 的挂点已经就位**。换库之后两个 HTTP 传输都走
  `mcp.NewStreamableHTTPHandler(getServer, ...)` / `mcp.NewSSEHandler(getServer, ...)`，
  `getServer` 的签名是 `func(*http.Request) *mcp.Server` —— 官方 SDK 天生按请求解析
  server，而不是构造时绑定一个。当前实现是
  `internal/server/server.go` 的 `(*Server).serverForRequest`，返回唯一那个 server;
  按 `/mcp/{merchant}` 查表就加在这里。

还需要注意:
- 商户下线时对应路由要能摘掉，不能只靠重启进程。
- 上游调用方的 HTTP 头可以从 `request.Extra.Header` 读到(`mcp.CallToolRequest` =
  `ServerRequest[*CallToolParamsRaw]`)，需要把调用方的头转发给上游时用得上。

## 2. `/mcp` 默认无鉴权，而进程持有全部商户的上游凭证

`internal/server/handler/http.go`，OAuth 关闭时:

```go
mux.Handle("/", mcpHandler)
logger.Info("Running without authentication")
```

单 spec 本地使用没问题。做成多租户之后，这个进程同时持有**所有**商户的上游地址与
凭证，一个无鉴权的 `/mcp` 等于任何能连上该端口的人都可以借我们的凭证向商户下单。

至少要有其一:静态 bearer；或只监听 loopback、由同机调用方访问。两者都没有时
多租户不应上线。

## 3. `outputSchema` 无法暴露 —— 已解决

换用官方 SDK 后解决。`mcp.Tool.OutputSchema` 的类型是 `any`，可以直接放一份自己
构造的 JSON Schema。

实现:`internal/parser/parser.go` 的 `responseSchema` 取 2xx 响应的 schema
(`200` → `201` → `default`，媒体类型按名字排序保证确定性)，走同一个 `jsonSchemaFor`。
**顶层不是 object 的响应(数组、标量)不声明** —— MCP 要求 outputSchema 顶层是 object，
声明错的形状比不声明更糟:会校验的客户端会拒掉每一次成功调用。

同时补上了配套的 `structuredContent`(`internal/server/tool/handler.go` 的
`successResult`):声明了 outputSchema 却只回文本，等于叫客户端等一个永不到达的
结构化结果。上游回的不是 JSON 对象时降级为仅文本，不报协议错误。

顺带一个前提也随之满足:`outputSchema` 要求 2025-06-18 之后的协议，而旧库协商出来的是
2025-03-26。现在协商结果是 **2025-11-25**。

## 4. `oneOf` / `anyOf` 的转换是有损的(需确认取舍)

`internal/parser/jsonschema.go` 的 `unionBranches` 把分支并成一个对象:属性取并集、
不标任何必填、分支要求写进 `description`。

换来的是分支字段可达。代价是 schema 不再能表达"必须恰好满足一支"。忠实输出
`anyOf` 的话，不实现组合的消费方会把整个属性压成一个标量，那样分支字段一个都碰不到 ——
这是选它的原因，但取舍值得复核。

## 5. 没有 spec 注册 / 热加载入口

加一个商户现在等于改 `config.yaml` 再重启进程。目标形态需要其一:
目录扫描(`specs/<noun>/openapi.yaml` + 每商户配置)+ SIGHUP reload，或一个
管理用 HTTP 端点。注意这个端点本身要鉴权(见 #2)。

## 6. 缺少 `config.yaml` 时无法仅用 flag / env 启动

`config.Load()` 找不到配置文件就直接失败:

```
Failed to load configuration: Config File "config" Not Found in "[. /etc/auto-mcp]"
```

文档把 CLI flag 与 `AUTO_MCP_*` 环境变量描述为可用的配置方式，但没有配置文件时
两者都到不了启动那一步。容器化部署里这条最碍事。

## 7. 既有 lint 告警 —— 已解决

`internal/tui/main_page.go` 的两处 `QF1012` 已改为 `fmt.Fprintf`。
`golangci-lint run ./...` 现在是 0 issues。

## 8. Go 版本要求提高到 1.25

官方 SDK 的 `go.mod` 声明 `go 1.25.0`，因此本仓的 go 指令从 1.24.1 提到 1.25.0。
`.github/workflows/*.yml` 与 `Dockerfile` 里固定的版本已同步更新。使用旧工具链构建的
下游需要一并升级。
