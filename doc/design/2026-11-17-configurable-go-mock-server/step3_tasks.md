# 配置驱动 Go Mock Server — 实施任务

> 状态：待实施。本文拆分各阶段实现与验证任务。

## 1. 实施顺序

先固定配置契约、round 定位、请求计数和 output 匹配，再实现协议输出与真实连接控制，最后补齐内置场景剧本、CLI 构建与端到端测试。各阶段均可独立验证。

| 阶段 | 产物 | 依赖 |
|---|---|---|
| A | Go module、配置模型、validate/scripts 命令 | 无 |
| B | URL 寻址、协议解析、round 定位、请求计数、output 条件匹配、断言 | A |
| C | 普通协议输出、Step 执行引擎、SSE、分片和连接控制 | B |
| D | 请求捕获、snapshot/verify、counter 管理和诊断日志 | B、C |
| E | 标准示例剧本库、内置 Models 端点与端到端集成测试 | A–D |
| F | CLI 构建脚本、优雅关闭验证与 CI 流水线 | E |

## 2. 任务清单

### A. 配置与工程骨架

- [ ] 在仓库根目录初始化 Go module，固定 Go/YAML 依赖版本。
- [ ] 建立 controller、service、model、transport、utils、constants 目录，遵循项目分层和 gofmt。
- [ ] 实现 `version: 1` 严格配置解码；未知字段、重复 YAML key、anchor、alias、merge key、自定义 tag 和无效类型直接报错。
- [ ] 实现严格 server.yaml 解码，为每个 ID 固定注册 `/mock/{id}/v1/chat/completions`、`/mock/{id}/v1/messages`、`/mock/{id}/v1/responses` 三个标准形态路由；server.config 作为剧本 ScriptConfig 的服务级默认值，不提供可配置 URL prefix。
- [ ] 按相对路径字典序递归扫描 scripts_dir 的 `.yaml`/`.yml` 普通文件，不跟随符号链接；拒绝空文件和多 YAML document 文件。
- [ ] 校验剧本 ID、非空 rounds、output/outputs 互斥、非空 outputs、唯一末尾 fallback 和自动生成的三个 URL；YAML 中出现 protocol/url/path/route 或重复 ID 时直接拒绝。
- [ ] 实现 `serve`、`validate` 和 `scripts` 子命令，共用默认指向当前目录 `server.yaml` 的 `--config`；scripts 向 stdout 输出稳定排序的 JSON 数组，所有错误写 stderr 并显示文件与配置节点位置。
- [ ] 以 [配置参考](./configuration_reference.md) 为契约，使 Go 类型、严格校验器和全部文档示例保持一致。

验收：无效配置不能开始监听；可列出每个 ID 生成的三个协议 URL；重命名文件不改变 URL。

### B. 请求解析、Round 定位、计数和条件匹配

