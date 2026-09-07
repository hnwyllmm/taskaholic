# 推理强度

成员管理中，模型控件下方提供 `reasoning effort` 下拉框。创建和修改成员均支持；不改变既有角色、模型、权限或系统岗位绑定，不自动替团队选择更高档位。

- 默认值为空，表示不覆盖原生 Agent / Session 的配置。模型目录的推荐默认值不等于机器的实际生效值；系统不把空值猜成 medium，也不反查或接管原生 Session 记忆。
- Codex 通过安装的 App Server `model/list` 获取 `supportedReasoningEfforts` 和 `defaultReasoningEffort`。执行及 resume 都显式传入 `-c model_reasoning_effort="…"`。
- Cursor 当前通过 `agent models` 发现真实可选的模型变体。只提供列表内同系列的推理档位映射，Fast 与非 Fast 分开保留；不虚构模型 ID，不替未知模型生成未经验证的 bracket 参数。`auto` 或未提供档位的模型继续使用原配置。原模型输入仍支持手动填写。
- 新增插件仍通过可选 ModelProvider 扩展列表，需声明 `reasoning_effort` 能力，并负责执行该配置。档位由模型提供，不维护全平台统一枚举。

## 配置和生效时机

成员 API 的 `reasoning_effort` 是可选字符串。PUT 省略字段时保留原配置；显式空字符串表示不覆盖。旧客户端换模型但未提供该字段时，恢复默认以免把旧模型档位传给新模型。新建或改变非空值时，经所选 Runtime 验证；未知模型、未知档位和不能验证的离线变更会被拒绝。

优先级：本次 Run 明确覆盖 → 同模型成员默认 → 原 Session 初始模型/强度快照（成员换模型后的旧 Session）→ 原生默认。低层 `/tasks/{id}/runs` 接口支持单次 `reasoning_effort` 覆盖；托管工作继续通过原有调度流程执行。第一版没有增加任务级 UI，也不由 Router 自动选择强度。

修改成员强度影响下一次创建的 Run，包括工作、首页聊天、角色设计、路由和验收聊天。已经排队/运行的 Run 及该 Run 内的 Directive 后续轮次保留原快照。原生 Session ID、任务归属、固定模型和角色版本不变。升级构建器同样读取已保存的成员快照。

## 持久化与验证

控制库 schema 11 → 12 仅给 `run` 添加 `execution_json`，事务化迁移，迁移前强制备份；旧记录为 `{}`，不补写猜测值。成员复用 `agent_profile.data_json`；初始执行默认值复用 Session metadata。无需新数据库或网络协议。

Run、持久化 run.start 消息和 RunQueued 事件记录请求的强度及来源。CLI 启动后上报 run.configured，保存传递给 CLI 的模型 ID 和配置确认；这不是对模型内部推理过程或最终服务端参数的观测。默认档位仍标为未报告。Cursor 变体的执行模型与 Session 固定选择分开记录。

任务运行历史显示强度；完成总结使用最终交付 Run 的配置，不能被后来修改的成员或验收聊天覆盖。现有 SQLite 快照完整覆盖新增数据。旧二进制会拒绝 schema 12，不能直接覆盖程序强行降级，更不能通过还原旧库删除备份后的记录。

自动测试覆盖能力发现、参数与原生 Session 续轮、无效选项拒绝、配置优先级、历史快照、验收总结及备份重开。`node deploy/dev/reasoning-smoke.mjs` 是显式执行的 dev 隔离集成检查，使用临时数据目录和随机本机端口验证真实 Codex low → high 续聊；会调用模型，不访问正式数据库。

协议依据：[Codex 模型发现](https://learn.chatgpt.com/docs/app-server#models)、[Codex 配置项](https://learn.chatgpt.com/docs/config-file/config-reference)。Cursor 以 dev 已安装 CLI 的模型列表与帮助信息为准。
