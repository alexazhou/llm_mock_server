# 配置驱动 Go Mock Server — 配置参考

> 状态：设计稿，尚未实施。本文定义 server.yaml、剧本 YAML 和系统 key 的配置契约；系统实现见 [技术设计](./step2_technical.md)，Steps 字段见 [Output Steps 专项设计](./output_steps_design.md)。

## 1. 剧本主体结构

最外层 YAML 只表达剧本结构。下面的 `<...>` 都是类型占位符，不是具体规则或可直接运行的值。

```yaml
version: <schema-version>
id: <script-id>

config: <Config>
assert: <AssertionList>

rounds:
  - input: <Input>
    output: <SingleOutput>
    # 或者使用 outputs: <ConditionalOutputList>
```

普通 round 使用单个 output；input 省略时，结构为：

```yaml
rounds:
  - output:
      config: <OutputConfigOverrides>
      content: <Content>
      reasoning: <Reasoning>
      tool_calls: <ToolCalls>
      error: <ProtocolError>
      steps: <OutputSteps>
```

需要按条件返回多个结果时，改用 outputs；output 和 outputs 不能同时出现：

```yaml
rounds:
  - input:
      assert: <AdditionalAssertionList>
    outputs:
      - when: <ConditionList>
        config: <OutputConfigOverrides>
        content: <Content>
        reasoning: <Reasoning>
        tool_calls: <ToolCalls>
        error: <ProtocolError>
        steps: <OutputSteps>
```

一份文件只定义一个剧本。服务端根据 id 生成三个协议 URL，剧本不声明 protocol、URL、path 或 route。请求访问的 URL 唯一决定协议。顶层 config 提供公共配置，assert 提供公共输入断言。round 定位和 output 匹配分别在第 6、7 章定义。

必填字段只有 version、id 和非空 rounds。顶层 config 和 assert 都可以省略。每个 round 必须包含 output 或非空 outputs，input 可省略。input 只负责校验已定位 round 的请求，不参与 round 或 output 选择；需要返回空 body 时写 output: {}。

第一版只接受整数 `version: 1`。id 必须匹配 `^[a-z0-9][a-z0-9_-]{0,63}$`，并且在整个 scripts_dir 中唯一；文件名和目录名不参与 ID。所有配置对象都使用严格字段集合，未知字段和重复 YAML key 在加载时直接拒绝。YAML anchor、alias、merge key 和自定义 tag 不属于 DSL，出现时同样拒绝。

input 只允许 assert。output 允许 config、content、reasoning、tool_calls、error 和 steps；input、when、error、steps 字段都可以省略。outputs 中每一项还可以包含 when。content、reasoning、tool_calls 和 error 定义返回内容；steps 只控制被选中 output 的执行过程。

完整配置示例见 [server.yaml](./examples/server.yaml) 和 [scripts](./examples/scripts/)。

## 2. 顶层结构的职责

### 2.1 剧本字段

| 结构 | 只负责什么 | 不负责什么 |
|---|---|---|
| id | 剧本标识和自动 URL | 不表示模型名或协议 |
| config | 请求运行限制，以及所有 output 共用的输出默认值 | 不包含断言或输出内容 |
| assert | 使用系统 key 检查本轮输入 | 不选择分支或跳轮 |
| round.input | 可选；校验已经根据 output_count 定位到的请求 | 不参与 round 选择，不允许返回内容 |
| round.output | 无条件单输出的简写 | 不与 outputs 同时使用 |
| round.outputs | 按系统 key 条件选择的候选输出列表 | 不负责选择 round |
| rounds | 按请求中已有模型输出数量定位唯一 round | 不按调用次数推进 round |

顶层只保留配置和断言两个公共结构：

```yaml
config: <Config>
assert: <AssertionList>
```

config 使用一层字段，不再嵌套 request 或 response。assert 中不会出现超时、状态码、响应头或流式选项。

### 2.2 服务配置与剧本配置

`server.yaml` 只配置进程和所有剧本的服务级默认值，不定义剧本、URL 或响应内容：

