# 配置驱动 Go Mock Server — 技术设计

> 状态：设计稿，尚未实施。YAML 字段见 [配置参考](./configuration_reference.md)，Steps 配置见 [Output Steps 专项设计](./output_steps_design.md)。

## 1. 设计目标

Go Mock 是面向测试的 HTTP 剧本执行器。技术实现需要满足四个目标：

1. 配置在开始监听前完成严格解析和静态校验。
2. 相同请求稳定选择相同 round；请求次数只影响当前 round 的 output。
3. 正常协议输出与延时、分片、挂起、断连等传输过程彼此分离。
4. Mock 自身错误可以被 capture 和测试钩子发现，不能被网关处理后掩盖。

第一版保持无热加载、无用户脚本、无服务端 round 游标。加载完成后的剧本注册表保持只读；运行期可变状态只有请求计数、活动请求和 capture 窗口。

## 2. 系统架构

```text
HTTP Controller
      ↓
Request Service ─────────────→ Capture Service
      ↓
Protocol Adapter
      ↓
Round / Output Selector
      ↓
Output Compiler
      ↓
Step Engine
      ↓
HTTP Transport
```

主要模块：

| 模块 | 职责 |
|---|---|
| controller | 路由、HTTP 参数和 JSON 响应；不承载剧本业务逻辑 |
| config service | 加载 server.yaml 和剧本 YAML，严格解码并建立只读注册表 |
| request service | 驱动单次请求的完整处理流水线 |
| protocol adapter | 按 URL 选中的协议解析请求、归一化 output_count 并生成响应记录 |
| selector service | round 定位、输入断言、request.count 分配和 output 条件选择 |
| output compiler | 解析动态引用，建立逻辑时间线，编译并静态校验 steps |
| step engine | 顺序执行 send、wait、end，并维护 HTTP 状态和 output 游标 |
| transport | Write、Flush、HTTP/1.1 Hijack、取消和连接登记 |
| capture service | 有限窗口、单调 sequence、snapshot/verify 和请求记录查询 |
| model | 配置模型、归一化请求、输出时间线、执行状态和 capture 记录 |

controller 保持薄层；业务逻辑进入 service，协议和执行状态进入 model，与业务无关的帮助函数放入 utils，常量集中在 constants。

## 3. 启动与配置编译

二进制只提供三个子命令：serve 启动 HTTP 服务，validate 完成相同的加载与编译后退出，scripts 完成校验后向 stdout 输出按 ID 字典序排列的剧本 ID 和三个协议 URL JSON 数组。三者共用 `--config`，默认值为当前目录的 `server.yaml`；未知参数返回非零退出码。

启动顺序固定为：

1. 严格解析 server.yaml，拒绝未知字段、重复 key 和无效类型。
2. 按相对路径字典序递归扫描 scripts_dir 中的 `.yaml`/`.yml` 普通文件，不跟随符号链接；每个文件只解析一个 YAML document 和一个剧本。
3. 校验 schema version、ID、round/output 结构和系统 key 使用位置。
4. 编译静态断言、when 条件、简单 JSONPath（`path`）和正则表达式。
5. 读取并限制 raw fixture；运行期间不再读取文件。
6. 校验 Output Steps 的 selector、协议能力、事件上限和终止形式（若 `content`、`reasoning` 或 `tool_calls[].arguments` 全部为静态字面量，启动时直接完成切片长度与字节/事件上限编译校验；若包含动态系统 key 引用，启动时校验 selector 语法与状态迁移合法性，具体长度与上限在请求运行时响应头提交前完成校验）。
7. 检查重复 ID，建立不可变剧本注册表。
8. 全部成功后开始监听；任一错误都阻止服务启动。

每个剧本固定注册 `/mock/{id}/v1/chat/completions`、`/mock/{id}/v1/messages` 和 `/mock/{id}/v1/responses` 三个标准形态 POST 路由。文件名和目录层级不参与寻址，server.yaml 不能修改 URL prefix。配置字段及默认值由 [配置参考](./configuration_reference.md) 唯一定义。

### 3.1 HTTP 服务行为

