# 配置驱动 Go Mock Server — Output Steps 专项设计

> 状态：设计稿，尚未实施。本文是 `output.steps` 的完整配置契约；剧本、round 和 output 字段见 [配置参考](./configuration_reference.md)，执行引擎见 [技术设计](./step2_technical.md)。

## 1. 核心模型

output 定义“返回什么”，steps 定义“何时发送、发送到哪里，以及连接怎样结束”。协议适配器负责把 reasoning、content、tool_calls 和 error 编码成 OpenAI、Anthropic 或 Responses 数据。

Steps 只保留三个操作：

| op | 作用 |
|---|---|
| send | 向客户端发送 output、heartbeat、error 或 raw 数据 |
| wait | 等待一段时间 |
| end | 正常结束 HTTP body，或强制断开连接 |

send 使用 source 区分数据来源：output、heartbeat、error、raw。source=output 消费 reasoning、content 和 tool_calls，并推进 output 游标；source=error 消费同级的 output.error。heartbeat 和 raw 的数据直接定义在 send 中。

`body`、`finish`、`done` 不再是独立操作，统一由 `send.target` 的目标决定。

## 2. 默认过程与显式 Steps

### 2.1 省略 Steps

只有正常字段的 output 不需要 steps：

```text
send → end
```

无 `target` 的 output send 生成本协议完整、合法的输出：

- OpenAI Chat 流式：payload、finish reason、可选 usage、`[DONE]`。
- Anthropic 流式：payload、message_delta、message_stop。
- Responses 流式：payload、response.completed。
- 非流式：完整 JSON 响应。

`output: {}` 的默认过程只有 end；end 会隐式提交空响应的状态码和响应头。

只有 error 的 output 也不需要 steps，默认过程为：

```text
send(source=error) → end
```

同时声明正常字段和 error 时，必须显式提供 steps 来指定发送顺序。

### 2.2 显式 Steps

steps 一旦出现，就完全替代默认过程：

```yaml
output:
  content: Hello
  steps:
    - op: send
      target: content[:3]
    - op: wait
      duration: 100ms
    - op: send
    - op: end
```

框架不会补 send 或 end。`steps: []` 无效，最后一步必须是 end 或 `wait.duration: infinite`。

## 3. 逻辑时间线与逻辑块

流式 output 被规范化为一条只能向前移动的输出时间线：

```text
initial → payload.start
  → reasoning.start → reasoning → reasoning.end
  → content.start → content → content.end
  → tool[0].start
    → tool[0].name.start → tool[0].name → tool[0].name.end
    → tool[0].arguments.start → tool[0].arguments → tool[0].arguments.end
  → tool[0].end
  → tool[1] ...
→ payload.end
→ generation.end
→ stream.end
```

省略的 reasoning、content 或 tool 直接从时间线删除。第一版拒绝空 content、空 reasoning、空 tool_calls 和空 tool name，因此不会产生 start/end 重合的零长度内容块。游标不能回退，已经位于游标后方的目标无效。

到达 payload.start 时发送协议的初始框架事件，例如 OpenAI role、Anthropic message_start 或 Responses response.created；随后才进入具体内容块。

Steps 同时维护 HTTP 状态和输出游标，两者彼此独立：

| 状态 | 取值 | 由什么推进 |
|---|---|---|
| HTTP 状态 | prepared / committed / ended / aborted | 任意数据发送、headers.end 或 end |
| 输出游标 | initial / payload / generation / stream 中的位置 | 仅 source=output 的 send |

`headers.end` 是 send 的特殊 HTTP 目标：只提交并刷新响应头，不推进输出游标。任意 source 发送实际数据前都会隐式提交响应头。这样 heartbeat、error、raw 可以与正常 output 交错，而不会改变下一次 output send 的起点。

输出时间线包含两类逻辑块：

| 类型 | 逻辑块 | 职责 |
|---|---|---|
| 内容块 | payload、reasoning、content、tool[i]、tool[i].name、tool[i].arguments | 承载输出内容；文本类内容块支持字符切片 |
| 生命周期块 | generation、stream | 承载模型完成事件和流结束标记；只支持 end 里程碑 |

payload 和 tool[i] 是复合内容块；payload 包含全部声明内容，tool[i] 包含该工具的 name 和 arguments。