| 字段 | 含义 |
|---|---|
| version | server schema 版本；第一版只能为整数 1 |
| listen | TCP 监听地址，默认 `127.0.0.1:9999`；端口必须为 1～65535，第一版拒绝端口 0；容器或远程访问需要显式改为其他地址 |
| scripts_dir | 剧本目录，相对于 server.yaml |
| config | 剧本 config 的服务级默认值 |
| transport.read_header_timeout | HTTP 请求头读取超时 |
| runtime.max_active_requests | 最大活动剧本请求数 |
| runtime.max_output_bytes | 单次请求全部有限 send 计划允许生成的最大字节数，包含正常输出、error、heartbeat 和 raw |
| runtime.max_fixture_bytes | 单个 raw fixture 最大字节数 |
| runtime.max_generated_events | 单次请求最大协议记录数 |
| runtime.max_write_units | 单次请求经过 chunks 后允许执行的最大 Write/Flush 单元数 |
| runtime.shutdown_timeout | 服务关闭时等待活动请求退出的上限 |
| capture.capacity | 内存 capture 最大记录数 |
| capture.max_body_bytes | capture 保存的单个请求 body 上限 |
| capture.max_total_bytes | capture 窗口保存的 body、headers 和诊断预览总字节上限 |

server.yaml 只有整数 `version: 1` 必填。其他字段省略时使用 [示例 server.yaml](./examples/server.yaml) 中的值；scripts_dir 默认相对于 server.yaml 的 `scripts`。所有 max/capacity 字段必须为正整数，只有 capture.max_body_bytes 可以为 0（表示不保存请求 body）。read_header_timeout 和 shutdown_timeout 必须是非负 duration；0s 分别表示关闭请求头读取超时和不等待活动请求退出。

剧本 URL 固定由 id 和协议生成，不提供可配置 URL prefix。config 的字段回退顺序是：剧本顶层 config → server.config → 内置默认值；其他 server.yaml 结构不能在剧本中覆盖。

scripts_dir 按相对路径字典序递归加载扩展名为 `.yaml` 或 `.yml` 的普通文件，不跟随符号链接；其他文件只可能被 raw.fixture 显式引用。每个被扫描到的 YAML 都必须是一个完整剧本，不能在同一文件中放多个 YAML document。

第一版日志固定输出 JSONL，HTTP write timeout 固定关闭，以保证 infinite wait 只由客户端取消或服务关闭结束；两者都不暴露只有单一有效取值的配置。

### 2.3 协议 URL

每个剧本固定生成三个标准形态的协议 URL：

| URL | request.protocol | 行为 |
|---|---|---|
| `/mock/{id}/v1/chat/completions` | openai | 按 OpenAI Chat 协议解析请求并封装响应 |
| `/mock/{id}/v1/messages` | anthropic | 按 Anthropic Messages 协议解析请求并封装响应 |
| `/mock/{id}/v1/responses` | responses | 按 OpenAI Responses 协议解析请求并封装响应 |

框架只根据 URL 选择协议，不检查 header 或 JSON 字段来猜测协议。请求 body 不符合 URL 对应协议时返回 `mock_protocol_invalid`，不尝试其他适配器，也不执行 output。三个入口共用 rounds 和配置，但分别维护 request.count。

## 3. 系统内置 key

系统 key 由框架提供，剧本不能创建、修改或覆盖。key 读取结果保留原始类型，不进行字符串插值。

第一版提供以下只读 key：

| key | 类型 | 参数 | 含义 |
|---|---|---|---|
| request.method | string | 无 | HTTP method |
| request.path | string | 无 | 实际请求 path |
| request.header | string / string[] / missing | field、可选 mode | 大小写不敏感地读取 header |
| request.headers | object | 无 | 全部请求头的只读副本 |
| request.query | string / string[] / missing | field、可选 mode | 读取 query 参数 |
| request.body | string | 无 | 通过严格 UTF-8 校验后的原始请求 body 文本 |
| request.json_body | any / missing | 可选 path | 按简单 JSONPath 读取解析后的完整请求 JSON body |
| request.round_input | array / any / missing | 可选 path | 本轮新增输入（最后一个历史模型输出之后的输入项列表；可配合 path 取值） |
| request.model | string | 无 | 框架自动解析的模型名；普通剧本无需断言 |
| request.stream | boolean | 无 | 协议解析后的 stream 值；请求省略时为 false |
| request.output_count | integer | 无 | 当前请求对话历史中已经包含的模型输出轮数 |
| request.count | integer | 无 | 当前剧本收到的第几次已接受请求，从 1 开始；输入断言通过后才产生 |
| script.id | string | 无 | 当前剧本 id |
| request.protocol | string | 无 | 当前请求实际使用的 openai / anthropic / responses 协议 |
| round.index | integer | 无 | 本次定位到的 round 序号，等于 request.output_count + 1 |

header/query 的 mode 默认为 single；实际出现多个值时报告 key 读取错误。mode=values 返回有序字符串数组。request.headers 使用小写 header name 映射到有序字符串数组，不合并重复值。