- 监听地址不可用或权限不足时，启动直接失败并输出明确诊断。
- 所有响应附带 `Access-Control-Allow-Origin: *`、`Access-Control-Allow-Methods: GET, POST, PUT, DELETE, OPTIONS` 和 `Access-Control-Allow-Headers: Content-Type, Authorization, x-api-key`。全局 OPTIONS 返回 200，不进入剧本流水线。
- 未匹配路径返回 JSON 404，方法不支持返回 JSON 405。
- body 超限返回 JSON 413，读取超时返回 JSON 408，非法 JSON 返回 JSON 400，协议结构无效返回 JSON 422。
- 三个剧本路由分别选择固定协议适配器；管理接口、内置 Models 端点和剧本接口使用独立路由，不参与彼此的 counter。
- 所有框架生成的错误响应包含稳定 error code，不返回 Go 默认纯文本错误页。

## 4. 请求执行流水线

进入任一 `/mock/{id}/v1/...` 剧本协议路由的请求按以下顺序处理：

```text
路由剧本
→ 读取并限制 body
→ 根据 URL 选择并执行协议解析器
→ 计算 output_count
→ 定位 round
→ 执行输入断言
→ 原子分配 request.count
→ 选择 output
→ 解析动态输出值
→ 编译协议时间线和 steps
→ 执行 steps
→ 记录结束结果
```

关键时点：

1. 路由先确定 request.protocol；对应协议解析器产生统一的 model、stream 和 output_count，不尝试其他适配器。
2. `rounds[output_count]` 是唯一候选；不存在跨 round 回退。
3. 顶层 assert 和当前 round.input.assert 全部通过后，才分配 request.count。
4. request.count 分配后，即使 output 无匹配、准备失败、无限等待或强制断连，该次数也已经消耗。
5. output compiler 必须在提交响应头前解析动态值并模拟完整 steps，避免执行一半后才发现静态配置错误。
6. 执行期间只允许客户端取消、服务关闭和真实写入错误改变预编译流程。

只携带 previous_response_id、没有内嵌历史的 Responses 请求在协议解析阶段返回 `mock_output_count_unavailable`，不分配 request.count。

body 在 JSON 解码前先做严格 UTF-8 校验；非法编码返回 `mock_invalid_json`，不允许 encoding/json 用 U+FFFD 修复。JSON 解码统一使用 `json.Decoder.UseNumber`，并确认首个 JSON 值之后只有空白；多个连续 JSON 值或其他尾随内容返回 `mock_invalid_json`。YAML 数字节点同样保留标量原文。断言比较和动态 usage 转换使用原始十进制表示，不经过 float64。

## 5. 状态与并发模型

### 5.1 服务状态

```text
starting → ready → shutting_down → stopped
```

ready 之前不接受剧本请求。serve 收到第一次 SIGINT 或 SIGTERM 后进入 shutting_down：先停止接收新请求，再取消活动请求、关闭 Hijack 连接，并在 shutdown_timeout 内等待退出；超时或收到第二次终止信号时立即退出非零状态。

### 5.2 剧本状态

每个剧本只维护：

```text
counter[openai|anthropic|responses]: atomic uint64
active_requests: atomic integer
```

round 和 output 选择不写入剧本状态。三个协议 counter 相互独立。每个剧本使用一个很短的 admission/reset 临界区：请求准入在其中递增 active_requests；counter reset 在同一临界区检查 active_requests 并把三个 counter 一起归零。这样 active_requests=0 的检查与 reset 之间不会插入新请求。存在任一协议的活动请求时 reset 返回 409，避免产生重复 request.count。

剧本路由通过全局容量检查后立即递增 active_requests，并在该请求所有退出路径中递减。因此 body 读取、输入校验、有限/无限 wait 和 Hijack 连接存活期间都算活动请求；是否已经分配 request.count 不影响 active_requests。

### 5.3 请求执行状态

单次请求维护两条正交状态：

```text
http: prepared | committed | ended | aborted
output_cursor: initial | payload | generation | stream 中的位置
```

heartbeat、error 和 raw 可以提交 HTTP 响应，但不推进 output_cursor。只有 source=output 的 send 推进正常输出游标。end.mode=normal 进入 ended；end.mode=abort 进入 aborted。

