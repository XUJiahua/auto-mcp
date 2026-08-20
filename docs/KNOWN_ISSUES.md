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

## 4. `oneOf` / `anyOf` 的转换是有损的(取舍待确认;缺陷已修)

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
(`oneOf` 与 `anyOf` 的措辞混淆已修:两者仍按同样方式拍平——可达字段集合本来相同——但
分别描述为 "exactly one of" 与 "at least one of",且 `discriminator` 只在两者都存在时
也仍只提一次。)

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

## 6. 配置加载 —— 已解决(并修掉两个同区域的缺陷)

原问题:`config.Load()` 把 `viper.ReadInConfig()` 的错误无条件返回,所以没有
`config.yaml` 就直接启动失败:

```
Failed to load configuration: Config File "config" Not Found in "[. /etc/auto-mcp]"
```

而文档把 CLI flag 与 `AUTO_MCP_*` 环境变量描述为完整的配置方式。容器化部署里这条最碍事。

现在:文件缺失不再是错误(**解析失败仍然是错误** —— 静默退回默认值会启动一个无视运营
意图的服务);同时注册了一套默认值,否则"容忍文件缺失"只是把失败挪了个地方——进程会
用 0 端口、空服务名起来。默认 host 是 **loopback**:`/mcp` 自身没有鉴权(见 #2),
一个完全没配置就起来的进程不该能从机器外访问,对外暴露必须是运营显式设 host 的决定。

修这条的过程中实机验证暴露了两个同区域的缺陷,一并修了:

- **`--mode` 的默认值覆盖了其他所有来源。** flag 默认值 `"stdio"` 非空,而那段覆盖
  是无条件的,于是配置文件里的 `server.mode` 与文档承诺的 `AUTO_MCP_SERVER_MODE`
  **从来没起过作用** —— 不显式传 `--mode` 就一定是 stdio(本仓自带的 `config.yaml`
  写着 `mode: sse`,也一样无效)。改为把 flag 绑定到配置键,由 viper 施行
  「显式 flag > 环境变量 > 文件 > 默认值」;viper 只在 flag 真被改过时才优先它。
  另外:非法的 mode 以前被静默忽略,现在启动即失败。
- **`--adjustment-file` 是文档里的写法,代码注册的是 `--adjustments-file`。**
  文档与 README 共 6 处用单数形式,照抄任何一条都会 `unknown flag`。现在两种写法都接受。
- **`AUTO_MCP_ENDPOINT_BASE_URL` 读不到。** `AutomaticEnv` 不足够:`Unmarshal` 只会
  查 viper 已知的键,一个既没有默认值、也不在配置文件里、也没有绑定 flag 的键,
  `Get` 读得到但进不了结构体。文档里承诺可用环境变量设置的键现在都显式 `BindEnv`。

仍然不支持:**map 类型的键无法用环境变量设置**,即文档里的
`AUTO_MCP_ENDPOINT_HEADERS_X_CUSTOM`。viper 没有从环境变量构造 map 的机制,要支持
就得自己扫 `AUTO_MCP_ENDPOINT_HEADERS_*` 前缀。目前用配置文件设置 `endpoint.headers`。

## 7. 既有 lint 告警 —— 已解决

`internal/tui/main_page.go` 的两处 `QF1012` 已改为 `fmt.Fprintf`。
`golangci-lint run ./...` 现在是 0 issues。

## 8. Go 版本要求提高到 1.25

官方 SDK 的 `go.mod` 声明 `go 1.25.0`，因此本仓的 go 指令从 1.24.1 提到 1.25.0。
`.github/workflows/*.yml` 与 `Dockerfile` 里固定的版本已同步更新。使用旧工具链构建的
下游需要一并升级。

## 9. 请求构造的三个缺陷 —— 已解决

读 agentgateway 与 higress 的 OpenAPI 处理时对照出来的,都经实机确认:

- **同一参数在 path item 与 operation 两级声明时重复入表。** 参数的身份是
  (name, location) 而不是 name(同名可以既是 query 又是 header)。原来把两份列表直接
  拼接,于是 required 里出现重复项,而重复声明的 query 参数会**在线上发两遍**
  (`?locale=x&locale=x`)。现在按 (name, location) 合并,后声明的(operation 级)更具体、
  优先。参照 agentgateway 的 `(name, ParameterType)` 去重。
- **`Content-Type` 硬编码 `application/json`。** 一个只声明
  `application/x-www-form-urlencoded` 的接口,body 被当 JSON 发出、头也声称是 JSON。
  现在按声明的 media type 选择编码:json 系列走 JSON,`x-www-form-urlencoded` 走
  url 编码,`text/*` 且 body 是字符串则原样发送,其余无法产出的类型退回 JSON **并如实
  声明 JSON**——头永远描述实际发出的字节,而不是声明。没有 body 的请求不再声明任何
  Content-Type。另外:多个 media type 并存时不再把各自的 properties 混合成一个 body
  (那会拼出一个没有任何 content type 接受的形状),而是确定性地选一个。
- **query / header 位置的对象值被 JSON 编码塞进去。** 现在跳过并告警。OpenAPI 的对象
  序列化风格(deepObject 等)没有实现,而猜一种编码与正确编码在被上游拒绝之前无法区分。
  参照 agentgateway 的 warn + skip。

## 10. 工具展示名与参数名冲突 —— 已解决

- **`title` 从不设置,`description` 被地址信息挤占。** 原来每个工具的 description 都是
  `"POST /api/queryHotelInfo \n 真正的说明"`,而 MCP 的 `Title` 字段一直空着。方法与路径是
  调用方无法据以行动的寻址细节,却挤在模型每次都要读过去的第一行。现在 `summary` 进
  `Title`(截断 64 字符),`description` 用文档原文(`description` → `summary`),两者都没有
  的操作才回落到 `METHOD /path`。参照 agentgateway 的 `with_title`。
- **参数名在扁平命名空间里静默互相覆盖。** 工具参数共享一个命名空间,而 OpenAPI 参数只在
  各自 location 内唯一,且请求体占用了 `body` 这个名字。实测:一个同时声明 query 与 header
  同名参数、外加一个名为 `body` 的 query 参数的接口,**三个参数里有两个凭空消失**。现在
  冲突方改名为 `<location>_<name>`,`ParamConfig.ArgName` 记住"从哪个参数读",`Name` 仍是
  上游要的名字,并在 description 里注明映射关系。

选择保留扁平命名空间(higress 亦是扁平 + `Position` 标注)而不是像 agentgateway 那样按
location 分组成 `{body,header,query,path}` 四层:扁平对模型更友好,下游拍平也更自然。
分组能天然免疫这类冲突,是这个选择的代价,现在用改名 + 检测来补。