`request.json_body` 和 `request.round_input` 支持可选的独立 `path` 参数（简单 JSONPath 子集）：
- `path` 省略、为空字符串或为 `$` 时，返回该数据源的根值（完整 JSON body 或完整的 `round_input` 数组）。
- 支持根标识 `$`（可省略，`$.messages[0].role` 与 `messages[0].role` 等价）。
- 支持对象字段访问 `.field` 与括号键名 `['special.field']`。
- 支持数组正数下标 `[0]`、负数下标 `[-1]`（从末尾倒数，`-1` 为最后一项）。
- 支持数组通配投影 `[*]`（遍历当前数组元素并继续后续路径，将命中的元素按顺序聚合为新的一维数组；若当前值不是数组或中间路径缺失则返回 `missing`）。
- 不支持递归下降 `..`、切片 `[m:n]` 或谓词过滤表达式 `[?()]`，非法语法在启动加载时直接报错。字段或下标不存在时返回内部 `missing` 值，与 JSON `null` 严格区分。

`request.round_input` 由协议解析器在计算 `request.output_count` 时同步提取，直接对应**客户端在本轮新提交的输入项列表**（即最后一个历史模型输出之后的所有输入项）：
- **第 1 轮（`output_count == 0`）**：尚无历史模型输出，`request.round_input` 为去除前置系统指令（OpenAI Chat 與 Responses `input` 中位于首轮开头的 `role: system` / `role: developer` 消息；Anthropic 的顶层 `system` 与 Responses 的顶层 `instructions` 本就不在消息列表内）后的全部初始输入项（Responses 若 `input` 为纯字符串则归一化为单元素数组 `[{"role": "user", "content": input}]` 或原字符串数组 `[input]`）。这样三个协议在第 1 轮的 `$[0]` 均统一对应第一条用户输入项。完整未过滤的原始消息数组始终可通过 `request.json_body`（如 `path: $.messages` 或 `path: $.input`）读取。
- **第 N 轮（`output_count >= 1`）**：截取最后一个历史模型输出（OpenAI/Anthropic 的最后一条 `role: assistant` 消息；Responses 的最后一组模型输出项 `reasoning` / `assistant message` / `function_call`）**之后**的所有输入项子数组（例如本轮回传的 `tool` / `tool_result` / `function_call_output` 或新一轮 `user` 消息）。

剧本协议请求 body 必须是合法 UTF-8 JSON。非法 UTF-8 在 JSON 解析前直接返回 `mock_invalid_json`；框架不会用替换字符修复，因此 request.body 与 request.json_body 始终来自同一份文本。

request.protocol 直接来自命中的协议 URL。request.model 由对应的请求解析器自动提取、校验，并由响应适配器自动回填。它对 content、日志和内部适配器可读，不需要在每份 YAML 中重复配置或断言。

key 读取结果可能是内部 missing 值。default 仅在 missing 时替换读取结果；null、false、0 和空字符串不会触发 default。missing 的处理取决于使用位置：exists/absent 直接检查实际值；其他 assert 操作符的实际值或 expected 引用为 missing 时断言失败；when 任一侧为 missing 时当前候选不匹配；动态输出引用遇到 missing 且没有 default 时返回 `mock_output_prepare_error`。

### 3.1 request.count 的定义

request.count 是按 script.id 和 request.protocol 隔离的服务端计数器，不按 round 或客户端 IP 拆分。同一个剧本的三个协议 URL 各自从 1 开始，并按以下时点分配：

1. 请求已经完成 body 读取和协议解析。
2. request.output_count 已经定位到有效 round。
3. 顶层 assert 和该 round 的 input.assert 全部通过。
4. 框架原子递增该剧本的计数器，并把新值写入 request.count。
5. 使用 request.count 和其他系统 key 匹配 output，然后执行被选中的 output。

读取失败、协议解析失败、round 不存在或输入断言失败的请求不计数。计数分配后，即使没有 output 匹配、输出返回错误、连接挂起或以 `end.mode: abort` 强制断连，该次数也已经消耗。同一剧本、同一协议的并发请求获得互不重复的 count，但响应完成顺序不保证与 count 一致；依赖精确次数的测试不得并发调用同一个协议 URL。

只有进入三个 `/mock/{id}/v1/...` 协议 URL 之一并通过上述准备阶段的剧本请求参与计数。health、capture、counter 管理请求、全局 OPTIONS 和未匹配路径都不参与任何剧本的 request.count。

三个协议计数器在进程启动并加载剧本时都为 0。counter reset 一次重置该剧本的三个计数器，并且只允许在该剧本没有活动请求时执行，避免生成重复 count。capture clear 与 counter reset 相互独立。管理接口及并发实现由技术设计定义。

