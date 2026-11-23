# 配置驱动 Go Mock Server

> 状态：设计中，尚未实施。

## 文档职责

| 文档 | 回答的问题 | 不负责什么 |
|---|---|---|
| [产品设计](./step1_product.md) | 用户如何使用、第一版提供什么、如何验收 | 不定义完整 YAML 字段或内部模块 |
| [配置参考](./configuration_reference.md) | server.yaml、剧本 YAML、系统 key 和继承规则怎么写 | 不解释 Go 内部架构 |
| [技术设计](./step2_technical.md) | Go 服务如何加载、选择、编译、执行、并发和关闭 | 不重复字段级 Schema |
| [Output Steps](./output_steps_design.md) | send、wait、end 的字段、状态机和协议边界 | 不定义 round/output 选择 |
| [场景矩阵](./current_mock_coverage.md) | 两级选择语义、系统 key 边界与典型场景配置速查 | 不作为配置契约 |
| [实施任务](./step3_tasks.md) | 按什么顺序实现和验证 | 不重新定义设计 |

## 建议阅读顺序

```text
产品设计
→ 配置参考
→ 技术设计
→ 需要异常传输时阅读 Output Steps
→ 场景矩阵
→ 实施任务
```

出现不一致时，按关注点使用对应的唯一事实来源：YAML 字段以配置参考和 Output Steps 为准，内部行为以技术设计为准，实施状态以任务清单为准。