- [ ] 实现相互分离的 `ScriptConfig`、`OutputConfig`、`AssertionList`、`ConditionList`、`RoundInput` 和 `RoundOutput` 类型。
- [ ] 实现 body 大小限制、读取超时、严格 UTF-8、JSON 解析和简单 JSONPath（`path`，支持 `.field`、`['key']`、正负下标 `[0]`/`[-1]` 及通配 `[*]`）；JSON Decoder 使用 UseNumber 并拒绝第二个 JSON 值或非空白尾随内容。body 超限返回 413，读取超时返回 408，非法 UTF-8/JSON 返回 400，协议结构或输入不满足返回 422。
- [ ] 在读取 body 前实现全局 max_active_requests 容量控制；容量满时返回 `mock_capacity_exceeded` JSON 503，不分配 request.count，并记录 framework error。
- [ ] 三个 URL 分别固定选择 `openai`、`anthropic`、`responses` 适配器，不根据 header/body 猜测或回退；自动提取并校验 model、stream 和对话历史。
- [ ] 为三个协议实现 `request.output_count` 归一化与 `request.round_input`（最后一个历史模型输出之后的新增输入列表；首轮 `output_count == 0` 时剥离前置 `system`/`developer` 消息）提取：OpenAI/Anthropic 统计历史 assistant 输出；Responses 将同一轮的 reasoning、message、function_call 项归为一个模型输出。
- [ ] 明确拒绝只有 `previous_response_id`、但没有可计数历史的 Responses 请求，并返回可诊断错误。
- [ ] 使用 `rounds[request.output_count]` 定位唯一 round；越界返回 `mock_round_not_found`。不得保存服务端 round 游标，也不得按调用次数推进。
- [ ] 验证相同初始请求连续调用仍选择第 1 个 round；携带一轮模型历史的请求选择第 2 个 round；并发相同请求得到相同 round。
- [ ] 实现按 script ID + request.protocol 隔离的三个原子 request.count；只在协议解析、round 定位和全部输入断言通过后分配，各自从 1 开始。
- [ ] 验证解析失败、round 不存在和断言失败不计数；计数分配后，无 output 匹配、输出错误、无限 wait 和 `end.mode: abort` 均消耗该次数。
- [ ] 实现 outputs.when 顺序匹配：条件列表使用 AND，选择第一个匹配项；无 when 的末尾项为 fallback。
- [ ] 验证多个候选同时匹配时前项优先；多个 fallback、fallback 非末项、空 when 和空 outputs 在启动时拒绝。
- [ ] 实现 `mock_output_not_found` 和 `mock_output_condition_error` 诊断，记录每个候选条件的求值结果。
- [ ] 所有响应头提交前的框架错误使用统一 JSON envelope；actual/expected 等任意诊断值使用最多 4096 UTF-8 字节的紧凑 JSON 预览和 truncated 标记，不泄漏 Go 调用栈。
- [ ] 验证 request.count=2 可只让同一协议 URL 的第 2 次已接受请求失败；request.count=1 可表达首次失败、重试成功，其他协议 URL 的 count 不受影响。
- [ ] 实现只读系统 key：request、script 和 round 范围内的文档字段，包括 request.count；第一版不提供自定义变量、作用域、赋值或生成器。
- [ ] 限制 request.count 只用于 outputs.when、动态输出、日志和 capture；它作为顶层 assert/input.assert 的 key 或 expected 引用时，启动校验失败。
- [ ] 实现 assert 列表和 `exists`、`absent`、严格比较、类型、包含、正则、长度、数值操作符；`name` 仅作为可选诊断名称。
- [ ] 拒绝 exists/absent 与 expected 或 default 混用；其他操作符在 default 替换 missing 后再做类型检查和比较。
- [ ] 先执行顶层 assert，再执行已选中 round 的 `input.assert`；局部断言不替换顶层断言，也不参与 round 选择。
- [ ] 拒绝 input 中的输出字段、output 中的 assert/when、outputs 候选中的 assert、round 直属 config、剧本 protocol/url/path/route、`rules` 包装层和旧变量语法。
- [ ] 验证断言失败不会尝试其他 round，相同请求重试仍定位到同一 round。

验收：错误输入返回包含剧本 ID、request.count（如已分配）、output_count、round.index、字段位置、实际值和期望值的诊断；请求 model 由框架解析，无需 YAML 断言。

### C. 输出和传输