允许动态值的输出字段也可以直接引用系统 key。例如把请求中的 cached_tokens 写入 usage：

```yaml
cached_input_tokens:
  key: request.json_body
  path: $.cached_tokens
  default: 0
```

这只是系统值引用，不创建变量，也不允许赋值。

## 4. 输入断言

assert 是断言列表。每一项直接指定系统 key、该 key 的可选参数、操作符和期望值。name 是可选的诊断名称，与 key 并列；expected 可以是字面值，也可以是另一个系统 key 引用。

```yaml
assert:
  - name: <optional-name>
    key: <system-key>
    <key-argument>: <value>
    op: <operator>
    expected: <literal-or-system-key-reference>
```

不需要 expected 的操作符可以省略该字段：

```yaml
assert:
  - name: <optional-name>
    key: <system-key>
    <key-argument>: <value>
    op: <exists | absent>
```

第一版操作符：

| op | 实际值要求 | expected |
|---|---|---|
| exists / absent | 任意系统 key | 不需要；检查 missing 状态 |
| eq / ne | 任意 JSON 值 | 字面值或另一个系统 key，严格类型比较 |
| type | 任意值 | null / boolean / number / string / array / object |
| contains | string 或 array | 子串或数组元素 |
| regex | string | Go RE2 正则字符串 |
| gt / gte / lt / lte | number | 精确十进制 number |
| length_eq / length_gte / length_lte | string / array / object | 非负整数 |

name 只用于日志和断言失败诊断，不参与读取、比较或继承。名称在同一个 assert 列表中必须唯一；省略 name 时，诊断使用 assert[0]、assert[1] 等列表位置。

exists 和 absent 必须同时省略 expected 与 default，直接检查原始 missing 状态。其他操作符可以使用 default；default 替换 missing 后再执行类型检查和比较。

length 操作符对 string 统计 Unicode code point，对 array 统计元素，对 object 统计 key；不按 UTF-8 字节数计算。字节长度只出现在 max_body_bytes、fixture 上限和 Output Steps 的 chunks 中。

请求 JSON number 和 YAML 中的数字字面量都使用原始十进制文本解析，不先转换为 float64。数值比较和动态 usage 的整数校验不得因 IEEE-754 转换丢失精度。

eq/ne 对 array 保留顺序，对 object 忽略 key 声明顺序并递归比较，对 number 比较精确数值，因此 1 与 1.0 相等；不同 JSON 类型始终不相等。contains 检查 array 元素时复用同一相等规则。

顶层 assert 是所有请求必须满足的公共断言。框架先根据 request.output_count 定位 round，再执行顶层 assert 和该 round 可选的 input.assert。input 或 input.assert 省略、以及 assert: []，都表示该 round 不增加局部断言。expected=null 是合法期望值；assert=null 无效。

request.count 在这些输入断言通过后才分配，因此不能作为顶层 assert 或 input.assert 的 key，也不能出现在 expected 引用中。它可用于 outputs.when、允许系统 key 引用的输出字段、日志和 capture。

任一断言失败时返回输入断言错误，不执行 output。input.assert 不参与 round 选择，因此同一个请求不会因为断言失败而尝试其他 round。

例如，检查 header 中的租户与 query 中的租户一致：

```yaml
assert:
  - name: tenant_matches
    key: request.header
    field: X-Tenant
    op: eq
    expected:
      key: request.query
      field: tenant
```

default 与 key 参数处于同一级：

```yaml
assert:
  - key: request.header
    field: X-Tenant
    default: public
    op: eq
    expected: public
```

## 5. Config 与继承

### 5.1 顶层 Config

顶层 config 同时提供请求运行限制和公共输出默认值，所有字段直接位于 config 下，不增加 request/response 包装层：

| 字段 | 类型 / 内置默认值 | 含义 |
|---|---|---|
| config.max_body_bytes | 正整数，8388608 | 当前请求 body 接收上限 |
| config.read_body_timeout | 非负 duration，15s | 当前请求 body 的读取时限；0s 显式关闭 |
| config.stream | auto / boolean，auto | auto 使用 request.stream；true/false 强制生成对应模式 |
| config.status | 200～599 integer，200 | 默认 HTTP 状态码 |
| config.headers | string map，空 map | 附加/覆盖协议默认响应头 |
| config.flush | boolean，true | body 写入及分片后是否刷新 |
| config.line_ending | lf / crlf，lf | SSE 换行格式 |
| config.usage | Usage，零值 | input/output/cache token 统计 |
| config.usage_mode | auto / emit / omit，auto | 是否包含 usage；auto 按协议和请求选项决定 |

