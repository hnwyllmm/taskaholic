# 任务源部署验收记录

日期：2026-09-07，Asia/Shanghai。功能提交：`ffc0831`。部署目录保持 `/data/wangyunlai.wyl/workspace/work-assistant`，Runtime 仍为 `dev`，同时保留 Cursor / Codex。正式服务仍由现有 systemd 用户服务与 supervisor 托管。

## 测试与只读接口验证

- 本地全套 `go test -race -p 2 ./...`、`go vet ./...` 通过。首次默认并发的 race 测试中，既有 Codex 模型探测耗时断言超时；该测试单独重跑与降低包并行度后的全量重跑均通过，没有放宽超时或修改生产逻辑。
- dev 全量 race 测试与 vet 通过；最终源码增补后，对 store / tasksource / server 再次运行 race 测试通过。5 个正式程序均完成构建。
- Node UI / 部署辅助测试 46 项通过。
- 覆盖精确迭代/指派人筛选、分页失败不推进游标、ETag/304、限流、分页域名限制、重复事件、数据库重开/备份恢复、原 Session 续接、人工暂停、完成任务只记录、评审 fan-out/fan-in、旧 SHA 失效、PR 归属冲突和控制凭据隔离。
- 使用 dev 现有登录身份只读验证 AntMultica：匹配 `SEEK-478` 一项；连续两次规范化读取，第二次新增事件为 0。
- 使用 `https://github.com/oceanbase/seekdb/pull/1360` 只读验证 GitHub API：head 为 `0b477a9ed4fb94d197e8c6b4b26ef28acd3d74c6`；首次观测 1 个 head 事件，第二次新增事件为 0。该 PR **没有登记到正式系统，也没有向 GitHub 创建评论、评审请求或其它对象**。

## 数据迁移与恢复点

在正式服务停止前，先对恢复点副本运行真实 Store.Open，演练 schema 12 → 13。核对所有原表的每条记录不变，新表为空，仅追加 schema 13；正式重启后再核对原数据与运行端 spool，系统成员绑定不变。运行存在或数据在停止过程中变化时部署脚本会中止；失败后不会用旧数据库覆盖新数据。

备份目录：

`/data/wangyunlai.wyl/work-assistant-backups/dev/e688631bf5cc20df2aa521fd`

均已运行 `assistantctl backup-verify`：

- 升级前：`snapshot-20260907T054117.146914388Z-2851550298`。
- 升级后、启用任务源前：`snapshot-20260907T054121.290915555Z-3444801982`。
- 启用任务源并导入工单后：`snapshot-20260907T054519.468474989Z-2772866425`。

演练、原始程序副本与私有记录保存在 `.deploy/sources-20260907-a9WvAt/`；这些文件不入 Git。没有删除旧备份、修改 CLI 凭据、覆盖原生 Session 文件、创建新系统服务或改动网络认证。

## 正式启用与页面

通过 `seed-task-sources.mjs --apply` 创建了两项新配置，不覆盖现有配置：

- AntMultica：SeekDB、迭代 `1.5.0`、精确指派给宁封，每 60 秒读取；使用 SeekDB / seekdb-bindings 开发者角色自动分派。
- GitHub：每 5 秒检查已登记 PR；默认架构、QA/测试、通用代码三类 reviewer 角色。此时没有正式登记 PR。

`SEEK-478` 已导入为 `task_1788759773057_589a3908336a07e73b93`，自动分派给现有 `agent_1788750330049_5570ad5f83d16edd48ec`。验收时该成员的实际配置为 Codex / gpt-6-astra，真实 Run 已进入 RUNNING。多轮轮询后仍只有 1 个该工单任务、1 个源事件，没有源错误。源创建后出现的新任务/Run 是预期业务变化，不属于迁移时的历史数据改写。

实际浏览器已验证页面加载、导航、状态、导入事件链接、AntMultica 编辑表单和 GitHub 配置表单；截图显示正常。迭代修改只在草稿中尝试后取消，正式配置保持 `1.5.0`。页面控制台当时无错误。最后一轮额外的复选框/PR 弹窗浏览器检查连接超时，未将该轮算作通过；相应表单提交和原任务筛选已由 DOM 测试、HTTP 测试验证。

systemd 验收状态：active/running，MainPID `572417`，NRestarts=0。已有免登录 LAN/VPN 访问方式不变；运行端认证仍保留。

## 后续真实评审与分页补验

工单 Agent 在只读调查中识别了已有且明确关联 SEEK-478 的 PR `https://github.com/oceanbase/seekdb/pull/1358`，通过结果中的 pull_requests 将其登记到原任务；不是本次新建的 PR。源在 head `e6d49057970e29768a6467ba8557d5235c052781` 上自动创建了架构、QA、通用代码三个评审任务。架构与 QA 已完成；通用 reviewer 已交付报告但返回 blocked，原因是自身沙箱无法取得当前 CI/PR/工单在线材料。系统保留报告且没有把 blocked 算作通过，原任务仍等待子评审，不自动批准或合并。

最后通过真实 GitHub Link 验证发现分页使用 `/repositories/1080442728/`。补充提交 `b12e0dd`：从 PR 的 base.repo.id/full_name 校验该 ID，允许同仓库的数字路径，仍拒绝其它仓库、未确认的 ID 和其它域名。本地/dev provider race 测试通过，dev store/server race 与 vet 再验通过。只读探针对 PR #1358 使用 `--page-size 10`，实际读取 1 个数字仓库分页，第二次轮询新增事件为 0；未向 GitHub 写入。

等待全部运行结束后安装分页补丁，没有中断评审。再次核对 schema 仍为 13、任务源配置和系统绑定不变、原任务与原生 Session 引用保留，健康检查通过。新旧程序副本位于 `.deploy/github-pagination-20260907-HHCn6H/`。补丁前后恢复点均通过 backup-verify：

- 补丁前：`snapshot-20260907T060727.165608554Z-1919379888`。
- 补丁后：`snapshot-20260907T060731.220034727Z-331591768`。

## 未扩大到的范围

没有开放普通 Agent 的仓库写入、推送、评论发布、PR 合并权限。自动 reviewer 完成只表示其内部评审报告交付，不等于开发任务已获人工批准。轮询以实际观测到的 head 为版本；不是每个瞬时 push/中间 commit 的无遗漏捕获。原生 CLI Session、远端仓库和 CLI 凭据仍不包含在应用 SQLite 备份内。
