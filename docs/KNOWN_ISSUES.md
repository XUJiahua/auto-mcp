# Known issues

针对「放一份商户 OpenAPI 就得到一条 MCP 路由」这个目标，下面是已定位、未解决的问题。
每条都带定位到文件的锚点。已解决的部分见 PR #1。

## 1. 多租户: 一份 spec 一条路由(未做)

现在的架构是「一个进程一份 spec」。三处挡着:

- `cmd/auto-mcp/main.go` 把 `config.EndpointConfig` 作为 **fx 单例**提供
  (`fx.Provide(func() *config.EndpointConfig { ... })`)，全进程共享一个 `BaseURL`、
  一种 auth、一个 token。
- `internal/server/server.go` 的 `Server` 只持有一个 `*mcpserver.MCPServer`，
  `setupTools()` 从 `cfg.SwaggerFile` 读**一份** spec。
- `internal/server/handler/http.go` 把 MCP handler 挂在 `mux.Handle("/")`，
  没有按商户区分的路径。

改动面比看上去集中: `NewHTTPRequester` / `NewHTTPAuthManager` / `NewHTTPRequestBuilder`
三个构造函数**已经是参数化的**，只是被 fx 喂了单例值。

注意:
- `mcp-go` 的 `StreamableHTTPServer` 挂子路径要用它自己的 endpoint path 选项，
  不要靠 mux 前缀推断。
- 商户下线时对应路由要能摘掉，不能只靠重启进程。

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

## 3. `outputSchema` 无法暴露(受阻于依赖)

`mcp-go v0.31.0` 的 `mcp.Tool` 只有 `Name` / `Description` / `InputSchema` /
`RawInputSchema` / `Annotations`，**没有 `OutputSchema` 字段**。因此 OpenAPI 的
响应 schema 目前完全没有被使用，调用方无法从工具定义知道能从结果里取到什么。

要么升级 mcp-go，要么改用 `NewToolWithRawSchema` 自己拼(那会绕过现有的 option 体系)。

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

## 7. 既有 lint 告警(未触碰)

`golangci-lint run ./...` 在 `internal/tui/main_page.go:118` 与 `:125` 报两处
`QF1012`(`WriteString(fmt.Sprintf(...))` 应为 `Fprintf`)。与本轮改动无关，
未一并修改以免扩大 diff。
