# 配置驱动 Go Mock Server — 场景表达矩阵

> 状态：设计稿，尚未实施。本文总结两级选择语义、系统 key 边界，以及各类正常与异常传输场景的配置表达方式。

## 1. 概述

普通输出可以省略 `output.steps`；异常传输通过显式 steps 描述。`request.output_count` 负责选择 round，`request.count` 和多个 `outputs.when` 负责在同一 round 内按请求次数或其他系统 key 选择结果。

Output Steps 的 send 目标、协议错误、raw 和字节分片契约见 [Output Steps 专项设计](./output_steps_design.md)；Models list 使用内置 `/models` 与 `/v1/models` 端点，请求记录检查使用 capture 接口。只带 `previous_response_id`、没有内嵌历史的 Responses 请求无法仅从请求体计算 round，第一版明确拒绝。

## 2. 两级选择语义

第一步，框架检查传入请求已经包含几轮历史模型输出，再直接按下标选择 round：

```text
round.index = request.output_count + 1
selected_round = rounds[request.output_count]
```

| 请求内容 | output_count | 选择结果 |
|---|---:|---|
| 初始请求，没有历史 assistant/model 输出 | 0 | 第 1 个 round |
| 请求带回 1 轮历史 assistant/model 输出 | 1 | 第 2 个 round |
| 请求带回 2 轮历史 assistant/model 输出 | 2 | 第 3 个 round |

因此：

- 同一份初始请求调用任意次数，都会使用第 1 个 round；request.count 的变化不会推进 round。
- 工具调用的下一次请求携带上一轮 assistant tool call，因而使用第 2 个 round。
- `input.assert` 只验证已经选中的 round，不负责匹配 output 或跳到其他 round。
- output_count 超过 rounds 长度时返回 `mock_round_not_found`。

第二步，输入断言通过后，框架为该 script + request.protocol 原子分配从 1 开始的 request.count，再从已选中 round 的 outputs 中按顺序选择第一个 when 全部为 true 的候选项。没有 when 的末尾项是 fallback。

```text
selected_round = rounds[request.output_count]
request.count = atomic_increment(script.protocol_counter[request.protocol])
selected_output = selected_round.output
               or first_match(selected_round.outputs, system_keys)
```

单个 output 是无条件简写，不需要 outputs 列表。request.count 是服务端按剧本和协议保存的状态，因此测试间需要显式 counter reset；它和 round 定位所使用的请求历史是两个独立概念。

### 协议计数

- OpenAI Chat：统计 `messages` 中 `role: assistant` 的消息数；同一 assistant 消息内多个 tool_calls 只算一轮。
- Anthropic Messages：统计 `messages` 中 `role: assistant` 的消息数；同一消息内多个 content block 只算一轮。
- Responses：将同一次历史响应的 reasoning、message 和 function_call items 归组后计为一轮。

Responses 如果只有 `previous_response_id`、请求中没有内嵌历史，纯无状态服务无法推导 output_count。第一版拒绝并返回 `mock_output_count_unavailable`；后续可扩展受控的 response-ID 历史索引。

## 3. 核心能力与配置表达

| 模拟能力 | 设计表达 | 说明 |
|---|---|---|
| OpenAI/Anthropic/Responses 普通文本 | `output.content` | 协议适配器生成各自容器 |
| thinking/reasoning | `output.reasoning` | 转成对应协议的 reasoning/thinking 结构 |
| 三协议工具调用 | `output.tool_calls` | ID 省略时自动生成，显式 ID 保持不变；arguments 支持系统 key 引用 |
| 流式/非流式 | `config.stream: auto/true/false` | auto 跟随请求 |
| 模型名回填 | `request.model` | 框架解析并自动写入协议响应 |
| 状态、headers、usage、LF/CRLF | 顶层 config 或 `output.config` | output 内字段优先，缺失时回退 |
| 请求字段断言 | 顶层 assert + `round.input.assert` | 两组依次执行 |
| 单轮重复调用 | output_count=0 始终选第 1 轮 | request.count 可让每次选择不同 output |
| 首次失败后成功 | `outputs.when request.count == 1` | 失败项在前，无条件成功项作为 fallback |
| 指定第 N 次失败 | `outputs.when request.count == N` | 其他次数使用 fallback |
| 按请求字段选择结果 | `outputs.when` + 任意系统 key | 可检查 tools、header、query、body、stream 等 |
| 有 tools 时返回工具调用 | `request.json_body $.tools` + `length_gte 1` 条件 | 无 tools 时由 fallback 返回文本 |
| 同一 URL 自动跟随 stream | `config.stream: auto` | 不需要为 true/false 分别建剧本 |
| cached_tokens 动态回填 | output.config.usage 引用 request.json_body | 保留请求中的数值类型 |
| 多轮工具结果校验 | `request.round_input` 与 `request.json_body` 的 `path` 比较 | 比较本轮新增 tool result ID 与历史 tool call ID |
| 头前挂起 | wait(duration: infinite) | 不发送 headers |
| 头后 body 挂起 | send(target: headers.end) → wait(duration: infinite) | headers.end 立即 Flush |
| 部分输出后挂起 | send(target: payload[:N]) → wait(duration: infinite) | 发送前 N 个逻辑字符后挂起 |
| 完整输出后挂起 | send → wait(duration: infinite) | 发送完整协议内容但保持连接不关闭 |
| 中途强制断开 socket | … → end(mode: abort) | HTTP/1.1 Hijack + `SetLinger(0)` 发送 TCP RST |
| 不完整响应后 EOF | send(target: payload.end) → end | 不发送 generation.end 或 stream.end |
| 缺 `[DONE]` / `message_stop` | send(target: generation.end) → end | 保留完成事件，省略 stream.end |
| 慢速和分片 | send.interval、chunks 或多个带 interval 的 send | target 控制逻辑目标，chunks 控制编码后的字节切分，interval 在最终发送单元前等待 |
| heartbeat comment | send(source: heartbeat) | 可结合 CRLF |
| 无效 SSE JSON | send(source: raw) | 原样写入非法帧 |
| HTTP/流式协议 error | `output.error` | 普通错误省略 steps；需要控制顺序时用 send(source: error) |

