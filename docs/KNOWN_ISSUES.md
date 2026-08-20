# Known issues

针对「放一份商户 OpenAPI 就得到一条 MCP 路由」这个目标，下面是已定位、未解决的问题。
每条都带定位到文件的锚点。已解决的部分见 PR #1。

MCP 库已从 `github.com/mark3labs/mcp-go` 换成官方 `github.com/modelcontextprotocol/go-sdk`，
本文件中受此影响的条目已相应更新。

## 1. 多租户: 一份 spec 一条路由 —— 已解决

配置里列 `services`,每个 service 得到自己的 MCP 端点 `/mcp/{name}`:

```yaml
services:
  - name: hotel                       # → /mcp/hotel
    swagger_file: hotel.yaml
    endpoint: { base_url: "https://hotel.example.com" }
    upstream_security: { id: hotel_key }
  - name: flight                      # → /mcp/flight
    swagger_file: flight.yaml
    endpoint: { base_url: "https://flight.example.com" }
    upstream_security: { id: flight_key }
```

实测(两个 service、两个上游、两把不同的 key):

```
/mcp/hotel   server="auto-mcp/hotel"   tools=[getHOTEL]   上游 9971 收到 X-API-Key: HK-1
/mcp/flight  server="auto-mcp/flight"  tools=[getFLIGHT]  上游 9972 收到 X-API-Key: FK-2
/mcp/nope    404
```

实现要点:

- **每个 service 一套 parser + requester + `mcp.Server`**,不共享。parser 持有它读过的
  文档,requester 持有它要发往的地址与凭证 —— 共享任何一个都会让某个上游的配置替另一个
  作答。进程里只共享监听套接字与前门鉴权。
- 路由挂点就是上一轮换库时留下的那个:官方 SDK 的
  `mcp.NewStreamableHTTPHandler(getServer, ...)` 按请求解析 server,
  `Server.serverForRequest` 从 `/mcp/{name}` 取出名字查表。
- **单 service 形式保留**,端点仍在 `/mcp`,地址不变,`stdio` 也只在这种形式下成立
  (一根管道对一个客户端,没有可用于区分 service 的地址)。
- **未知 service 是 404 而不是空工具列表** —— SDK 对解析不出的 server 回 400
  "no server available",而地址写错应当是 404;否则一个拼错的路由看起来像"一个存在
  但什么都没有的 service"。
- `security_schemes` 保持全局(scheme 描述凭证怎么携带,不描述属于哪个上游),
  `upstream_security` 下沉到每个 service。

fx 的图随之简化:`parser.Module` 与 `requester.Module` 删除,`Server` 自己按 service 构造。
`server_test.go` 里那个注入 `mockParser` 断言"server 调了 parser.Init"的测试也删了 ——
注入点不存在了,而它的意图现在由 routing 测试用可观察结果覆盖。

## 2. `/mcp` 无鉴权 —— 已解决

原问题:OAuth 关闭时 `internal/server/handler/http.go` 是 `mux.Handle("/", mcpHandler)` ——
`/mcp` 裸奔,而这个进程持有上游所需的凭证,等于把凭证借给任何能连上该端口的人。

现在照 higress 的形状引入了**两侧分离**的 security 模型(`internal/config/security.go` +
`internal/security/`):凭证一次性描述为 `security_schemes`(`apiKey` header/query、
`http` basic/bearer),再由 `downstream_security`(client → 本服务)与
`upstream_security`(本服务 → 上游)分别引用。凭证支持 `${VAR}` 环境变量插值,
引用了未设置的变量是启动失败而不是空凭证。

**启动时强制两条**:
- 绑定地址可从机器外访问(`server.host` 不是 localhost/127.0.0.1/::1)时,必须配
  `downstream_security` 或开 OAuth,否则启动失败。`stdio` 无监听套接字,豁免。
- 一个 requirement 必须有可用凭证(自己的 `credential`、scheme 的
  `default_credential`,或 `passthrough`)。

