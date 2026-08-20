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

## 4. `oneOf` / `anyOf` 的转换是有损的(取舍待确认;两处缺陷已修)

`internal/parser/jsonschema.go` 的 `unionBranches` 把分支并成一个对象:属性取并集、
不标任何必填、分支要求写进 `description`。换来的是分支字段可达。

**为什么不忠实输出 `anyOf`**:实测过。拿一份三分支的支付 `oneOf` 喂给下游的 schema
拍平器,忠实输出得到 2 个 flag(`payment` 塌成一个 `string`,六个分支字段一个都碰不到,
且 exec 会把字符串塞进上游要对象的位置);合并输出得到 7 个 flag,路径全部正确。
「整支做成不透明 object」同样是 1 个不可用字段。所以取舍的实质是:**在"schema 说不清
约束但字段可达"与"schema 诚实但字段不可达"之间选了前者。**

仍然损失的(有意):

- 「恰好一支」这个约束本身。合并后没有机器可读的东西表达它,只剩 description 一句话。
- 条件必填降级为不必填。三支的必填求并集会变成"同时要求所有分支的字段",
  任何一次合法调用都会被判成缺参数,所以必填只能整体放弃。
- 字段的归属关系。调用方从 schema 看不出 `cardNo` 与 `walletId` 不能同时给。
- `oneOf`(恰好一支)与 `anyOf`(至少一支)被同样处理,description 里一律写
  "exactly one of",对 `anyOf` 是错的措辞。**这一条还没修。**

已修的两处(它们不是取舍,是错误):

- **同名属性不再"第一支胜",改为按facet 合并**。discriminator 模式下每一支都用
  单值 enum 声明选择器,先到先得会让发布出去的 schema 声称该属性只接受一个值,
  于是其余分支在调用方眼里根本不可达 —— 那不是信息变少,是信息变假。现在:
  enum 取并集;任一支不带 enum 则整个去掉(那一支接受任何值);type 冲突时发布
  类型列表;嵌套 required 取**交集**(并集会重犯"要求只有一支用得到的字段"那个错)。
- **`discriminator` 不再丢弃**,`propertyName` 写进 description(`selected by kind`)。
- **`not` 不再静默丢弃**,以一句 `must not be …` 记入 description。它无法在拍平后的
  schema 里表达,但静默丢掉意味着调用方只能从上游的拒绝里发现这条约束。

一个下游现象,记在这里以免以后被当成本仓的 bug 复现:**这两处修正对严格读 schema 的
客户端有效,但对 merchant-hub 那一侧看不出差别** —— 它的 `Flag` 模型不带 enum
(只取 `enum[0]` 当 example),且拍平时会丢掉中间对象节点的 `description`,所以
分支结构那句话到不了任何 flag。那是消费方的缺口,不是这里的。

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
