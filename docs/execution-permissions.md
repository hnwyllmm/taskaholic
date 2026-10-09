# 执行权限

当前接入 `development.execute`、兼容旧流程的 `windows_seekdb_phase0`，以及
开发 Agent 主动申请的 `agent.network_access` 和 `agent.host_full_access`。
权限由 Manager 持久化检查，不通过提示词自我授权。
其他执行器可按同一结构扩展，但 GitHub 评论、GitLab pipeline 和系统升级目前
仍使用原有边界，不宣称已被这个页面统一管理。

## 策略与等待

策略范围为操作、runtime、仓库；机器和仓库支持精确值或完整 `*`。
任何匹配的 deny 优先；否则最具体的规则优先，等具体度 ask 优先。
未配置时 development.execute 沿用现有“方案已批准才执行”的行为；Windows 默认 ask。
迁移不将“宿主已有工具/凭据”解释为全局预授权；用户在首个授权卡片选择记住范围后，后续相同范围不再询问。

Windows 子任务在创建 Run/outbox 前检查 runtime 声明的执行器能力。
缺环境进入 WAITING_ENVIRONMENT，每 30 秒重新检查，不新增授权单或调用模型。
这属于调度前检查，不是从自然语言推断所有后续步骤，也不自动迁移已有 Session。
声明反映最近连接时的配置；工具链/连接/真实测试由执行器预检和执行结果确认。

需要确认进入 WAITING_AUTHORIZATION，保留未投递输入，停止调度忙循环。
首页及 `/permissions` 可以批准一次、拒绝、或批准并保存精确范围预授权。
批准后同一个任务继续调度，同一 Session 沿用原上下文，方案审批独立保留。
策略变化不抢占运行；待授权记录可显式“按最新规则重新检查”。

若已批准实施的任务处于 `BLOCKED`，但 Agent 没有按协议提交结构化
`capability_request`，任务页会提供“主动授权运行能力”：用户可明确选择
`network_access` 或 `host_full_access`，一次放行或记住当前 runtime/仓库范围。
这是恢复路径，不从报错或 Agent 自然语言猜测权限。每次主动授权仍绑定当前
任务版本、原 Agent/Session、仓库、plan hash 和批准 review；它只会续跑原任务，
不能授权任意命令、sudo 或修改方案。已有待授权单时必须处理该单，不能创建第二张。

一次性授权绑定 task、执行成员、runtime、操作、仓库、方案 hash、review ID、
待处理消息序号和任务 revision。批准使用版本 CAS，放行与 Run/outbox 同事务消费。
暂停或用户修改要求使未消费授权失效；新的输入/方案/执行范围不能复用旧授权。
拒绝和失效保留记录。没有宿主执行器/宿主权限上限时，页面批准也不能凭空提供能力。

开发 Agent 在已批准方案的实施阶段可通过结构化 `capability_request` 申请能力。
批准后 Manager 将一次性能力写入下一轮 `ExecutionGrant`，Codex 沿用原生 Session：

- `network_access`：保留 workspace-write 文件沙箱，允许该轮网络访问。
- `host_full_access`：高风险，使用 Codex danger-full-access；仅在确需宿主机操作时批准。

两者都绑定 task、Agent、runtime、仓库、plan hash 和 review ID。Agent 输出的申请
本身不是授权；审批前不会创建有扩展权限的 Run。一次性授权在 Run 入队时消费。

## 存储/API

SQLite schema 19 新增 execution_policy、permission_request，附带事件审计，
沿用数据库备份/恢复。迁移不改原任务、Session、审批或产物。

- GET `/api/v1/execution-permissions`：规则、最近授权记录、runtime 能力。
- PUT `/api/v1/execution-permissions/policies`：精确范围 CAS 更新，version 为当前版本。
- POST `/api/v1/execution-permissions/requests/{id}`：expected_version、decision
  (`approve` / `deny` / `recheck`)、remember。

预授权不会替代人工方案确认、最终验收或系统升级确认，也不改变 Agent 沙箱、
后台服务 NoNewPrivileges、凭据文件权限和网络隔离。