- [ ] 以 [Output Steps 专项设计](./output_steps_design.md) 为唯一完整契约，实现 step 编译器、连接状态机和协议状态机。
- [ ] 实现配置回退：请求限制使用顶层 config → server.config → 内置默认值；输出配置使用被选中的 output.config → 顶层 config → server.config → 内置默认值。0、false、空 map 和空数组均视为显式值。
- [ ] headers 按层原子覆盖并校验 Go header 语法；同一 map 中规范化后重名的 header 直接拒绝。允许显式覆盖 Content-Type，拒绝 Content-Length、Transfer-Encoding、Connection、Trailer、Upgrade 和固定 CORS header。
- [ ] status 为 204、205 或 304 时拒绝 content、reasoning、tool_calls、error 以及任何实际 body send，只允许 headers、wait 和 end。
- [ ] 实现声明式 content、reasoning、tool_calls（含 `arguments` 内部叶子节点的系统 key 动态引用与确定性 JSON 序列化）和 error；由协议适配器自动生成 OpenAI、Anthropic、Responses 的流式或非流式容器及协议错误。
- [ ] 拒绝空 content、空 reasoning、空 tool_calls、空 tool name、空显式 tool ID 和空 error.message；空 body 通过省略四类输出字段表达。
- [ ] 有效 `config.stream: auto` 使用 `request.stream`；显式 true/false 强制对应模式。
- [ ] `tool_calls[].id` 省略时生成协议合法 ID，显式 ID 原样使用。后续请求可在自己的历史 assistant 输出和 tool result 之间直接断言 ID 相等。
- [ ] 在省略 steps 时按 output 内容生成默认过程：正常字段执行 send(output)/end，error 执行 send(error)/end，空 output 只执行 end；正常字段与 error 同时存在时要求显式 steps。
- [ ] 实现 output.steps：send、wait、end；send.source 支持 output、heartbeat、error、raw，wait.duration 支持普通 Go duration 或 infinite，end.mode 支持 normal 和 abort；自定义 steps 必须以 end 或无限 wait 结束。
- [ ] 明确自定义 steps 不会自动补 send 或 end；使用 send.target 的 payload.end、generation.end、stream.end 构造不完整流和缺结束事件，使用 wait.duration=infinite 构造挂起。
- [ ] 实现 send 的结构化 SSE、raw 原始负载、LF/CRLF、SSE comment heartbeat source，以及 stream.end 协议结束标记（OpenAI `[DONE]`、Anthropic message_stop）；heartbeat 不再设置只有单一取值的 format。
- [ ] 实现 send 逻辑游标和统一 selector 解析：内容块支持 `.start`、`.end`、`[:N]`，并保证 `[:完整长度]` 仍停在 `.end` 之前；HTTP 和生命周期块只提供 `.end` 里程碑。tool 使用显式 0-based 索引，payload 支持跨子块切片；OpenAI 支持 name delta，Anthropic/Responses 原子发送完整 name。
- [ ] 统一时间控制：duration 只表示 wait 动作持续时间；send.interval 只表示每个最终发送单元执行前的等待时间，包括第一项。未配置 chunks 时发送单元是协议记录，配置后是字节片；chunks 不再定义 interval，wait.duration 和 send.interval 都必须可被请求取消或服务关闭中断。
- [ ] 对每次 send 生成的完整字节序列实现 chunks 切分和逐片 Flush；size 与非空 sizes 必须二选一，sizes 只从完整序列开头应用一次，不在每条协议记录上重置。
- [ ] 实现 output.error 的 type/code/param/message（其中 message 和 param 支持系统 key 引用），以及 send.source=error 对同级 error 的引用；根据 status、protocol 和 stream 模式选择 HTTP error、stream error 或 Responses failed response。实现 source=raw 的 text/base64/fixture 三种互斥来源。
- [ ] 流式协议适配器对文本内容块按固定字符数切分 delta 记录；OpenAI 流式在 generation.end 阶段固定先发 `finish_reason` chunk，开启 usage 时再发 `{choices: [], usage: {...}}` chunk。
- [ ] 实现 max_output_bytes、max_fixture_bytes、max_generated_events 和 max_write_units 编译上限；max_output_bytes 统计单次请求全部有限 send 的正常输出、error、heartbeat 和 raw 总字节数，max_write_units 在 chunks 展开后限制最终 Write/Flush 单元。fixture 解析符号链接后必须仍位于剧本目录且是普通文件。
- [ ] `send.target: headers.end` 立即 Flush；分别验证头前挂起、头后 body 挂起、部分输出后挂起和完整协议后挂起。
- [ ] headers.end 在流式、非流式、空 output 和 error output 中都可使用；非流式 source=output 除 headers.end 外禁止所有显式 target。
- [ ] 实现 context 可取消的有限/无限 wait、`end.mode: abort` 的 HTTP/1.1 Hijack（配合 `SetLinger(0)` 发送 TCP RST）、接管连接登记和有界 shutdown。
- [ ] 使用真实 socket 验证 EOF、不完整流、客户端取消和断连；执行 Go race 检查。

验收：正常 output 和协议 error 都不需要编写 steps；超时、缺终止标记、非法 SSE、慢速分片、挂起和断连场景均可以由单份 output 的 steps 表达。

### D. 捕获与测试验证

