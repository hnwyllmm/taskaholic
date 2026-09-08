# 受控 Windows 验证执行器

第一版只支持已完成人工方案审批的 `oceanbase/seekdb` 开发任务，以及
`windows_seekdb_phase0` profile。它不是任意远程命令接口，不改变 Agent 的沙箱配置。

## 流程

开发 Agent 返回 `outcome=blocked` 和
`environment_request={"profile":"windows_seekdb_phase0","reason":"验证理由"}`。
Manager 创建原任务下面的执行子任务，父任务等待。子任务沿用原成员的 runtime
归属，但 runtime 不调用模型，直接执行预置的 Windows 驱动。

执行器读取原 Session 已登记 worktree 中
`tools/windows/long_path_phase0/` 的六个固定文件，拒绝符号链接和超限文件，
冻结快照、计算 SHA256。只读取宿主配置的工具链和 SQLite 头文件、LIB、DLL，
三者均校验固定 SHA256。预检失败可重试两次；实际提交不盲目重试。

Windows 使用 `C:\work-assistant\jobs\<子任务ID>` 独立构建目录，专用文件锁串行化。
启动 VS 编译环境，构建探针并验证 manifest，再用新进程分别验证
`LongPathsEnabled=0/1`。配置必须明确授权策略切换。切换前落盘恢复日志，
finally 恢复并读取验证原值。异常中断的日志由下一次取得锁的执行恢复。
机器断电/进程被强杀不能保证 immediately 恢复；遗留恢复日志及不确定回执
必须核对，不能作为成功或直接重复执行的依据。

结果作为子任务产物落入数据库，包含结论、源码摘要和日志尾部。
完整单次 Windows 构建/测试文件留在虚拟机的 job 目录；Linux 保留快照、回执
和有界日志于 `WorkRoot/windows-jobs`。数据库备份包含任务和报告；这两个目录
如需完整复现也应纳入主机/虚拟机备份，不能仅依赖数据库备份。

子任务完成表示执行报告已交付，不表示测试通过或产品已完成，不新增人工验收。
Manager 将结果作为系统事件送回原开发 Session，保留原方案批准。
暂停或改变批准范围后返回的结果只记录，不自动推进。每个父任务最多自动申请三轮。
此限制是防止重复失败，不替代真正缺失的权限/依赖决策。

## 宿主配置

`WorkRoot/windows-profiles.json` 为 profile 名到配置的映射。字段：
`winrm_command`、`cmake`、`clang`、`ninja`、`vs_dev_cmd`、
`sqlite_include`、`sqlite_library`、`sqlite_header_sha256`、
`sqlite_library_sha256`、`sqlite_dll_sha256`、`allow_policy_switch`。

WinRM helper 由宿主维护并自行读取受保护凭据；不把凭据写入 Agent 指令、
请求或日志。测试源码在专用虚拟机账户下执行，并不是新的 Windows 安全沙箱；
因此虚拟机应专用，不应包含不相关的敏感资料。

dev 入口 `deploy/dev/windows-run` 使用 WinRM stdin 分块传输，避免 Windows
命令行长度限制。按已验证的 win11 MAC 从 root 拥有的只读 DHCP 租约文件发现
IP，不调用 sudo，兼容后台服务的 NoNewPrivileges。更换虚拟机或网络时需要
管理员更新此宿主入口的身份配置。WinRM 请求不经过模型的 HTTP 代理。

已有受阻任务可由用户显式调用
`POST /api/v1/work/tasks/{id}/environment`，带当前 `expected_version`、
`profile` 和 `reason`，直接发起执行子任务，避免通过普通聊天重新触发方案审批。
只有当前审批仍有效、没有运行或待处理输入的受阻开发任务可使用。