省略 steps 时，正常字段自动执行 send(output)/end，error 自动执行 send(error)/end，空 output 只执行 end。只有延时、部分发送、挂起、断连、raw 或正常内容与错误混合发送时才描述 steps。

### 25 类典型异常与边界场景

| 场景标识 | 行为描述 | 配置表达 |
|---|---|---|
| chat-incomplete | 部分 chunk 后 EOF，无 finish/`[DONE]` | send(target: payload[:N]) → end |
| chat-no-done-no-usage | content + finish 后 EOF，无 usage/`[DONE]` | usage_mode=omit + send(target: generation.end) → end |
| chat-no-done | content + finish + usage 后 EOF，无 `[DONE]` | usage_mode=emit + send(target: generation.end) → end |
| chat-disconnect | 一个 chunk 后强制重置 socket | send(target: payload[:N]) → wait(duration) → end(mode: abort) |
| chat-slow | 两个 chunk 后永久挂起 | 多个 send(interval) → wait(duration: infinite) |
| chat-hang-body | headers 已发，body 不发 | send(target: headers.end) → wait(duration: infinite) |
| chat-hang-headers | 不发送 headers | wait(duration: infinite) |
| chat-trickle | 每 400ms 一段，最终正常完成 | 多个 send(interval: 400ms) → end |
| chat-bad-data | 非法 SSE JSON 后 EOF | send(raw) → end |
| chat-heartbeat | CRLF 数据帧和 comment 交错 | send(output) 与 send(heartbeat) 交错 |
| chat-stream-error | 正常 finish 后又出现 error chunk | 正常字段 + error；send(target: generation.end) → send(error) → end |
| chat-error-400 | HTTP 400 OpenAI error body | config.status=400 + error，省略 steps |
| chat-unavailable-503 | HTTP 503 JSON error | config.status=503 + error，省略 steps |
| chat-balance-402 | HTTP 402 JSON error | config.status=402 + error，省略 steps |
| responses-incomplete | created + delta 后 EOF | send(target: payload[:N]) → end |
| responses-stream-failed-hang | response.failed 后挂起 | error；send(error) → wait(duration: infinite) |
| responses-stream-failed | response.failed 后 EOF | error，省略 steps |
| responses-slow | created + delta 后挂起 | send(target: payload[:N]) → wait(duration: infinite) |
| responses-complete-then-hang | response.completed 后挂起 | send → wait(duration: infinite) |
| responses-error | HTTP 错误响应 | config.status + error，省略 steps |
| responses-failed-body | HTTP 200、body status=failed | error，省略 steps |
| messages-stream-error | Anthropic error event + 尾随终止数据 | error；send(error) → send(raw: `[DONE]`) → end |
| messages-incomplete | start + delta 后 EOF，无 message_stop | send(target: payload[:N]) → end |
| messages-slow | start + delta 后挂起 | send(target: payload[:N]) → wait(duration: infinite) |
| messages-error | HTTP Anthropic error body | config.status + error，省略 steps |

## 4. 系统 key 边界

第一版不需要用户自定义变量。只读系统 key 覆盖输入引用、output 匹配和动态回填：

- 断言或匹配 request.method、header、query、body、json_body（配合简单 JSONPath `path`）、round_input（本轮新增输入）、model、stream、protocol 和 output_count；outputs.when 还可以匹配断言通过后产生的 request.count。
- 比较同一请求内两个位置，例如历史 assistant tool call ID 与 `request.round_input` 中的 tool result ID。
- 把请求里的字段动态写入 `content`、`reasoning`、`tool_calls[].arguments`、`output.error.message`、`output.error.param` 或 `output.config.usage`。
- 在日志、诊断和 capture 中记录 script.id、request.protocol、request.path、request.count、round.index 和 output index。

当前条件模型的边界是：

- tools、header 或 body 条件可以选择当前 round 的不同 output，但不能跳到另一个 round；round 只由 output_count 决定。
- request.count 可以让相同请求第一次返回 503、第二次返回 200；计数按 script ID + request.protocol 隔离，同一协议 URL 的并发调用只能保证 count 唯一，不能保证响应完成顺序。
- 不能代替无限 wait、`end.mode: abort`、raw、字节分片等传输过程；这些属于 steps。
- 不能执行用户提供的任意表达式、赋值或脚本；when 只使用结构化系统 key 条件。

需要精确调用顺序的测试应串行使用独立 script ID 和协议 URL，并在测试前显式重置计数器。

## 5. 网络传输边界

Go Mock 可以控制每次应用层 Write 的字节内容、顺序、间隔和 Flush，但不能保证客户端每次 read 对应一次 Write，也不能保证 TCP 包边界。代理、内核和 HTTP 实现可能合并或再次拆分数据。

“任意字节切分”用于验证客户端是否正确处理任意分片的字节流，配置中的 chunks 不作为网络包边界契约。