- [ ] 实现单请求捕获和 `GET /_test/requests` 数组查询接口，记录 request ID、script ID、request.protocol、request.count、output_count、round.index、output index、条件、断言和步骤结果。
- [ ] 实现 `GET /health.json`、`GET /_test/scripts.json` 和 capture snapshot/verify/clear 接口；capture clear 只清理窗口，不重置单调 sequence 或请求计数。
- [ ] 实现 `GET /_test/scripts/{id}/counter.json`、`POST /_test/scripts/{id}/counter/reset.json` 和批量 `POST /_test/counters/reset.json`（支持可选 `script_ids`）；查询返回三个协议 counter 和总 active_requests，reset 一次清零三个 counter。请求准入与 reset 使用同一个每剧本临界区，保证 active_requests 检查和归零之间不能插入新请求。有活动请求时 reset 返回 409，进程重启并重新加载剧本时计数归零。
- [ ] 从剧本请求通过容量检查开始到全部退出路径结束维护 active_requests，覆盖 body 读取、断言失败、无限 wait、abort、客户端取消和服务关闭。
- [ ] 实现包含 instance_id 和单调 sequence 的 capture cursor、同时受 capacity/max_total_bytes 限制的窗口、body 截断标记、完整 header_values，以及 `capture_window_lost` fail closed；任一窗口上限超出时淘汰最旧记录，单条记录仍超预算时以 `capture_record_too_large` framework error 代替。
- [ ] 输出 JSONL 请求、断言、步骤、写入和结束原因诊断。
- [ ] 验证“剧本故意返回 503”和“Mock 自身断言失败”能被 verify 区分（支持 `script_ids` 过滤与 `allow_framework_errors` 负向测试豁免）。

验收：并发请求 headers 不串；清空 capture 不改变 request.count，显式 counter reset 后下一次通过输入校验的请求 count 为 1。

### E. 标准示例剧本集与内置端点

- [ ] 建立正常 Chat、Messages、Responses 声明式单轮示例剧本（流式与非流式）。
- [ ] 建立自动生成工具调用 ID 与工具参数动态引用的两轮剧本；第 2 次请求通过 `request.round_input` 与历史 assistant tool call 断言 ID 一致。
- [ ] 建立同一 round 多 outputs 的计数与条件匹配剧本，分别覆盖首次失败后成功、第 2 次失败、按 tools 条件分支和无匹配诊断。
- [ ] 建立异常传输示例剧本集，使用 `output.steps` 表达 send/wait/end，覆盖头前挂起、头后 body 挂起、部分输出后 EOF/挂起/TCP RST 断连、完整输出后挂起、缺 `[DONE]`/`message_stop`、慢速分片与 heartbeat 交错。
- [ ] 建立 HTTP 400/402/503、HTTP 200 逻辑失败和流式 error 事件的协议错误剧本。
- [ ] 建立 malformed SSE 和故意错误事件顺序的 raw 输出剧本。
- [ ] 实现内置端点 `GET /models` 和 `GET /v1/models`，返回固定 OpenAI model list；Models list 不进入剧本 DSL。
- [ ] 覆盖 usage 字段、prompt cache / cached_tokens 动态回填和多模态输入断言示例。
- [ ] 验证每个剧本固定生成 `/mock/{id}/v1/chat/completions`、`/mock/{id}/v1/messages`、`/mock/{id}/v1/responses` 三个标准形态 URL。

验收：所有示例剧本通过 `validate` 静态校验，并通过端到端 HTTP / 真实 socket 测试验证。

### F. 生命周期、构建与 CI

- [ ] 默认监听 `127.0.0.1:9999`；容器或远程访问由 `server.yaml` 显式配置。第一版拒绝端口 0。
- [ ] `serve` 处理 SIGINT/SIGTERM：第一次执行有界优雅关闭（停止接收新请求、取消挂起 wait、关闭 Hijack 连接），第二次信号或 `shutdown_timeout` 到期立即退出。
- [ ] 验证子进程启动、`GET /health.json`（含 `instance_id`）就绪探测、启动失败诊断和信号优雅退出。
- [ ] 提供标准的构建脚本 / Makefile 与 `.gitignore`，配置 CI 执行 `go vet`、`validate` 示例校验和 `go test -race ./...`。
- [ ] 编写仓库顶层 `README.md` 使用说明，涵盖快速开始、CLI 命令、剧本编写指南与管理接口。

验收：一键编译与测试通过；异常退出或关闭后不残留连接与 goroutine。

## 3. 后续事项

- [ ] 为 Responses `previous_response_id` 增加受控的 response-ID 历史索引；完成前要求请求内嵌可计数历史。
- [ ] 可选自签名 HTTPS listener，迁移现有 TLS 测试。
- [ ] 热加载、压缩响应和 HTTP/2 流重置。
- [ ] 发布机器可读的 JSON Schema，并提供编辑器补全。

多轮定位和计数匹配都属于第一版：request.output_count 选择 round，request.count 和其他系统 key 只选择该 round 内的 output。第一版不支持跨 round 条件跳转、随机返回和任意表达式脚本。