Usage 使用协议无关字段，由适配器映射到各协议：

| 字段 | 类型 / 默认值 |
|---|---|
| input_tokens | 非负整数，0 |
| output_tokens | 非负整数，0 |
| cached_input_tokens | 非负整数，0 |
| reasoning_tokens | 非负整数，0 |

usage_mode 的行为固定为：

| 值 | 行为 |
|---|---|
| auto | 非流式响应按协议正常携带 usage；OpenAI Chat 流式仅在请求 `stream_options.include_usage=true` 时，在 `finish_reason` chunk 之后、`[DONE]` 之前单独发送一条 `{choices: [], usage: {...}}` chunk；Anthropic 流式在 `message_start` 携带 `input_tokens`，在 `message_delta` 携带 `output_tokens`；Responses 流式在 `response.completed` 携带 usage |
| emit | 只要协议存在 usage 位置就强制发送，包括数值全为 0；OpenAI Chat 流式忽略请求选项，始终在 `finish_reason` chunk 后单独发送 `{choices: [], usage: {...}}` chunk |
| omit | 不生成任何 usage 字段或独立 usage 事件（Responses 流式的 `response.completed` 中 `usage` 保持 `null`） |

error、raw 和空 body 不会自行生成 usage。显式 steps 只有发送到 generation.end 或更远时，才会发送正常输出的 usage。

headers 的名称必须符合 Go `net/http` header 规则，值中禁止 CR/LF。同一 map 中大小写不同但规范化后相同的 header 名称视为重复字段并拒绝。status、headers 和 duration 等静态字段在加载时校验。只有 output.config.usage、content、reasoning 以及 output.error 的 message/param 允许动态引用，并在单次请求的 output prepare 阶段校验类型和值域；顶层 config 和 server.config 必须全部是静态值。

配置和断言分别继承：

- 请求限制：顶层 config → server.config → 内置默认值；output 不能覆盖。
- 输出配置：选中 output.config → 顶层 config → server.config → 内置默认值。
- assert：顶层列表先执行，随后执行已定位 round 的 input.assert；局部列表不替换顶层列表。

字段缺失才回退。显式 false、0s、空字符串、空 map 和空数组都必须保留。顶层 config 和 output.config 中显式 null 是配置错误。headers 在每一层都是原子 map：下层一旦声明 headers，就整体替换上层用户自定义 headers；写 `{}` 表示清空用户 headers。协议适配器仍提供默认 Content-Type，显式 Content-Type 可以覆盖它。

`Content-Length`、`Transfer-Encoding`、`Connection`、`Trailer`、`Upgrade` 和服务固定的 `Access-Control-Allow-*` 是框架保留 header，配置中声明时直接拒绝，避免 Go HTTP 运行库改写分片、挂起或连接终止语义。

usage 按 input_tokens、output_tokens、cached_input_tokens、reasoning_tokens 四个子字段分别回退；`usage: {}` 不覆盖上层 usage，显式写 0 才覆盖对应数字。

max_body_bytes 和 read_body_timeout 只能由顶层 config 或 server.config 设置。stream、status、headers、flush、line_ending、usage 和 usage_mode 还可以由被选中的 output.config 覆盖。开始输出时，框架自动发送有效状态码和响应头；有效的 flush 控制 body 写入是否立即刷新。

HTTP 204、205 和 304 不允许响应 body。使用这些 status 时，output 必须没有 content、reasoning、tool_calls 或 error，显式 steps 也只能提交 headers、等待或结束；任何实际数据 send 都在准备阶段拒绝。

### 5.2 Output Config 覆盖

状态码、响应头和协议输出设置放在 output.config 中。顶层 config 提供所有 output 的默认值：

```yaml
config:
  stream: auto
  status: 200
  headers:
    Cache-Control: no-cache

rounds:
  - output:
      config:
        status: 202
        headers:
          X-Mock-Round: first
      content: 当前 output 覆盖顶层响应设置。
```

output.config 只设置如何输出；content、reasoning、tool_calls 和 error 定义输出什么。

第一版允许动态引用的字段固定为 `content`、`reasoning`、`tool_calls[].arguments`（支持对象/数组内的叶子节点使用系统 key 引用，在 `output prepare` 阶段先解析动态值再执行确定性 JSON 序列化）、`output.error.message`、`output.error.param` 和 `output.config.usage` 的四个数字字段。引用结果必须符合目标字段类型；其他 config 字段、`tool_calls[].id`、`tool_calls[].name`、`error.type`、`error.code` 和 `steps` 必须是静态配置。例如将请求中的 cached_tokens 写入 usage：

