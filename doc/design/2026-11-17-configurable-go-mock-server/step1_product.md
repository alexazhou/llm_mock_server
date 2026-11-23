# 配置驱动 Go Mock Server — 产品设计

> 状态：设计稿，尚未实施。目录日期为设计日期，落地后按实际发布日期归档。

## 1. 产品定位

面向 LLM 应用与 AI 网关测试的独立、配置驱动 Go Mock Server。通过声明式 YAML 剧本描述请求断言、多轮模型输出和底层传输过程；新增正常对话、工具调用、超时、协议错误、慢速分片或异常断连场景时，只需编写或增加剧本文件，无需修改 Go 代码。

一个剧本文件通过 `id` 自动生成 OpenAI Chat Completions、Anthropic Messages 和 OpenAI Responses 三个标准形态 URL。访问的 URL 决定请求和响应协议，YAML 不再声明 `protocol`。

## 2. 使用方式

### 2.1 创建剧本

最小剧本只需要声明版本、ID 和至少一个 round：

```yaml
version: 1
id: hello

rounds:
  - output:
      content: Hello!
```

服务加载后自动提供：

```text
POST /mock/hello/v1/chat/completions
POST /mock/hello/v1/messages
POST /mock/hello/v1/responses
```

剧本不能自定义 protocol、URL、path 或 route；重命名 YAML 文件不会改变 URL。

### 2.2 按需增加行为

正常响应只定义 content、reasoning 和 tool_calls，协议错误只定义 error，不需要编写协议原始结构。需要时再增加：

- 顶层 config：请求限制，以及所有 output 共用的状态码、响应头、流式模式和 usage 默认值。
- 顶层 assert：所有 round 共用的输入断言。
- round.input.assert：只作用于当前 round 的额外输入断言。
- output.config：当前 output 的响应设置覆盖。
- output.steps：延时、分片、挂起、断连或混合发送等传输过程。

### 2.3 启动和调用

Mock Server 可以作为独立守护进程启动，也可以由自动化测试框架或 CI 流水线作为子进程管理生命周期。默认只监听回环地址的 9999 端口，客户端通过 `localhost:9999` 访问对应的协议 URL。

```text
mock-server serve --config server.yaml
mock-server validate --config server.yaml
mock-server scripts --config server.yaml
```

`serve` 启动 HTTP 服务，`validate` 只加载并静态编译校验全部配置，`scripts` 校验后输出剧本 ID 和三个协议 URL 的 JSON 列表。`--config` 省略时使用当前目录的 `server.yaml`。

服务另外提供 health 检查、剧本列表查询、请求 capture 与验证、计数器管理，以及内置的 `/models` 和 `/v1/models` 模型列表端点。

## 3. 剧本模型

```text
script
└── rounds[]
    ├── input.assert   可选输入断言
    └── output         单个无条件结果
        或 outputs[]   多个条件结果
```

每个 round 表示一次模型输出：

- input 只验证请求，不能定义返回内容。
- output 表示无条件结果。
- outputs 按声明顺序选择第一个匹配结果，最后可以提供无条件 fallback。
- output 和 outputs 不能同时出现。
- input 可以省略；空 body 使用 `output: {}`。

框架负责生成 choices、content blocks、output items、响应 ID、时间戳和省略的 tool call ID。YAML 只描述协议无关的内容和行为。

## 4. 请求如何选择响应

响应选择分两级进行：

```text
请求中的历史模型输出数量
        ↓
选择唯一 round
        ↓
执行公共断言和 round 断言
        ↓
分配该剧本当前协议的请求序号
        ↓
选择当前 round 的 output
```

### 4.1 选择 Round

框架从当前请求的对话历史计算已经包含的模型输出数量：

| 历史模型输出数 | 选择结果 |
|---:|---|
| 0 | 第 1 个 round |
| 1 | 第 2 个 round |
| 2 | 第 3 个 round |

相同初始请求重复调用时始终选择第 1 个 round。客户端把上一轮 assistant/tool call 带回请求后，才会进入后续 round。服务端不保存 round 游标。

### 4.2 在 Round 内选择 Output

输入断言通过后，框架为该剧本的当前协议 URL 分配从 1 开始的请求序号。outputs 可以根据请求序号或请求内容选择结果，因此同一个初始请求可以表达“第一次失败、重试成功”或“第 N 次失败”。三个协议 URL 的序号相互独立。

请求序号只选择当前 round 内的 output，不会推进或跳转 round。依赖精确序号的测试应使用独立剧本 ID 和协议 URL，并在测试前重置该剧本计数器。

系统 key 的名称、可读取数据、断言语法和精确计数时点由配置参考定义；第一版不提供用户自定义变量、赋值或表达式脚本。

