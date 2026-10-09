# 不打断 Agent 的控制面升级

托管部署由 `assistant-supervisor` 分别启动 `assistantd` 和 `assistant-runtime`。Agent 进程属于 Runtime 的进程组，不再属于控制服务；控制 WebSocket 断开不会取消 Run。Runtime 将活动、结果和终态先写入 `runtime.sqlite`，控制服务恢复后重连并按序补传。

候选版本由可信代码按文件路径计算 `restart_scope`：

- `CONTROL`：页面、任务源、工作流、路由等明确只属于控制面的变更。安装时短暂停止新调度和控制连接，Runtime 继续执行并缓存新事件；随后生成组合恢复点、替换控制端并启动新的 `assistantd`。正在运行的 Agent 与 Session 不变。
- `RUNTIME`：Adapter、Runtime、共享 RPC/模型、未知目录以及混合变更。安装前等待 Run 自然结束，再重启控制端和 Runtime。

`internal/server/hub.go` 即使位于 server 包也按 Runtime 影响处理，因为它属于控制端与 Runtime 的协议边界。候选中记录的范围必须与安装时重新计算的范围一致，否则拒绝安装。旧候选没有范围字段时按 `RUNTIME` 处理。

数据保护仍由控制端统一负责。Runtime 先打开 spool，随后控制端的启动恢复点同时备份 `control.sqlite` 和 `runtime.sqlite`；Runtime 在这种同机托管模式下不再启动第二个备份调度器，避免两个进程在同一目录产生只有半份数据的恢复点。独立部署的 Runtime 仍保留自身备份。

这次变更涉及 supervisor、升级协议、数据保护所有权和启动入口，属于不可自动演进的信任基础。现有单进程实例第一次采用它时必须：

1. 等当前 Run 自然结束并创建、验证组合备份；
2. 停止旧 supervisor；
3. 一次性替换全部五个二进制和对应源码，其中包括人工审查过的新 supervisor；
4. 启动后确认进程链为 `supervisor -> assistantd + assistant-runtime`，两库完整性检查通过；
5. 用测试 Run 验证只重启 `assistantd` 时 Runtime PID、Agent 子进程和 Session 不变，断线事件能补传；
6. 验证失败则停服并恢复旧基线，不覆盖数据库。

完成这次人工基线迁移后，普通 `CONTROL` 候选可以在业务 Run 执行期间安装；以后再次修改 supervisor、升级判定、备份门禁或 Runtime 协议，仍走人工基线发布。