```yaml
output:
  config:
    usage:
      cached_input_tokens:
        key: request.json_body
        path: $.cached_tokens
        default: 0
  content: Hello
```

系统 key 只能读取，不能赋值，也不形成用户自定义变量。

## 6. 根据请求历史定位 Round

rounds 中的每个对象表示一次模型输出。框架不保存调用游标，而是由协议解析器计算当前请求已经携带了多少轮历史模型输出：

```text
round.index = request.output_count + 1
```

然后直接使用 `rounds[request.output_count]`。例如：

| 请求中的历史模型输出数 | 使用的 round |
|---|---|
| 0 | rounds[0]，第 1 个 round |
| 1 | rounds[1]，第 2 个 round |
| 2 | rounds[2]，第 3 个 round |

这里的“数量”来自传入请求，不是 YAML 中 `outputs` 列表的长度，也不是服务端已经调用了多少次。request.output_count 只用于选择 round；round 内多个 outputs 的选择由 request.count 和其他系统 key 决定。

这使单轮剧本天然可以重复调用：只要传入的都是没有历史模型输出的初始请求，每次都会使用第 1 个 round。多轮工具对话会把上一轮 assistant/tool call 带回请求，因此自然进入后续 round。

如果 request.output_count 超出 rounds 范围，返回 `mock_round_not_found`，不自动循环或使用最后一轮。

### 6.1 output_count 的协议归一化

协议适配器负责把不同请求格式归一化为“历史模型输出轮数”：

- openai：统计 messages 中 role=assistant 的消息数量；同一条 assistant 消息里的多个 tool_calls 仍算一次输出。
- anthropic：统计 messages 中 role=assistant 的消息数量；同一条 assistant 消息里的多个 content block 仍算一次输出。
- responses：扫描 input 中由模型产生的 reasoning、assistant message 和 function_call item；连续的模型输出 item 归为一组，遇到 user/developer message 或 function_call_output 后结束该组。同一组中的 reasoning、message 和多个并行 function_call 只算一次输出（1 个 round）；若多次工具调用之间夾有 function_call_output（顺序调用），则按多组计为多个 round。

注意：所有 `role: assistant` 消息一律计入 `output_count`，因此测试单轮剧本时请勿使用 assistant 角色预填少样本上下文。

只携带 `previous_response_id`、没有内嵌历史输出的 Responses 请求无法仅凭请求 body 推导 output_count。第一版返回 `mock_output_count_unavailable`，不分配 request.count，也不执行 output。response-ID 历史索引属于后续能力。

### 6.2 Input 只负责验证

可选的 input 只校验已经定位好的 round，不参与 round 选择：

```yaml
rounds:
  - input:
      assert: <AssertionList>
    output:
      config: <OutputConfigOverrides>
      content: <Content>
      reasoning: <Reasoning>
      tool_calls: <ToolCalls>
      error: <ProtocolError>
      steps: <OutputSteps>
```

处理顺序是：

1. 解析请求协议、model、stream 和 output_count。
2. 根据 output_count 定位唯一 round。
3. 执行顶层 assert。
4. 执行当前 round.input.assert；input 省略时跳过局部断言。
5. 分配 request.count，匹配并执行当前 round 中的 output。

任一断言失败时返回输入断言错误，不尝试其他 round。相同请求重复发送时仍会定位到同一个 round；服务端不记录请求之间的 round 状态。

## 7. 在 Round 内匹配 Output

round 可以使用单个 output，也可以使用多个 outputs。单个 output 是无条件简写：

```yaml
rounds:
  - output:
      content: 始终返回这一项。
```

需要根据系统 key 返回不同结果时使用 outputs。每个候选项的 when 是条件列表；列表内所有条件都为 true 时，该候选项匹配。框架按 YAML 顺序检查并选择第一个匹配项：

这里的“变量条件”指框架提供的只读系统 key，包括 request.count、request.output_count、request.json_body、request.round_input、request.header、request.stream、round.index 等；第一版仍不引入用户自定义变量。

```yaml
rounds:
  - outputs:
      - when:
          - name: fail_second_request
            key: request.count
            op: eq
            expected: 2
        config:
          status: 503
        error:
          type: server_error
          message: Mock upstream unavailable

      - content: 其他请求正常返回。
```

这个剧本的第 1 次已接受请求使用后面的无条件 output，第 2 次已接受请求使用 503 output，第 3 次及以后再次使用无条件 output。如果要表达“第 1 次失败，重试成功”，把 expected 改成 1 即可。