时间线中的每个位置只有一个名称。`payload.end`、`generation.end` 和 `stream.end` 是依次向后的三个里程碑，不再为同一位置提供 `generation.start`、`stream.start` 等别名。

三个结束边界分别表示：

| 边界 | 含义 |
|---|---|
| payload.end | reasoning、content、tool_calls 已发送完，不包含完成事件 |
| generation.end | 模型生成完成，包含 finish reason、usage 或 response.completed |
| stream.end | 独立的流结束标记，例如 `[DONE]` 或 message_stop |

Responses 没有独立 stream.end。非流式响应是一个完整 JSON，不能按逻辑字段部分发送。

## 4. send

send 是唯一的数据发送操作。它的配置按 source、target、interval、chunks 四部分理解。

### 4.1 结构

```yaml
- op: send
  source: <output | heartbeat | error | raw>
  target: <SendTarget>
  interval: <duration>
  chunks: <ChunkConfig>
```

字段：

| 字段 | 默认值 | 含义 |
|---|---|---|
| source | output | 本次发送的数据来源 |
| target | source=output 时为本协议最终边界 | 从当前 output 游标发送到目标位置；其他 source 不适用 |
| interval | 0s | 本次 send 的每个发送单元执行前的等待时间 |
| chunks | 不分片 | 本次 send 完整编码字节序列的可选分片设置 |

source 的专属字段不能混用；target 只适用于 source=output。interval 和 chunks 是 send 的公共控制字段。

### 4.2 source

source 决定本次 send 发送什么，默认值是 output：

| source | 发送内容 | 专属字段 |
|---|---|---|
| output | 当前 output 中的 reasoning、content 和 tool_calls | target |
| heartbeat | SSE heartbeat comment | text、count |
| error | 当前 output.error 对应的协议错误 | 无 |
| raw | 未经协议编码的原始字节 | text、base64、fixture |

只有 source=output 推进 output 游标；其他 source 不改变下一次 output send 的起点。

#### 4.2.1 output

output 是默认 source，可以省略。它从当前 output 游标发送到 target；省略 target 表示发送本协议剩余的完整合法输出，但不结束 HTTP body。

#### 4.2.2 heartbeat

```yaml
- op: send
  source: heartbeat
  text: ping
  count: 1
```

heartbeat 只用于流式响应，按有效 config.line_ending 编码为单行 SSE comment，不推进 output 游标。text 默认为空字符串且不能包含 CR/LF；count 默认为 1 且必须为正整数。需要多行或故意损坏的 heartbeat 时使用 raw。

#### 4.2.3 error

```yaml
output:
  config:
    status: 503
  error:
    type: server_error
    code: upstream_unavailable
    message: Mock upstream unavailable
```

这个 output 省略 steps 即可返回 HTTP 503 错误。显式 steps 中的 error send 只引用同级 error：

```yaml
- op: send
  source: error
```

