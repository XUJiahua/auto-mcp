# 使用手册

把一份 OpenAPI 文档变成一个 MCP server：auto-mcp 读文档、生成工具、把工具调用转成对上游
API 的 HTTP 请求。文档进，MCP 出，中间不经过模型。

本手册讲怎么用。配置项的逐项说明在 [CONFIGURATION.md](CONFIGURATION.md)，
已知边界与取舍在 [KNOWN_ISSUES.md](KNOWN_ISSUES.md)。

- [1. 五分钟跑起来](#1-五分钟跑起来)
- [2. 一个进程接多个 API](#2-一个进程接多个-api)
- [3. 不重启加 API](#3-不重启加-api)
- [4. 鉴权](#4-鉴权)
- [5. 裁剪与校订](#5-裁剪与校订)
- [6. 生成出来的工具长什么样](#6-生成出来的工具长什么样)
- [7. 运维](#7-运维)
- [8. 出错时怎么读](#8-出错时怎么读)
- [9. 明确不做的事](#9-明确不做的事)

---

## 1. 五分钟跑起来

```bash
go build -o auto-mcp ./cmd/auto-mcp
```

最小配置只需要两样东西：文档在哪、上游在哪。

```bash
AUTO_MCP_SWAGGER_FILE=openapi.yaml \
AUTO_MCP_ENDPOINT_BASE_URL=https://api.example.com \
AUTO_MCP_SERVER_MODE=http \
./auto-mcp
```

端点是 `http://localhost:8080/mcp`。三种传输：

| `--mode` | 用在哪 | 端点 |
|---|---|---|
| `stdio`（默认） | Claude Desktop 等本地客户端拉起子进程 | 标准输入输出 |
| `http` | 常规部署，Streamable HTTP | `/mcp` |
| `sse` | 需要长连接事件流的客户端 | `/mcp` |

配置可以来自 `config.yaml`、`AUTO_MCP_*` 环境变量、或 CLI flag，优先级是
**显式 flag > 环境变量 > 配置文件 > 默认值**。`config.yaml` 不是必须的。

只有三个 flag：`--mode`、`--swagger-file`、`--adjustment-file`。其余一律走配置文件或
环境变量（端口是 `AUTO_MCP_SERVER_PORT`，不是 `--port`）。

写成文件是这样：

```yaml
server:
  mode: http
  host: localhost # 默认；改成 0.0.0.0 就必须配鉴权，见第 4 节
  port: 8080
endpoint:
  base_url: https://api.example.com
swagger_file: openapi.yaml
```

跑起来以后，启动日志会告诉你接了什么：

```
Registered service {"route": "/mcp", "tools": 20, "schema_bytes": 5517,
                    "largest_tool": "addPet", "largest_tool_bytes": 610}
```

---

## 2. 一个进程接多个 API

每个 API 一条路由，`/mcp/{名字}`：

```yaml
server:
  mode: http

services:
  - name: hotel # → /mcp/hotel
    swagger_file: hotel.yaml
    endpoint:
      base_url: https://hotel.example.com
  - name: flight # → /mcp/flight
    swagger_file: flight.yaml
    endpoint:
      base_url: https://flight.example.com
```

每个 service 有自己的文档、自己的地址、自己的凭证，互不共享。进程里共用的只有监听端口
和前门鉴权。

几条规则：

- 名字是路由段，必须是单个路径段（`[a-zA-Z0-9][a-zA-Z0-9_-]*`），且不能重复。
  配了多个 service 时每个都必须有名字。
- 路由里的名字对不上任何 service 是 **404**，不是"一个空的 service"。
- `stdio` 只能服务一个 service —— 一根管道对一个客户端，没有可用于区分的地址。
  配多个会在启动时报错。
- 单个 API 用顶层 `swagger_file` 就好，端点留在 `/mcp`，不必凭空取名字。

---

## 3. 不重启加 API

把 `services` 换成一个目录，一个子目录就是一个 service：

```
services/
  hotel/
    openapi.yaml      # 也认 .yml/.json，以及 swagger.*
    service.yaml      # 可选：endpoint、upstream_security、adjustment_file
    adjustment.yaml   # 可选，不必指名
  flight/
    openapi.json
```

```yaml
services_dir: services
```

`service.yaml` 放文档表达不了的东西：

```yaml
endpoint:
  base_url: https://hotel.example.com
upstream_security:
  id: hotel_key
```

它**不能**设名字和文档路径 —— 那两项来自目录。一个能给自己所在目录改名的文件，会让路由
同时取决于两个地方。

加一个 API：放进目录，发信号。

```bash
mkdir -p services/flight
cp flight.yaml services/flight/openapi.yaml
kill -HUP $(pgrep -x auto-mcp)
```

```
Reloading configuration on SIGHUP
Registered service {"route": "/mcp/flight", "tools": 12, ...}
Updated service    {"route": "/mcp/hotel",  "tools": 20, ...}
Reload complete    {"services": ["flight", "hotel"]}
```

**已连接的客户端不会被断开。** 已有的 service 是原地更新的，工具增删时客户端收到
`notifications/tools/list_changed`，重新 list 一次就看到新工具 —— 这是 MCP 协议自己
对"工具集会变"的处理方式。

**重载失败什么都不改。** 所有 service 先全部构建完再整体换入，所以一份突然解析不了的文档
不会把正在服务的进程搞下线，失败只记日志。

目录里没有 OpenAPI 文档会**报错而不是跳过** —— 跳过会让一个名字写错的文档看起来像
"一个没有工具的 service"。

---

## 4. 鉴权

凭证描述一次，两个方向分别引用：

```yaml
security_schemes:
  - id: caller # 认调用方
    type: http
    scheme: bearer # basic | bearer
    default_credential: "${AUTO_MCP_DOWNSTREAM_TOKEN}"
  - id: upstream_key # 认我们自己（对上游）
    type: apiKey
    in: header # header | query
    name: X-API-Key
    default_credential: "${UPSTREAM_API_KEY}"

downstream_security: # 客户端 → auto-mcp
  id: caller
upstream_security: # auto-mcp → 上游 API
  id: upstream_key
```

`${VAR}` 从环境变量取，所以凭证不必写进文件。**引用了未设置的变量是启动失败**，不是空凭证
—— 空凭证到上游那里表现为权限错误，离真正的原因（变量名打错）很远。

两条启动时强制的规则：

1. **能从机器外访问的地址必须能认出调用方。** `server.host` 不是
   `localhost`/`127.0.0.1`/`::1` 时，必须配 `downstream_security` 或开 `oauth.enabled`。
   MCP 端点手上握着上游要的凭证，一个开放的端口等于把这些凭证借给任何能连上的人。
   `stdio` 没有监听套接字，豁免。
2. **一个 requirement 必须有可用凭证**：自己的 `credential`、scheme 的
   `default_credential`，或 `passthrough`。

多个 service 时，`security_schemes` 是全局的（scheme 描述凭证怎么携带，不描述属于哪个
上游），`upstream_security` 下沉到每个 service：

```yaml
security_schemes:
  - {id: hotel_key,  type: apiKey, in: header, name: X-API-Key, default_credential: "${HOTEL_KEY}"}
  - {id: flight_key, type: apiKey, in: header, name: X-API-Key, default_credential: "${FLIGHT_KEY}"}
services:
  - {name: hotel,  swagger_file: hotel.yaml,  endpoint: {base_url: "https://h.example.com"}, upstream_security: {id: hotel_key}}
  - {name: flight, swagger_file: flight.yaml, endpoint: {base_url: "https://f.example.com"}, upstream_security: {id: flight_key}}
```

### 透传调用方的凭证

```yaml
upstream_security:
  id: upstream_key
  passthrough: true
```

上游收到的是**调用方自己的凭证**，不是我们的。调用方没出示凭证时**直接报错，不会回落到
配置里的凭证** —— 那等于替一个匿名调用方送出平台的身份。

`downstream_security` 不能用 `passthrough`：再往上没有可转发的对象。

OAuth 2.1（含 PKCE、动态注册、GitHub/Google）是另一条路，见 [OAUTH.md](OAUTH.md)。

---

## 5. 裁剪与校订

校订文件按 `(路径, 方法)` 索引，做三件事：挑路由、改描述、裁响应。

```yaml
# 只暴露这些路由；不写这一段就是全都暴露
routes:
  - path: /pet/{petId}
    methods: [get]
  - path: /store/order
    methods: [post]

# 改写给模型看的说明
descriptions:
  - path: /pet/{petId}
    updates:
      - method: get
        new_description: "按 ID 查一只宠物，返回名字与状态"

# 裁剪上游响应
responses:
  - path: /pet/{petId}
    updates:
      - method: get
        prepend_body: "宠物："
        body: "{{ .name }}（{{ .status }}）"
        error_body: "上游拒绝：{{ .message }}"
```

```bash
./auto-mcp --swagger-file=openapi.yaml --adjustment-file=adjustment.yaml
```

响应裁剪值得单独说：`tools/list` 和每次调用的结果都进模型的上下文，而真实上游响应经常
巨大且噪声高（分页元数据、内部 traceId、几十个空字段）。实测一份 219 字节的响应裁到
19 字节。

- `body` 是 Go 模板，作用在**解析后的 JSON 响应**上。
- `prepend_body` / `append_body` 包裹结果，可以不配 `body` 单独使用。
- `error_body` 在上游报错时替代 `body` —— 解释失败的字段通常不是承载结果的字段。
- **套不上就原样返回响应**并记日志（语法错、响应不是 JSON、引用了不存在的字段）。
  模板是运营的便利，丢掉响应等于丢掉"上游到底说了什么"这唯一证据。
- **配了模板就不再发 `structuredContent`** —— 模板声明的是"调用方该看到什么"，
  再把未裁剪的整份发出去等于把刚删掉的放回去。

还有一个交互式的 TUI 可以生成这份文件：

```bash
go install ./cmd/mcp-config-builder
mcp-config-builder --swagger-file=openapi.yaml
```

---

## 6. 生成出来的工具长什么样

知道映射规则，才能预判文档改动会怎么影响工具面。

**工具名取 `operationId`**，没有才回落到 `method_path`（小写）。重名会加 `_2` 后缀而不是
互相覆盖。这一条要紧：很多 API 用 POST 做读操作，`post_api_queryhotelinfo` 这样的名字
抹掉了操作语义，而 `queryHotelInfo` 没有。

**`summary` 进 `title`**（截断 64 字符），`description` 用文档原文
（`description` → `summary`），两者都没有才回落到 `METHOD /path`。

**参数按位置投放，扁平成一层工具参数**：

| 文档里 | 工具参数 | 发出去 |
|---|---|---|
| `in: path` | 同名，恒为必填 | 替换进 URL 模板 |
| `in: query` | 同名 | 查询串；数组按 `explode` 展开成重复键或逗号连接 |
| `in: header` | 同名 | 请求头 |
| `in: cookie` | 同名 | `Cookie` 头 |
| `requestBody` | `body` | 按声明的 media type 编码 |

同名冲突（比如 query 和 header 都叫 `token`，或有个 query 参数叫 `body`）会把冲突方
改名成 `<位置>_<名字>`，并在描述里注明它对应上游哪个参数。**上游收到的名字不变。**

**请求体按声明的 media type 编码**：`application/json` 与 `*+json` 走 JSON，
`x-www-form-urlencoded` 走 url 编码，`text/*` 且 body 是字符串则原样发送。
产不出来的类型退回 JSON **并如实声明 JSON** —— 头永远描述实际发出的字节。

**annotation 只写 HTTP 方法真正保证的事**：GET 是只读+非破坏+幂等，DELETE 是破坏+幂等，
PUT 是幂等，**POST/PATCH 不带任何 annotation**。因为很多 API 用 POST 做读操作，标上
`readOnlyHint=false` 就是在陈述一件假事。

**响应 schema 会发布成 `outputSchema`**（取 2xx，`200` → `201` → `default`），
同时返回 `structuredContent`。顶层不是对象的响应（数组、标量）**不声明** —— MCP 要求
那里是对象，声明错的形状比不声明更糟。

**schema 保真度**：嵌套深度、数组 item schema、`description`/`example`/`default`/`enum`
和每一层的 `required` 都保留；`$ref` 内联展开（循环引用按指针身份剪断）；`allOf` 合并；
`oneOf`/`anyOf` 既合并成可达的 `properties`，也原样保留原关键字，两者交集就是文档陈述的
约束。`anyOf: [T, null]`（OpenAPI 3.1 表达可空的写法）折叠成 `T`。

---

## 7. 运维

### 文档必须合规

启动时按 OpenAPI 规范校验，不合规就拒绝加载并报出原因：

```
OpenAPI document does not conform to the specification: invalid components:
schema "Address": extra sibling fields: [exampleSetFlag types]
```

拒绝是有意的。一个非合规构造通常**不会响亮地失败，它只是没被读到**。实测一份 425 KiB 的
文档用非标准的 `types` 键声明了 854 个属性里的 220 个，照旧加载的话四分之一的 API 会以
"没有类型"的样子发布出去，而没有任何地方说这件事。

正则不在校验范围内：OpenAPI 规定用 ECMA-262 正则（支持 lookahead），而 Go 的正则引擎不
支持。用了 lookahead 的文档是**正确的**，拒绝它等于用本实现的限制否定一份合法文档。

支持 OpenAPI 3.0 / 3.1 / 3.2，以及 Swagger 2.0（自动转换）。

### schema 体积

每个 service 注册时报告体积。这是**持续成本**：`inputSchema` 每次 `tools/list` 都进模型
上下文。实测参照：

| 文档 | 工具数 | schema 总计 | 最大的单个工具 |
|---|---|---|---|
| petstore | 20 | 5.5 KiB | 610 B |
| 某平台 API | 87 | 51 KiB | 6.9 KiB |
| 某支付 API | 23 | 185 KiB | **35 KiB** |

需要护栏时：

```yaml
max_tool_schema_kib: 64 # 0（默认）= 不限
```

默认不限，因为 schema 大是成本而不是错误，多大算不可接受属于部署。超限在**启动时**被拒，
而不是在首次调用时；又因为重载是"全部构建完再整体换入"，一份长过上限的文档也不会把
正在服务的进程搞下线。

### 部署

```bash
docker run --rm -p 8080:8080 \
  -v $(pwd)/services:/server/services \
  -e AUTO_MCP_SERVER_MODE=http \
  -e AUTO_MCP_SERVER_HOST=0.0.0.0 \
  -e AUTO_MCP_DOWNSTREAM_TOKEN=... \
  ghcr.io/brizzai/auto-mcp:latest
```

容器里必须绑 `0.0.0.0` 才能通过发布端口访问，而这就触发第 4 节那条规则：必须配
`downstream_security`。细节见 [DOCKER.md](../DOCKER.md)。

日志走 `logging.output_path` 与 stdout。**`stdio` 模式下自动关闭控制台输出** ——
那条流是协议本身，往里写任何非协议内容都会打断它。

---

## 8. 出错时怎么读

| 症状 | 原因 |
|---|---|
| `swagger file is required` | 没给文档。用 `--swagger-file`、`AUTO_MCP_SWAGGER_FILE` 或配置文件里的 `swagger_file` / `services` / `services_dir` |
| `does not conform to the specification: ...` | 文档不合规，消息里带具体位置。修文档，不要指望这里将就 |
| `server.host ... is reachable from outside this machine but nothing authenticates callers` | 绑了非 loopback 地址却没配调用方鉴权。配 `downstream_security`、开 OAuth，或绑回 `localhost` |
| `references unset environment variable(s): X` | `${X}` 没设。**这是故意报错的** —— 空凭证到上游那里看起来像权限问题 |
| `has no credential: set its credential, give security scheme ... a default_credential, or use passthrough` | 引用了 scheme 但没有可用凭证 |
| `service "x" has no swagger_file` / `contains no OpenAPI document` | `services` 项缺文档；或 `services_dir` 的子目录里没有 `openapi.*`/`swagger.*` |
| `duplicate service name "x"` | 名字重复（含目录发现与显式列表撞名） |
| `stdio serves a single service, but N are configured` | `stdio` 只能一个。换 `http`/`sse` |
| `unsupported server mode "x"` | `mode` 只能是 `stdio`/`sse`/`http`。**以前这里是静默忽略** |
| `has a N KiB input schema, over the M KiB max_tool_schema_kib` | 超过配置的上限。用校订文件裁掉这个操作，或提高上限 |
| 调用返回 `missing required path parameter(s): x` | 必填 path 参数没给。以前会把 `{x}` 编码后发给上游，上游回一个谁也没想请求的 URL |
| 404 而不是工具列表 | 路由里的名字不匹配任何 service。用启动日志里的 `route` 值 |
| 日志里 `Query parameter value cannot be serialised; skipping` | 往 query/header 位置传了对象。OpenAPI 的对象序列化风格未实现，猜一种编码与正确编码在被上游拒绝前无法区分 |
| 日志里 `Request body media type is not supported; sending JSON` | 文档声明的是我们产不出的 media type（比如 XML）。发的是 JSON 并如实声明 JSON |
| 工具名是 `post_api_xxx` 这种 | 文档里那个操作没有 `operationId`。补上就会用它 |

配置排查从这里开始：

```bash
AUTO_MCP_LOGGING_LEVEL=debug ./auto-mcp --mode=http
```

---

## 9. 明确不做的事

- **不校验入参。** MCP 的 schema 是给客户端看的契约，auto-mcp 不在调用前按它检查参数。
  校验错误由上游给出。
- **不做字段/状态归一。** 上游的字段名、枚举、错误码原样透出。要归一就在消费方做。
- **不改请求体的形状。** 工具参数怎么给，body 就怎么发（`body.header` 里放签名字段是可行的
  ——那正是很多商户 API 需要的）。没有"请求模板"这一层：参数本来就是从同一份文档推导的，
  形状对不上的情况不出现。
- **不接受不合规文档。** 见第 7 节。
- **`oneOf`/`anyOf` 的约束不完全可机读。** 合并后的 `properties` 保证字段可达，
  原关键字保证约束可读，但两者并存意味着 schema 体积上升。取舍与量化见
  [KNOWN_ISSUES.md](KNOWN_ISSUES.md) 第 4 节。