全局 max_active_requests 使用有界 semaphore，并在读取 body 前尝试获取。容量已满时返回带 `mock_capacity_exceeded` code 的 JSON 503，不分配 request.count，并在 capture 中标记为 framework error。管理接口和内置 Models 端点不占用该 semaphore。

request.count 分配仍使用原子操作；admission/reset 临界区只保护请求准入和 reset，不覆盖 body 读取、断言、output 执行或 capture 写入。capture 窗口使用独立同步机制。任何实现都不得持有全局锁或剧本锁执行 wait、网络 Write 或 Flush。

## 6. 协议适配器

三个协议适配器共享同一职责接口：

1. 校验请求是否符合 URL 已确定的协议，不做协议猜测或回退。协议校验仅做最小结构检查：请求体为 JSON Object、`model` 为非空字符串、`stream`（若存在）为布尔值，以及用于计算 `output_count` 的 `messages`（OpenAI/Anthropic）或 `input`（Responses）结构合法；不校验 `max_tokens`、`temperature` 等业务可选字段，避免误拒网关转发或补齐前后的请求。
2. 解析 model、stream 和协议请求结构。
3. 从内嵌历史计算 output_count，并提取最后一个历史模型输出之后的新增输入项作为 `request.round_input`（`output_count == 0` 时剥离位于开头的 `role: system` / `role: developer` 消息，使三协议首轮 `$[0]` 统一对应首条用户输入）。
4. 把 reasoning、content 和 tool_calls 编译为协议逻辑时间线（Anthropic thinking 块固定附带确定性占位 `signature: "mock_signature"`，流式在 `thinking_delta` 后、`content_block_stop` 前发送 `signature_delta`；Responses reasoning item 生成最小合法结构 `{id, type: "reasoning", summary: [{type: "summary_text", text: ...}]}`；多工具流式 delta 严格按工具 0-based 下标填充协议 `index`）。
5. 把 output.error 映射为 HTTP error、stream error 或 Responses failed response。
6. 把统一 Usage 映射到协议字段（Responses 流式中间事件 `response.created`/`in_progress` 的 `usage` 固定为 `null`，完成事件 `response.completed` 按 `usage_mode` 填充对象或 `null`）。

适配器输出“协议记录”，Step Engine 不直接拼接协议 JSON。流式模式下，适配器对 reasoning、content、tool arguments 按固定字符数（默认 5 个 Unicode code point）拆分为多条 delta 协议记录；OpenAI 流式的 `generation.end` 固定采用标准分拆形态：先发送带 `finish_reason`（`delta: {}`）的完成 chunk，启用 usage 时再单独发送一条 `{choices: [], usage: {...}}` chunk。工具参数在解析完内部动态系统 key 引用后先做确定性 JSON 序列化；自动 ID、响应 ID 和时间戳在 output prepare 阶段一次生成，后续分片不能重新生成。output.error 支持 `message`、`type`、`code`、`param`，其中 `message` 和 `param` 允许引用系统 key。

OpenAI/Anthropic 按历史 assistant 消息计数。Responses 把连续的 reasoning、assistant message 和 function_call item 归为一轮，遇到 user/developer message 或 function_call_output 后结束该组。

内置 `/models` 和 `/v1/models` 由独立 handler 提供，返回固定的 OpenAI 格式模型列表，不进入协议适配器、剧本选择或 request.count。

## 7. Step 执行引擎

output 没有显式 steps 时，output compiler 根据声明内容生成默认计划：

```text
正常字段      send(source=output) → end(mode=normal)
error         send(source=error)  → end(mode=normal)
空 output     end(mode=normal)
```

正常字段与 error 同时存在时必须显式提供 steps，以确定发送顺序。显式 steps 出现后完全替代默认计划。

Step Engine 只执行三个操作：

| op | 引擎职责 |
|---|---|
| send | 取得预编译的正常输出、output.error、heartbeat 或 raw 数据，应用 target、chunks、interval，然后 Write/Flush |
| wait | 使用可取消 timer 等待有限 duration，或等待取消/关闭 |
| end | 正常结束 body，或在 abort 模式下 Hijack HTTP/1.1 连接、设置 `SetLinger(0)` 并立即关闭以发送 TCP RST |