when 也可以读取其他系统 key。例如请求包含至少一个 tool 时返回工具调用，否则返回普通文本：

```yaml
rounds:
  - outputs:
      - when:
          - key: request.json_body
            path: $.tools
            op: length_gte
            expected: 1
        tool_calls:
          - name: get_weather
            arguments:
              city: Beijing

      - content: 不需要调用工具。
```

when 的单项语法与 assert 相同，支持系统 key、key 参数、op、expected 和可选 name，但语义不同：

- assert 为 false 时，本次请求失败，不执行任何 output。
- when 为 false 时，只跳过当前候选 output，继续检查下一项。
- when 列表使用 AND；第一版不提供 OR、表达式脚本或用户自定义变量。
- 没有 when 的 output 是 fallback，最多一个，并且必须位于 outputs 最后。
- `when: []` 无效；无条件候选必须省略 when。
- 所有带 when 的候选都不匹配且没有 fallback 时，返回 `mock_output_not_found`，诊断包含 request.count、output_count、round.index 和每个候选的条件结果。
- 实际 key 为 missing 或类型不满足操作符要求时，该条件为 false，并在诊断中记录原因；unknown key、无效 key 参数或未知 op 在加载配置时直接拒绝。
- 条件求值器自身失败时返回 `mock_output_condition_error`，不是普通的不匹配。

output 和 outputs 互斥。outputs 必须是非空列表；单个无条件场景优先使用 output，避免多一层列表。

选择顺序固定为：协议解析 → round 定位 → 输入断言 → 分配 request.count → output 条件匹配 → 执行 output。request.count 不参与 round 定位，request.output_count 也不会因为请求计数增加而改变。

## 8. Output 内容

output 的正常结果使用三个协议无关字段：

| 字段 | 类型 | 含义 |
|---|---|---|
| content | string / system-key reference | 助手回复的正文文本 |
| reasoning | string / system-key reference | 助手的思考或推理文本 |
| tool_calls | ToolCall[] | 助手发起的一个或多个工具调用 |

三个字段都可省略，也可以同时出现。框架按 reasoning、content、tool_calls 的顺序交给协议适配器。content 和 reasoning 出现时必须解析为非空字符串；tool_calls 出现时必须是非空列表，每项 name 必须是非空字符串。