## 5. 正常输出与异常传输

只有正常字段的 output 省略 steps 时，框架自动发送完整协议输出并正常结束 HTTP body。只有 error 的 output 也可以省略 steps，框架自动发送协议错误并结束：

```yaml
output:
  config:
    status: 503
  error:
    type: server_error
    message: Mock upstream unavailable
```

需要控制传输过程时才使用 steps，例如发送前等待、只发送部分内容、挂起、断连，或先发送正常内容再追加流式错误。Steps 只有三个操作：

| op | 作用 |
|---|---|
| send | 发送正常 output、heartbeat、协议 error 或 raw 字节 |
| wait | 有限等待或一直等待到取消/服务关闭 |
| end | 正常结束 HTTP body，或以 abort 模式强制断开连接 |

send.target 控制协议内容发送到哪个逻辑边界，send.chunks 控制编码后的字节分片，send.interval 控制每个最终发送单元之前的等待。Steps 不会自动补 `[DONE]`、message_stop 或 response.completed。

完整字段和状态机以 [Output Steps 专项设计](./output_steps_design.md) 为准。

## 6. 第一版范围

| 能力 | 第一版范围 |
|---|---|
| 剧本寻址 | 每文件一个 ID，固定生成 `/mock/{id}/v1/chat/completions`、`/v1/messages`、`/v1/responses` 三个入口 |
| 协议 | URL 唯一确定 openai、anthropic 或 responses，不从请求内容猜测 |
| 多轮请求 | 根据请求历史选择 round，不按调用次数推进 |
| 重复请求 | 剧本 ID + 协议 URL 级请求计数，可表达首次失败或第 N 次失败 |
| 输入断言 | 存在性、类型、相等、包含、正则、长度和数值比较 |
| 声明式输出 | content、reasoning、tool_calls、error、usage、状态码和响应头 |
| 异常传输 | raw 字节、缺结束事件、非法 SSE、挂起和断连 |
| 时间与连接 | 发送前等待、有限/无限 wait、正常结束和强制断连 |
| 分片 | 固定大小或显式字节大小列表，可切在 UTF-8、JSON、SSE 内部 |
| 可观测性 | capture、断言结果、选择结果、step 结果和结束原因 |
| Models 端点 | 内置 `/models` 和 `/v1/models` 固定模型列表 |
| 配置校验 | 启动前拒绝未知字段、无效类型和非法执行顺序 |

自签名 HTTPS、热加载和 HTTP/2 RST_STREAM 属于后续能力。

## 7. 典型覆盖场景

第一版支持三个协议的普通响应、工具调用、多轮对话流程以及各类异常传输故障注入，包括：

- HTTP 400/402/503 错误响应和 HTTP 200 流内/体内逻辑失败。
- 缺少完成事件、usage 或流结束标记（如缺 `[DONE]` 或 `message_stop`）。
- 响应头前挂起、响应头后 body 挂起、部分输出后挂起、完整输出后挂起。
- 慢速流式输出、SSE heartbeat 交错、CRLF 换行、无效 SSE 帧和 TCP RST 强制断连。
- 模型列表查询、prompt cache / cached tokens 动态回填、工具参数动态引用和多模态请求断言。

## 8. 验收标准

1. 新增普通响应、超时、协议错误或断连场景时，只需增加剧本配置。
2. 相同请求稳定定位同一 round；请求序号可以在该 round 内选择不同 output。
3. 输入断言失败不分配请求序号，也不尝试其他 round 或 output。
4. 断言、条件、round 和 output 错误包含剧本、字段位置、实际值及期望值等诊断。
5. 响应头前等待与响应头后 body 等待可以被客户端准确区分。
6. 可以在 SSE、JSON 和 UTF-8 编码内部切分应用层写入。
7. 正常结束、强制断连和协议业务错误具有独立、可验证的结果。
8. 客户端取消或服务关闭后，挂起请求、计时器和接管连接全部回收。
9. capture 可以区分剧本预期错误与 Mock 框架错误，并在窗口记录丢失时失败。
10. 全部单元测试、端到端集成测试和 `go test -race` 并发检查通过。

## 9. 不在第一版范围

- 只携带 previous_response_id、没有内嵌历史的 Responses 多轮定位。
- 跨 round 条件跳转。
- 随机、加权或任意表达式脚本。
- 用户自定义可变变量。
- 热加载、压缩响应、代理认证和 HTTP/2 流重置。

配置字段、系统实现、Steps、场景矩阵与实施任务分别见 [配置参考](./configuration_reference.md)、[技术设计](./step2_technical.md)、[Output Steps 专项设计](./output_steps_design.md)、[场景表达矩阵](./current_mock_coverage.md) 和 [实施任务](./step3_tasks.md)。