`upstream_security` 支持 `passthrough: true`(把调用方自己的凭证转发给上游)。它**不会**
回落到配置里的凭证 —— 那等于替一个没出示凭证的调用方送出平台的身份。

`endpoint.auth_type` / `auth_config` 已删除 —— 原型阶段不背兼容包袱,两条并行的鉴权路径
是最容易腐烂的东西。不需要凭证的上游靠"什么都不配"表达,而不是写 `auth_type: none`。

一处代价:`examples/petshop/config/config.yaml` 用的是 `host: 0.0.0.0`(容器里必须如此),
因此示例现在需要 `AUTO_MCP_DOWNSTREAM_TOKEN`。这是那条强制规则的直接后果。

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

## 4. `oneOf` / `anyOf` —— 已解决

`internal/parser/jsonschema.go` 现在**两者都发**:合并后的 `properties`,加上原样保留的
原关键字(`oneOf` 仍是 `oneOf`,`anyOf` 仍是 `anyOf`)。

这两个关键字在 JSON Schema 里独立且同时生效,交集恰好是文档陈述的约束:合并后的
`properties` 描述每个字段的形状、保住可达性;分支列表让约束可机读。

**为什么不是只发一种**(同一份三分支支付 `oneOf`,实测):

| 形态 | 字节 | 下游可用 flag | 「恰好一支」可机读 |
|---|---|---|---|
| 只发合并 | 710 | 7,路径全对 | ✗ 只剩 description |
| 只发 `anyOf` | 808 | 2(`payment` 塌成 string) | ✓ |
| 只发不透明 object | 252 | 2(`payment` 是 object) | ✗ |
| **两者都发(现状)** | **1282** | **7,路径全对** | **✓** |

只发合并会把「恰好一支」降级成一句话;只发分支会让每个分支字段对不实现组合的消费方
不可达(实测下游把 `payment` 压成一个 `string`,六个字段一个都碰不到)。两者都发的代价
只有字节。

原先记为「四条有意取舍」的四项(约束不可机读、条件必填降级、字段归属关系丢失、
`oneOf`/`anyOf` 同样拍平)**全部随之恢复** —— 它们本是同一个根因(只发出合并后那一个对象)
的四个表现,不是四个独立取舍。

**分支要转换两次,不能复用合并用的那批。** 合并会就地放宽 facet(discriminator 的 enum
变成各分支取值的并集),而且合并是把分支自己的属性 map **按引用**插进去的。共用会把
发布出去的分支一起放宽,而一个选择器接受所有取值的分支已经不再选中它自己 ——
那会让发出去的约束变成任何值都满足。测试专门钉了这条。

剩下的代价:

- **体积 +81%**(710 → 1282)。`tools/list` 进模型上下文,这是真实 token 成本。
- 共享分支被内联两次,所以 +81% 是这个小例子上的下限;**深层嵌套多态会放大到多少没有实测**
  (手头没有真实商户 spec)。真实 spec 上体积成问题时,退一步是只发合并 + 把分支归属
  写进各字段的 description。

## 5. spec 注册 / 热加载 —— 已解决

两部分。

**目录发现**:`services_dir` 下每个子目录是一个 service,名字取自目录名:

```
services/hotel/openapi.yaml      # 或 .yml/.json、swagger.*
services/hotel/service.yaml      # 可选:endpoint、upstream_security、adjustment_file
services/hotel/adjustment.yaml   # 可选,不必指名
```

`service.yaml` 承载文档表达不了的东西(发往哪、用哪把凭证),但**不能设名字与文档路径**
—— 那两项来自目录;一个能给自己所在目录改名的文件会让路由同时取决于两个地方。
目录里没有 OpenAPI 文档是**报错而不是跳过**:跳过会让一个名字写错的文档看起来像
"一个没有工具的 service"。

**SIGHUP 重载**:重新扫描目录并让运行中的 server 与之一致。