流式 send step 使用“逻辑块 + 定位后缀”的 `target` selector。payload、reasoning、content、tool name 和 tool arguments 支持字符切片，generation/stream 只提供 end 里程碑。工具索引从 0 开始；具体契约见 [Output Steps 专项设计](./output_steps_design.md#43-target)。

YAML 不直接编写 choices、delta、content block 或 output item：

| 逻辑字段 | openai | anthropic | responses |
|---|---|---|---|
| content | assistant message content | text content block | output_text |
| reasoning | reasoning_content | thinking content block | reasoning item |
| tool_calls | tool_calls | tool_use content block | function_call item |

request.model 自动回填到响应。响应 ID、创建时间、协议事件 ID 和省略的 tool call ID 由框架生成。有效 config.stream=auto 时跟随 request.stream；显式 true/false 可以故意生成与请求不同的响应模式。

流式模式下，协议适配器对每个已发送的文本内容段（reasoning、content、tool arguments）按固定字符数（默认每 5 个 Unicode code point 一条 delta 事件；受 `send.target` 切片边界约束时在切片终点截断）生成协议 delta 记录，保证无需手写多条 steps 即可产生真实的流式多帧输出。

完成原因由 output 内容自动推导：包含 `tool_calls` 时 OpenAI `finish_reason` 为 `"tool_calls"`、Anthropic `stop_reason` 为 `"tool_use"`；仅包含文本/推理时分别为 `"stop"` 和 `"end_turn"`；Responses 正常完成状态为 `status: "completed"`。

### 8.1 文本和思考

```yaml
rounds:
  - output:
      reasoning: 先查询北京的天气，再组织回答。
      content: 北京今天晴。
```

### 8.2 工具调用

ToolCall 的非空 name 和 arguments 必填。arguments 必须是可表示为 JSON 的值（其内部字段值允许使用系统 key 引用，如 `{key: request.round_input, path: $[0].content}`），所有 object key 必须是 string，不能包含 YAML 专有类型。协议适配器在 `output prepare` 阶段先解析动态引用，再将所有层级的 object key 按字典序排列并确定性地序列化为紧凑 JSON。id 可选；省略时由框架生成协议合法的 ID，显式提供时必须是非空字符串。

```yaml
rounds:
  - output:
      reasoning: 需要先查询天气。
      tool_calls:
        - name: get_weather
          arguments:
            city: Beijing

  - input:
      assert:
        - name: tool_call_id
          key: request.round_input
          path: $[0].tool_call_id
          op: eq
          expected:
            key: request.json_body
            path: $.messages[1].tool_calls[0].id
    output:
      content: 北京今天晴。
```

第二次请求包含一条历史 assistant tool_calls 消息，所以 output_count=1，定位第 2 个 round。`request.round_input` 直接对应第 2 轮新提交的工具结果消息列表，断言直接比较本轮新增输入中的 `tool_call_id` 和请求历史中的 assistant tool call ID，不需要服务端保存上一轮的工具 ID。

### 8.3 协议错误

HTTP 或流式协议错误使用与 content 同级的 error，不需要为了返回错误而编写 steps：

```yaml
rounds:
  - output:
      config:
        status: 503
      error:
        type: server_error
        code: upstream_unavailable
        message: Mock upstream unavailable
```

error 字段：

| 字段 | 必填 | 含义 |
|---|---|---|
| message | 是 | 非空错误消息（string 或系统 key 引用） |
| type | 否 | 非空协议错误类型；省略时使用协议默认值 |
| code | 否 | 非空错误码；协议不支持时忽略 |
| param | 否 | 非空错误参数标识（string 或系统 key 引用）；OpenAI / Responses 错误体透传该字段，Anthropic 忽略 |

省略 steps 时，只有 error 的 output 自动发送错误并正常结束。错误编码由发送时的状态唯一决定：

| 发送 error 时的状态 | 编码结果 |
|---|---|
| 有效 status 为 4xx/5xx | 对应协议的 HTTP error JSON；尚未 committed 时同时提交响应头 |
| status 不是 4xx/5xx，protocol 为 responses | 流式 response.failed 或非流式 status=failed JSON |
| status 不是 4xx/5xx，protocol 为 openai/anthropic 且有效 config.stream=true | 对应协议的 stream error |
| 其他组合 | `mock_output_prepare_error`，不开始发送 |

正常字段与 error 可以同时声明，但此时必须显式提供 steps，用 `send.source: output` 和 `send.source: error` 指定顺序。例如可以先发送到 generation.end，再追加 stream error。没有 steps 时，同时声明正常字段与 error 是配置错误。注意：`status` 为 2xx、协议为 `openai` 或 `anthropic`、且有效 `stream=false` 时声明 `error` 属于无效组合（非流式 OpenAI/Anthropic 没有 200 内嵌 error 容器），会在准备阶段返回 `mock_output_prepare_error`。

需要 0 字节空 HTTP body（用于测试网关对空响应体的容错）时省略 content、reasoning、tool_calls 和 error，写 `output: {}`；不要使用空字符串、空列表或空 error。

### 8.4 steps 字段

steps 字段可以省略。普通响应省略该字段时，框架使用默认完整响应；显式提供后完全替代默认过程。所有 op、字段、状态机和协议映射只在 [Output Steps 专项设计](./output_steps_design.md) 中定义，本参考不重复展开。

## 9. 组合示例：重试后进入下一 Round

下面的剧本同时使用 request.count 和 request.output_count：第 1 次请求返回 503；客户端重试同一初始请求时仍在第 1 个 round，但 request.count 已变为 2，因此返回工具调用；客户端带回工具结果后，request.output_count 变为 1，进入第 2 个 round。

```yaml
version: 1
id: weather-retry

config:
  stream: auto

rounds:
  - outputs:
      - when:
          - name: first_request
            key: request.count
            op: eq
            expected: 1
        config:
          status: 503
        error:
          type: server_error
          message: Mock upstream unavailable

      - reasoning: 需要先查询天气。
        tool_calls:
          - name: get_weather
            arguments:
              city: Beijing

  - input:
      assert:
        - name: tool_result_id_matches
          key: request.round_input
          path: $[0].tool_call_id
          op: eq
          expected:
            key: request.json_body
            path: $.messages[1].tool_calls[0].id
    output:
      content: 北京今天晴。
```

执行过程：

| 调用 | request.output_count | request.count | round | output |
|---:|---:|---:|---:|---|
| 初次请求 | 0 | 1 | 1 | 条件 output：HTTP 503 |
| 重试相同请求 | 0 | 2 | 1 | fallback output：工具调用 |
| 带回工具结果 | 1 | 3 | 2 | 单个 output：最终文本 |

如果初次请求在输入断言阶段失败，它不会取得 request.count；修正后的下一次请求仍然获得 1。HTTP 503 是被选中 output 的预期行为，因此已经取得并消耗 request.count=1。