执行引擎遵循以下约束：

- steps 严格顺序执行，不跳转、不循环、不重试 Write。
- send.interval、有限 wait 和 infinite wait 都监听请求 context 与服务关闭 context。
- chunks 只改变编码后的写入边界，不改变协议记录和 output_cursor。
- end 和 infinite wait 是终止形式；终止后没有后续 step。
- HTTP/1.1 Hijack 连接必须登记，保证服务关闭时可以回收。
- HTTP/2 RST_STREAM 不在第一版范围。

selector、source、interval、chunks 和协议边界的完整配置契约只在 [Output Steps 专项设计](./output_steps_design.md) 中维护。

## 8. Capture 与管理接口

Capture 使用有界内存窗口，并为每条记录分配进程内单调递增 sequence。窗口同时受 capacity 和 max_total_bytes 限制；加入记录后任一上限超出时，从最旧记录开始淘汰。总字节统计保存的 body、header 名称和值以及诊断预览的 UTF-8 字节，不包含难以精确计算的 Go 对象开销。单条记录在 body 和诊断按各自上限截断后仍超过 max_total_bytes 时，不保存原记录，改为保存小型 `capture_record_too_large` framework error 记录。clear 只清空窗口，不回退 sequence，也不影响剧本 counter。

| method / path | 行为 |
|---|---|
| `GET /health.json` | 返回 ready、instance_id 和剧本数 |
| `GET /_test/scripts.json` | 返回剧本 ID 和 openai/anthropic/responses 三个 URL |
| `GET /_test/requests` | 返回当前窗口内捕获的请求记录数组 |
| `GET /_test/capture/snapshot.json` | 返回 instance_id 和当前 sequence cursor |
| `POST /_test/capture/verify.json` | 校验 cursor 之后的 Mock 框架错误，可按 script_ids 过滤 |
| `POST /_test/capture/clear.json` | 清空 capture 窗口 |
| `GET /_test/scripts/{id}/counter.json` | 查询三个协议的 count 和剧本 active_requests |
| `POST /_test/scripts/{id}/counter/reset.json` | 在无活动请求时重置该剧本的三个协议 counter |
| `POST /_test/counters/reset.json` | 批量重置剧本 counter（可选 `script_ids: string[]`，省略时重置全部剧本；任一目标剧本存在活动请求时返回 409 且不部分重置） |

scripts 命令和 `GET /_test/scripts.json` 共用 `{id, urls: {openai, anthropic, responses}}` 结构，urls 的值是以 `/mock/` 开头的路径，不拼接 scheme 或监听地址。counter 查询返回 `{counts: {openai, anthropic, responses}, active_requests}`；reset 成功后三个 count 都为 0。

cursor 只能用于同一 instance_id。起始 sequence 已被窗口淘汰时，verify 返回 `capture_window_lost`，不能把当前可见记录误判为完整窗口。

调用方可在每个测试用例前后通过 `snapshot.json` → `verify.json` 检查该区间内新增的请求记录。`POST /_test/capture/verify.json` 支持可选字段 `script_ids: string[]`（仅检查指定剧本）和 `allow_framework_errors: boolean`（默认 `false`；设为 `true` 时允许专门测试负向协议/断言错误的用例豁免框架错误判定）。

`GET /_test/requests` 返回 JSON 数组：每条记录包含 `method`、`url`、`headers`（`map[string]string`，同名多值以 `", "` 拼接）、`header_values`（`map[string][]string` 完整多值头）、`body`（UTF-8 字符串）、`body_truncated`、`json`（解析后的 JSON 或 `null`）、`receivedAt`（RFC3339 时间戳）、`sequence`、`script_id`、`request_count`、`output_count`、`round_index`、`output_index`、`result` 和 `framework_error`。

Capture 至少记录请求 ID、script ID、request.protocol、request.count、output_count、round/output index、断言和条件结果、step 结果、写入字节数、结束原因和 framework error。