```
kill -HUP $(pgrep auto-mcp)
```

关键决定:**已存在的 service 原地更新,不换 `mcp.Server`**。于是打开的会话保持连接,
并在工具增删时收到 `notifications/tools/list_changed` —— 这是协议自己对"工具集会变"
给出的答案,所以重载既不需要断开客户端,也不会让客户端停在一份已不存在的列表上。
(我原本准备让人在"旧会话继续用旧工具集"与"主动断开重连"之间选,查过 SDK 后发现
`AddTool`/`RemoveTools` 会 `changeAndNotify`,这个选择不必做。)

**重载失败则什么都不改**:所有 service 先全部构建完再整体换入,因此一份突然解析不了的
spec 不会把一个正在正常服务的进程搞下线,失败只记日志。

实测(进程不重启):

```
启动          Registered service {"route":"/mcp/hotel", "tools":1}
              /mcp/flight → 404
放入目录+HUP   Reloading configuration on SIGHUP
              Registered service {"route":"/mcp/flight", "tools":1}
              Updated service    {"route":"/mcp/hotel",  "tools":1}
              Reload complete    {"services":["flight","hotel"]}
              /mcp/flight → getFlight/查询机票 → 上游 9962
```

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

## 11. 兼容层清理

原型阶段允许破坏性更新,因此为向后兼容留下的并行路径已删除:

- **`endpoint.auth_type` / `endpoint.auth_config`**(连同 `config.AuthType` 与
  `HTTPAuthManager` 里那个 switch)。上游鉴权只有 `security_schemes` +
  `upstream_security` 一条路。两套并行的鉴权实现是最容易腐烂的东西。
- **`--adjustments-file` 复数拼写**。全线统一为单数:flag `--adjustment-file`、
  配置键 `adjustment_file`、环境变量 `AUTO_MCP_ADJUSTMENT_FILE`、字段 `AdjustmentFile`。
  原来是文档单数、flag 复数、环境变量复数,三处不一致。
- **`MethodConfig.QueryParams`**。已被带 location 的 `Params` 取代,留着只会让"查询参数
  有几个"这个问题有两个答案。

## 12. 响应裁剪(borrow from higress)—— 已实现

上游响应原样返回给调用方,而真实商户响应经常巨大且噪声高(分页元数据、内部 traceId、
几十个空字段),这些全都进模型的上下文。higress 的 `responseTemplate` 是这一层的现成设计。

实现挂在 **adjustment file** 上 —— 那里已经是「人工校订」的通道(选路由、改描述),
按 (path, method) 索引:

```yaml
responses:
  - path: /api/hotel
    updates:
      - method: GET
        prepend_body: "Hotel: "
        body: "{{ .bussinessResponse.hotelName }} ({{ .bussinessResponse.starRate }}星)"
        error_body: "upstream refused: {{ .returnMsg }}"
```

实测:219 字节的上游响应裁到 19 字节。

几条边界:
- 模板在注册工具时解析一次,语法错误在启动时报,而不是每次调用。
- 无法套用(语法错、响应非 JSON、引用了不存在的字段)时**原样返回响应**并记日志 ——
  模板是运营的便利,丢掉响应等于丢掉"上游到底说了什么"的唯一证据。
- 配了模板就**不再发 `structuredContent`**:模板声明的是"调用方该看到什么",
  再把未裁剪的整份作为结构化内容发出去,等于把刚删掉的东西放回去。
- `error_body` 与 `body` 分开,因为解释失败的字段通常不是承载结果的字段。
  `prepend/append` 只作用于成功路径 —— 它们描述的是一个结果,而失败不是。

higress 的 `requestTemplate`(URL/Header/Body 模板 + `argsToJsonBody` 等开关)**没有实现**。
它解决的是"上游要的 body 形状与工具参数不是 1:1",而 auto-mcp 从 OpenAPI 直接推导时这种
错配不出现;真需要时再补。