send 本身不再接受 form、type、code 或 message。发送形式由有效 status、protocol 和 config.stream 唯一确定，映射规则见 [配置参考](./configuration_reference.md#83-协议错误)。error source 不自动 end，也不推进 output 游标。

例如先提交 503 响应头，100ms 后再发送错误 body：

```yaml
output:
  config:
    status: 503
  error:
    message: Mock upstream unavailable
  steps:
    - op: send
      target: headers.end
    - op: wait
      duration: 100ms
    - op: send
      source: error
    - op: end
```

正常字段与 error 同时存在时，可以先用 source=output 发送正常内容，再用 source=error 追加流式错误：

```yaml
output:
  config:
    stream: true
  content: Hello
  error:
    type: server_error
    message: Failed after completion
  steps:
    - op: send
      target: generation.end
    - op: send
      source: error
    - op: end
```

无法根据协议、流式模式、HTTP 状态和提交状态得到合法错误形式时，准备阶段直接失败。

#### 4.2.4 raw

```yaml
- op: send
  source: raw
  text: <string>
  base64: <string>
  fixture: <relative-path>
```

text、base64、fixture 三选一，解析结果必须至少包含一个字节。text 使用 UTF-8 编码，base64 使用严格标准 Base64 解码，fixture 保留文件原始字节。raw 原样写入字节，不添加 SSE framing，也不推进 output 游标；它用于非法 JSON、错误事件顺序和其他故意损坏的协议数据。

fixture 相对于当前剧本 YAML 所在目录解析，禁止绝对路径和 `..` 逃逸。加载器解析目录与目标文件的符号链接后，再确认目标仍位于该目录的真实路径内且是普通文件。文件在配置加载时读取并校验，服务运行期间不会重新读取。

### 4.3 target

只有 source=output 可以设置 target。target 是输出时间线上的绝对目标；send 从当前 output 游标向前发送到该目标。

#### 4.3.1 语法

`target` 统一写成“逻辑块 + 定位后缀”。逻辑块包括：

```text
headers
payload
reasoning
content
tool[0]
tool[0].name
tool[0].arguments
generation
stream
```

工具索引遵循 Python 和协议数组习惯，从 0 开始。承载内容的逻辑块支持 `.start` 和 `.end`，并支持 Python 风格前缀切片；HTTP 和生命周期块只提供有实际效果的结束里程碑：

```text
selector := content-block.start | content-block.end
          | content-block[:N]
          | headers.end | generation.end | stream.end
```

```yaml
targets:
  - headers.end
  - payload.start
  - payload[:10]
  - payload.end
  - reasoning.start
  - reasoning[:5]
  - reasoning.end
  - content.start
  - content[:10]
  - content.end
  - tool[0].start
  - tool[0].name[:3]
  - tool[0].name.end
  - tool[0].arguments[:8]
  - tool[0].arguments.end
  - tool[0].end
  - generation.end
  - stream.end
```

定位规则：

```text
.start  已进入逻辑块、尚未发送第一个内容字符的位置；包含协议需要的 start 事件
[:N]   内容块开始后的第 N 个 Unicode code point 位置
.end    逻辑块完整结束后的位置；包含协议需要的 end 事件
```

第一版不支持 `[A:B]`、负数和步长，因为它们会跳过尚未发送的内容或破坏顺序发送语义。

| 逻辑块 | `.start` | `.end` | `[:N]` |
|---|---|---|---|
| headers | 不支持 | 支持 | 不支持 |
| payload | 支持 | 支持 | 支持，跨 reasoning/content/tool 子块计数 |
| reasoning、content | 支持 | 支持 | 支持 |
| tool[i] | 支持 | 支持 | 不支持 |
| tool[i].name、tool[i].arguments | 支持 | 支持 | 支持 |
| generation、stream | 不支持 | 支持 | 不支持 |

#### 4.3.2 headers.end

```yaml
- op: send
  target: headers.end
```

到达 headers.end 时提交有效 status 和 response headers，并立即 Flush，但不发送 payload。它用于区分响应头前等待和响应头后的 body 等待。

任意 source 的 send 在响应头尚未发送时都会隐式提交响应头。`end.mode: normal` 也会隐式提交响应头；wait 和 `end.mode: abort` 不会隐式提交。

因此：

```yaml
steps:
  - op: wait
    duration: infinite
```

表示响应头前挂起；而：

```yaml
steps:
  - op: send
    target: headers.end
  - op: wait
    duration: infinite
```

表示响应头已发送，body 前挂起。

#### 4.3.3 内容块切片

切片是块内绝对目标，不是“再发送 N 个字符”。例如当前 content 已发送 2 个字符：

```yaml
- op: send
  target: content[:5]
```

只发送 content 的第 3～5 个字符。当前游标已经达到或越过 `content[:5]` 时配置无效。

切片长度必须精确存在。内容块不足 N 个字符时，执行前返回 `mock_output_prepare_error`，不会静默发送到块末尾。

`[:N]` 只到第 N 个字符之后，不包含该块的 `.end` 边界。即使 N 恰好等于完整字符数，仍需后续发送到 `.end`，才能产生 content_block_stop、output_item.done 等协议结束事件并推进逻辑游标。没有独立结束事件的协议也保留这个逻辑位置，因此 selector 语义不随协议变化。

#### 4.3.4 复合内容块

payload 是包含 reasoning、content 和全部 tool 的复合内容块。它的切片可以跨子块：

```yaml
- op: send
  target: payload[:10]
```

`payload[:10]` 表示从 payload 开始累计 10 个逻辑字符。连续分段可以写成 `payload[:5]`、`payload[:10]`、`payload.end`。它不会越过 payload.end 去发送 generation 或 stream 内容。

payload 只计算 reasoning、content、tool name 和 tool arguments；role、tool ID、item ID 和事件类型等协议框架元数据不计入字符数。

#### 4.3.5 工具目标

工具必须显式使用 0-based 索引，避免“当前工具”或“下一个工具”的隐式状态：

```yaml
- op: send
  target: tool[0].name[:3]
```

这是一次 send：它先发送到第一个工具开始，再发送该工具名前 3 个字符，然后停止。

```yaml
- op: send
  target: tool[1].arguments[:8]
```

它发送到第二个工具，并在该工具参数的第 8 个字符处停止。途中尚未发送的块都会按时间线顺序完整发送。

协议能力：

| 协议 | 工具名 | 工具参数 |
|---|---|---|
| OpenAI Chat | 可使用多个 function.name delta 分段 | 可使用多个 function.arguments delta 分段 |
| Anthropic | name 必须随 content_block_start 原子发送 | 可使用 input_json_delta 分段 |
| Responses | name 必须随 response.output_item.added 原子发送 | 可使用 function_call_arguments.delta 分段 |

因此 Anthropic 和 Responses 不允许停在 `tool[i].start` 或工具名中间；应至少定位到 `tool[i].name.end`。若只测试网络字节分片，使用 chunks。

tool arguments 先确定性地序列化为紧凑 JSON：所有层级的 object key 都按字典序排列，不添加无意义空格。切片作用于序列化后的 Unicode 字符。

#### 4.3.6 生命周期目标

以下三种异常流不需要独立的 finish/done 操作：

```yaml
# payload 完整，但缺少完成事件和流结束标记
steps:
  - op: send
    target: payload.end
  - op: end
```

```yaml
# 有完成事件，但缺少 [DONE] 或 message_stop
steps:
  - op: send
    target: generation.end
  - op: end
```

```yaml
# 协议完整，但 HTTP body 保持不结束
steps:
  - op: send
  - op: wait
    duration: infinite
```

### 4.4 interval

interval 是可选的发送前等待时间，默认值为 0s。它必须是非负 Go duration，不支持 infinite。

每个最终发送单元执行前都等待一次 interval，包括第一个；发送完成后不等待：

```text
wait(interval) → unit[0] → wait(interval) → unit[1] → wait(interval) → unit[2]
```

最终发送单元按以下方式确定：

- 未配置 chunks：一个协议记录是一个发送单元。
- 配置 chunks：一个字节片是一个发送单元。
- target=headers.end：提交响应头是一个发送单元。

协议记录是协议适配器生成的一条完整记录，例如一个 SSE event、一个 heartbeat comment，或者非流式响应的完整 JSON。

```yaml
- op: send
  interval: 100ms
```

未配置 chunks 时，这表示每条协议记录发送前等待 100ms。

### 4.5 chunks

send 先由协议适配器生成有序协议记录，再把本次 send 的记录编码并连接成一段字节序列。配置 chunks 时，只对这段完整字节序列切分一次：

```yaml
chunks:
  size: <positive-integer>
  sizes: <positive-integer-list>
  flush: <boolean>
```

| 字段 | 默认值 | 含义 |
|---|---|---|
| size | 未设置 | 按固定最大字节数切分 |
| sizes | 未设置 | 按列表长度依次切分，剩余字节作为最后一片 |
| flush | 继承有效 config.flush | 每个字节片写入后是否 Flush |

chunks 出现时必须且只能设置 size 或 sizes。chunks 在协议记录编码完成后生效，可以切在协议记录之间，也可以切在 JSON、SSE 或 UTF-8 字符内部，但不改变逻辑事件数量。

size 按固定最大字节数切完整序列。sizes 必须是非空正整数列表，并从完整序列开头依次取指定字节数；全部 sizes 之后仍有字节时，剩余字节作为最后一片。sizes 之和超过完整序列长度时准备失败。

分片后的字节片成为 send.interval 所作用的最终发送单元。因为切分对象是本次 send 的完整字节序列，sizes 不会在每条 SSE event 上重新开始。

```yaml
- op: send
  chunks:
    size: 10
```

这个配置表示每个字节片最多 10 字节。

```yaml
- op: send
  chunks:
    sizes: [5, 10]
```

这个配置表示本次 send 的前 5 字节作为第一片，随后 10 字节作为第二片，其余字节作为最后一片；协议记录数量不会让 sizes 重新计数。

## 5. wait

wait 不发送数据，只暂停当前 steps 的执行。

### 5.1 结构

```yaml
- op: wait
  duration: <duration | infinite>
```

### 5.2 duration

wait 不发送任何数据。duration 接受普通 Go duration 或小写特殊值 `infinite`：

```yaml
# 有限等待，结束后继续下一步
- op: wait
  duration: 100ms

# 一直等待，直到客户端取消或服务关闭
- op: wait
  duration: infinite
```

普通 duration 必须非负，`0s` 是 no-op，结束后继续下一步。infinite wait 必须是最后一步，并一直等待客户端取消或服务关闭。两种模式都监听请求 context 和服务关闭 context，不使用不可取消的 sleep。

## 6. end

end 终止当前请求的 steps，不补 payload、generation.end 或 stream.end。

### 6.1 结构

```yaml
- op: end
  mode: <normal | abort>
```

### 6.2 mode

| mode | 默认值 | 含义 |
|---|---|---|
| normal | 是 | 正常结束 HTTP response body；底层 keep-alive 连接可以继续复用 |
| abort | 否 | 使用 HTTP/1.1 Hijacker 接管底层连接，对 TCP 连接设置 `SetLinger(0)` 后立即关闭，强制发送 TCP RST（若响应头或部分 chunked 数据已 Flush，则客户端收到已刷新数据后立即遭遇连接重置 / 异常断开） |

```yaml
# 正常结束；mode: normal 可以省略
- op: end

# 强制断开连接
- op: end
  mode: abort
```

两种 mode 都只终止传输，不自动补齐任何协议内容。normal 在响应头尚未提交时会提交响应头，并把 HTTP 状态推进到 ended；abort 不提交响应头，直接把状态推进到 aborted。end 必须是最后一步。HTTP/2 RST_STREAM 属于后续能力。

## 7. 协议边界映射

| 逻辑边界 | OpenAI Chat 流式 | Anthropic 流式 | Responses 流式 |
|---|---|---|---|
| payload.end | 最后一条 reasoning/content/tool delta | 最后一个 content_block_stop | 最后一个 output_item.done |
| generation.end | 先发 `finish_reason` chunk（`delta: {}`），若产生 usage 再发独立的 `{choices: [], usage: {...}}` chunk | message_delta/stop_reason/usage | response.completed |
| stream.end | `data: [DONE]` | message_stop | 不存在 |

流式模式下，适配器将每个逻辑内容块（reasoning、content、tool arguments）按固定字符数（默认每 5 个 Unicode code point 一条 delta 事件，并在 `send.target` 边界处对齐切分）拆分为多条协议记录。`send.target` 到达较远目标时会依次生成途中所有事件。例如从开始发送到 generation.end，会自动经过完整 payload.end。

非流式响应只允许无 `target` 的 output send；逻辑切片和生命周期边界在执行前拒绝。非流式仍可通过 chunks 对完整 JSON 做字节分片。

## 8. 状态与校验

Step 引擎维护：

```text
http: prepared | committed | ended | aborted
output_cursor: <logical-position>
```

在发送响应头之前，框架完成：

1. 解析 output 和系统 key。
2. 生成响应 ID、tool call ID 和时间戳。
3. 建立协议逻辑时间线。
4. 静态模拟 HTTP 状态和输出游标，确认 output target 严格向前。
5. 校验协议能力、事件数量、编码长度和 chunks 分片。

主要规则：

1. steps 非空，并且只以 end 或 `wait.duration: infinite` 结束。
2. headers.end 最多显式执行一次；HTTP committed 后不能再次把它作为 target，也不能修改 status 或 response headers。
3. 除 headers.end 外，source=output 的 send 目标必须存在且位于当前 output 游标之后。
4. `[:N]` 必须满足 N>=0，目标块支持切片，并且至少包含 N 个 Unicode code point；`[:0]` 等价于 `.start`。当目标内容块（`content`、`reasoning` 或含动态引用的 `tool[i].arguments`）完全为静态字面量时，在启动加载阶段直接校验 N 不越界并校验 `max_output_bytes`/`max_generated_events`/`max_write_units`；当包含系统 key 动态引用时，启动阶段仅校验 selector 语法、状态顺序与协议能力，具体长度及大小上限在请求运行时 `output prepare`（提交响应头前）解析完动态值后立即校验。
5. tool 索引必须存在且从 0 开始；selector 不隐式选择当前或下一个工具。
6. 非流式 output send 只允许省略 target 或使用 headers.end；reasoning、content、tool、payload、generation、stream 等内容和生命周期 target 均无效。省略 target 时，interval 在完整 JSON 记录发送前生效。
7. Responses 不允许 stream.end。
8. Anthropic/Responses 不允许停在 tool[i].start 或 tool[i].name 中间；payload[:N] 落在这些不可拆边界中时同样无效。
9. chunks 出现时必须且只能设置 size 或非空 sizes；所有大小必须为正，sizes 之和不得超过本次 send 完整字节序列的长度。
10. source=raw 可以绕过协议顺序，但不能出现在连接终态之后。
11. source 必须是 output、heartbeat、error、raw 之一；source 专属字段不能混用；source=error 要求 output.error 存在。
12. wait.duration 必须是非负 Go duration 或 infinite；infinite 时必须是最后一步。send.interval 必须是非负 Go duration，不支持 infinite。
13. target=headers.end 不产生数据记录：interval 在提交响应头前生效，chunks 必须省略。
14. heartbeat.count 必须为正整数，heartbeat.text 禁止 CR/LF；HTTP error 要求有效 status 为 4xx 或 5xx。
15. end.mode 必须是 normal 或 abort；省略时为 normal。
16. 只有 error、没有显式 steps 时生成 send(error) → end；正常字段与 error 同时存在但没有 steps 时配置无效。
17. 有效 status 为 204、205 或 304 时，禁止任何产生实际 body 字节的 send；headers.end、wait 和 end 仍可使用。
18. 编译后的总输出字节、协议记录和最终发送单元分别不得超过 max_output_bytes、max_generated_events 和 max_write_units。

准备失败返回 `mock_output_prepare_error`，不执行任何 step。request.count 已经分配，因此该次请求仍然消耗计数。

第一版默认上限：

```text
max_output_bytes: 67108864
max_fixture_bytes: 8388608
max_generated_events: 10000
max_write_units: 100000
```

## 9. 取消与可观测性

wait.duration 和 send.interval 全部监听请求 context。客户端断开后不继续执行后续 step，也不重试写入。

capture 至少记录：

```text
script_id
request.count
request.output_count
round.index
output.index
step.index
step.op
send.source
send.from
send.target
end.mode
events_generated
writes
bytes_written
elapsed
result
```

`end.mode: normal` 记录 result=ended，`end.mode: abort` 记录 result=aborted。其他终止 result 包括 client_cancelled 和 server_shutdown，异常包括 write_error 和 prepare_error。协议是否已经完整发送由 output_cursor 单独判断，不再使用含义重叠的 completed result。

## 10. 典型故障与流式场景表达

| 场景 | Steps |
|---|---|
| 正常流 | 省略 steps，或 send → end |
| payload 后直接 EOF | send(target: payload.end) → end |
| 缺 `[DONE]` / message_stop | send(target: generation.end) → end |
| 完整协议后挂起 | send → wait(duration: infinite) |
| 响应头前挂起 | wait(duration: infinite) |
| 响应头后挂起 | send(target: headers.end) → wait(duration: infinite) |
| 部分 content 后 EOF | send(target: content[:N]) → end |
| 部分输出后断连 | send(target: payload[:N]) → wait(duration) → end(mode: abort) |
| 慢速正常流 | 一个 send(interval) → end，或多个 send(interval) → end |
| 心跳 | send(output) 与 send(heartbeat) 交错 |
| 无效 SSE | send(raw) → end |
| HTTP 协议错误 | 声明 output.error，省略 steps |
| 完成后出现 stream error | 声明正常字段和 error；send(output, target: generation.end) → send(error) → end |
| Responses failed 后挂起 | 声明 error；send(error) → wait(duration: infinite) |

第一版不保证 TCP 包边界，不支持 HTTP/2 RST_STREAM，也不执行任意用户脚本。