剧本预期生成的 HTTP 4xx/5xx、协议 error、failed_response 和 end.mode=abort 属于正常剧本结果。协议解析失败、断言失败、output 无匹配、condition error、prepare error 和非预期写入错误属于 Mock 框架结果，由 verify 报告。

## 9. 错误处理

错误分为四类：

| 类别 | 示例 | 行为 |
|---|---|---|
| 启动配置错误 | 未知字段、重复 ID、非法 selector | 不开始监听 |
| 请求输入错误 | 协议无法解析、round 不存在、断言失败 | 响应结构化 Mock 错误，不执行 output |
| 剧本预期结果 | 配置的 503、stream error、abort | 按剧本执行，capture 标记为 expected |
| 运行框架错误 | 动态值缺失、Write 失败、内部状态错误 | capture 标记 framework error，verify 失败 |

响应头尚未提交时，框架错误返回带稳定 error code 的 JSON：

```json
{
  "error": {
    "code": "mock_assertion_failed",
    "message": "input assertion failed",
    "script_id": "weather",
    "request_id": "req_...",
    "details": {}
  }
}
```

code 和 message 必须存在；script_id、request_id 和 details 在上下文可用时出现。details 使用稳定 JSON 字段，不包含 Go 类型名、调用栈或日志文本。actual、expected 和其他任意值先紧凑 JSON 序列化，再在 Unicode code point 边界截断为最多 4096 个 UTF-8 字节并附带 truncated 标记，避免错误响应复制完整大请求。

状态码映射：

| HTTP status | 错误 |
|---:|---|
| 400 | `mock_invalid_json` |
| 404 | `mock_script_not_found`，或管理资源不存在 |
| 405 | `mock_method_not_allowed` |
| 408 | `mock_body_read_timeout` |
| 413 | `mock_body_too_large` |
| 422 | `mock_protocol_invalid`、`mock_output_count_unavailable`、`mock_round_not_found`、`mock_assertion_failed` |
| 500 | `mock_output_not_found`、`mock_output_condition_error`、`mock_output_prepare_error` 和内部状态错误 |
| 503 | `mock_capacity_exceeded` |

响应头已经提交后不能改写状态码；引擎记录错误并强制终止当前连接。错误日志固定输出 JSONL，并包含 script、request、round、output 和 step 上下文。

## 10. 资源限制与安全边界

- body、capture body、单次请求全部有限 send 的总字节数、单个 raw fixture、协议记录数和最终 Write/Flush 单元数分别设置上限。max_output_bytes 同时覆盖正常输出、error、heartbeat、raw inline 和被重复发送的 fixture 内容；max_write_units 在 chunks 展开后校验，防止极小分片产生海量系统调用和 timer。
- fixture 只能读取剧本 YAML 所在目录内的相对普通文件；解析符号链接后的真实路径仍必须位于该目录，并在启动时载入。
- Go duration 必须非负；只有 wait.duration 接受 infinite。
- raw 可以生成非法协议字节，但不能绕过文件边界、内存上限和连接终态。
- 不执行 shell、模板语言、用户表达式或动态代码。
- HTTP write timeout 固定关闭；无限等待只由客户端取消或服务关闭终止。
- 应用层 Write/Flush 边界不承诺 TCP 包或客户端 read 边界。

## 11. 验证策略

1. 配置参考中的所有 YAML 示例必须通过严格解析。
2. 为未知字段、重复 key、继承、missing/default 和非法 key 使用位置建立表驱动测试。
3. 为三个协议建立请求解析、output_count、`request.round_input` 和完整输出的 golden tests。
4. 为 selector、工具名原子边界、chunks 和 interval 建立 Step Compiler 测试。
5. 使用真实 socket 验证头前挂起、头后挂起、正常 EOF、unexpected EOF、TCP RST 和客户端取消。
6. 执行 `go test -race`，验证 counter、capture、取消和 shutdown 无数据竞争或 goroutine 泄漏。

## 12. 后续能力

- Responses previous_response_id 的受控历史索引。
- 热加载及其 counter/capture 保留语义。
- 自签名 HTTPS listener、压缩响应和 HTTP/2 RST_STREAM。
- 随机故障、加权条件和用户自定义变量需独立设计，不扩展第一版执行器。
