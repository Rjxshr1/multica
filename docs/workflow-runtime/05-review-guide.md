# 05｜架构评审提纲

## 1. 建议评审顺序

不要先看表字段，按以下顺序看：

1. 系统分层是否成立；
2. 每个能力归 Studio、Workflow Runtime 还是现有 Task/Runtime；
3. 谁是唯一权威、谁只是投影；
4. 状态为什么不会被并发、重复、乱序和旧 Attempt 破坏；
5. Task completed 为什么不等于 Node succeeded；
6. 迁移过程是否会出现两个正式写者；
7. 最后才看表和 API 是否需要调整。

## 2. 本方案把系统分成几块

| 块 | 职责 | 不负责 |
|---|---|---|
| Workflow Studio | 定义业务 DAG、阶段、Prompt、Agent/Model、Repo、验收和人工策略 | 不直接写运行状态 |
| Multica Workflow Runtime | Run/Node/Attempt、CAS、fence、Artifact、Verification、依赖、Retry、Event/Outbox | 不理解 coding/review 等具体业务含义 |
| Multica Task/Runtime | Agent Task 排队、机器分发、claim、执行活性、runtime recovery | 不裁决业务 Node 是否成功 |
| Issue/Comment Projection | 给人看、协作、搜索、兼容现有 UI | 不是 Workflow Source of Truth |
| Reconciler/Supervisor | 检查结构化不变量、诊断新故障、改进 policy | 不成为第二个 reducer |
| Context Gateway | 为每次 Attempt 固化上下文快照，只注入最小启动上下文，其余内容通过只读接口按需查询 | 不让完整 Issue、历史评论和所有产物默认进入 Prompt |

## 3. 上层保留什么

- Planner 与 DAG authoring；
- `coding/self_test/cross_review/test_env_verify/diff_reason`；
- Agent、Model、Prompt、Skill 和 Repo 策略；
- 业务验收、blocker、human gate；
- remediation 分类与预算；
- Diff Analyzer、Bad Case、Supervisor；
- 面向用户的解释和报告。

理由：这些是产品/领域语义，应该快速迭代，不适合硬编码到通用底座。

## 4. Multica 里改什么

### 新增

- WorkflowRun；
- NodeExecution + Dependency；
- ExecutionAttempt + Task binding；
- ArtifactManifest；
- VerificationResult；
- Event Log + Transactional Outbox；
- Workflow command/reducer、projector、reconciler；
- Task-scoped Context Snapshot 与只读 Context Gateway；
- inspect/运维 API 与 CLI。

### 修改

- Task enqueue：Attempt 与 Task 同事务绑定；
- Task claim/start/complete/fail：绑定 Attempt 的生命周期同事务结算；
- Task retry：Workflow-owned Task 关闭 Task auto-retry；
- Workspace cleanup、Router、Realtime、API schema。

### 复用不重写

- Runtime Pool、机器分发；
- `agent_task_queue`；
- prepare lease、runtime heartbeat、orphan recovery；
- failure taxonomy；
- Workspace/Auth/Task token；
- Autopilot trigger、Event Bus、WebSocket。

## 5. 为什么这么改

| 当前痛点 | 设计回应 |
|---|---|
| metadata 多字段半写、无 CAS | Node 单行 revision CAS |
| 单 Scheduler 才能避免冲突 | per-run DB transaction lock，允许多实例 |
| 旧 Worker 晚到 | active_attempt + monotonic fence |
| Task completed 被误当完成 | Attempt result 与 Node verification 分层 |
| 自然语言评论承担协议 | structured Result/Artifact/Verification；Comment 只展示 |
| 依赖靠字符串和周期扫描 | normalized edge + terminal transaction release |
| 状态写了但事件丢了 | Event/Outbox 同事务 |
| Guardian 大量推理世界状态 | Reconciler 只检查结构化不变量 |
| Task retry 与 Workflow recovery 分散 | Workflow-owned Task 只有 Workflow policy 一个 retry owner |
| 长 Workflow 每次重试都重复携带完整历史 | 只注入当前任务和工作流概要；依赖结果、历史 Attempt、验证记录和产物通过只读接口按需查询 |

## 6. 上下文为什么能同时降低 Token 和耗时

旧架构会把 Issue、评论、前置节点结果、验证记录和产物尽量拼进每次 Agent Prompt。Workflow 越长、重试越多，重复传输越严重；模型还要在大量无关历史中找当前任务真正需要的信息。

新架构把上下文改成三层：

```text
L0 最小上下文
  默认只带当前任务 + 工作流概要，保证 Agent 可以立即开始

不可变上下文快照
  前置节点、历史 Attempt、Verification、Event、Artifact 固化为带 revision/digest 的目录

按需查询接口
  Agent 需要时调用 context_catalog / context_search / context_get / context_dependency
```

这不是简单地用模型生成一份摘要，而是“结构化裁剪 + 不可变快照 + 按需检索”。Session 复用进一步避免同一个 Issue 在连续节点中重复建立模型、仓库和系统指令上下文。

稳定性来自四个限制：查询只读、只允许访问当前 Task 的快照、整个 Attempt 绑定同一 `snapshot_id/revision/digest`、单项和搜索结果有硬上限。这样既不会因为查询期间上游数据变化产生上下文漂移，也不会让 Agent 通过接口改写正式状态。

当前证据需要分两类表达：

| 证据 | 结果 | 能证明什么 |
|---|---|---|
| 6 类真实任务 A/B | 总 Token `566,178 → 390,372`，降低 31%；双方都完成任务的中位耗时 `20.5s → 14.5s` | 上下文按需检索 + Session 复用整体策略在真实模型调用中的收益；不能把 31% 全部单独归因于 Context Gateway |
| 10 MiB 确定性上下文专项 | 单 Attempt 载荷 `10,448,874 → 15,287 bytes`，降低约 99.85%；10 并发 P95 `222.9ms → 38.5ms` | Context Gateway 对数据库读取、应用扫描和 JSON 序列化的收益；不等于供应商真实计费 Token 或端到端模型耗时 |

简历可写：

> 构建任务级不可变上下文快照与只读按需查询接口，仅默认注入当前任务及工作流概要，并结合单 Issue Session 复用，减少长链路重复上下文传输；真实任务 A/B 中 Token 消耗降低 31%、共同完成任务中位耗时降低 29%。

`10 MiB / 99.85%` 作为面试追问时的专项证据保留，不放进简历主句，避免把上下文载荷和供应商计费 Token 混为一谈。

### 6.1 Multica 相比本地直接运行，多出来的时间在哪里

```text
任务创建
→ WebSocket 唤醒 / Batch Claim
→ 鉴权与任务载荷构建
→ Runtime 路径和版本解析
→ Multica Skill 下载、校验、缓存
→ Remote MCP broker 与配置生成
→ Workdir / Session / Skill sidecar 准备
→ StartTask 回写
→ 启动 Agent CLI 进程
→ CLI 扫描本地 Skill、插件和 MCP
→ 模型首个有效输出
→ 工具执行与结果回写
```

不能用“任务总耗时 - 模型 API 耗时”笼统归因。新打点会分别记录 `runtime_resolve_ms / skill_resolve_ms / remote_mcp_ms / env_prepare_ms / start_task_rpc_ms / backend_setup_ms / ready_to_execute_ms / backend_start_ms / after_process_start_ms`，把平台准备、进程冷启动和模型首包拆开。

当前已落地的优化：

- Skill 在 Runtime Brief 中只保留名称索引，不重复描述和正文；已有真实任务测量约减少 3,100 Token，占旧 Brief 约 40%。
- Multica Skill 首次下载保持“每个 Skill 独立请求、独立校验、独立缓存”，由串行改为最多 4 路并发；单个失败仍不会清空其他成功缓存。
- Codex 本地 Skill 使用链接而不是按任务复制；已有 Skill Bundle 走磁盘缓存。
- 同一 Issue 优先复用 Workdir 和 Session；Runtime 路径、版本探测和升级自愈结果做缓存与并发合并。
- WebSocket `pending_work` 与 Batch Claim 缩短轮询等待，HTTP 只作为兼容和恢复路径。

Skill 专项 A/B（真实 HTTP、校验和缓存写入；服务端固定增加 10ms/Skill，不调用模型）：

| 未缓存 Skill 数 | 旧串行 | 4 路有界并发 | 降低 |
|---:|---:|---:|---:|
| 4 | 44.2ms | 11.6ms | 73.7% |
| 8 | 87.1ms | 23.4ms | 73.2% |
| 16 | 175.9ms | 46.0ms | 73.8% |

另外两组定位结果说明不应把所有慢都归因于 Skill 文件：Multica 环境准备从 0 个 Skill 的 0.37ms 增至 64 个 Skill 的 11.9ms；Pi 离线冷启动从无 Skill 的中位约 0.40s 增至显式加载 50 个真实 Skill 的约 0.44s。当前更大的固定成本是每次新起 Agent CLI 进程，Pi 本机约 0.4s；下一阶段可对支持 RPC/ACP 的 Runtime 做受控热进程池，但必须先解决工作目录、Session、MCP 凭证、取消和版本升级隔离，不能跨任务复用可变状态。

“完整 Skill 正文按需取”暂不作为默认路径：名称索引已经存在，而本地扫描新增开销当前只有约 40ms；若把下载失败从准备期移动到 Agent 执行中，还会降低可诊断性和稳定性。更合适的演进是只对超大 Workspace Skill 启用 task-scoped、版本固定的 `skill_catalog / skill_get`，Builtin、Plugin 和脚本型 Skill 继续预取，并通过灰度比较首包耗时、任务成功率和 Skill 命中率后再扩大范围。

## 7. 一致性证明的核心链条

```text
per-run transaction lock
  保证一个 Run 内命令有确定顺序

Node expected_revision CAS
  拒绝基于旧读的命令

active_attempt_id + fence_token
  拒绝旧 Attempt/旧 Worker 晚到结果

Task terminal + Attempt settlement 同事务
  消除 Task 已完成、Workflow 未记账的窗口

Verification PASS + Node success + dependency release + run rollup 同事务
  消除完成与下游释放之间的窗口

Event + Outbox 同事务
  消除状态已变、事件意图丢失的窗口

idempotency key + unique constraint
  把网络重复投递收敛为同一事实
```

## 8. 稳定性证明的核心链条

```text
Task 执行活性
  复用 prepare lease + runtime heartbeat + orphan recovery

Reducer 崩溃
  数据库事务回滚；没有半状态

Publisher 崩溃
  Outbox lease 过期后重新投递

Projector 崩溃
  Workflow 权威不回滚，按 revision 补投影

Verifier 崩溃
  Verification attempt 重试，不能降级成 executor 自证

未知不一致
  Reconciler 用相同 Reducer command 收敛，不 direct SQL 修状态
```

## 9. 我建议本轮明确拍板的问题

### Q1｜Issue 是否仍然是 Workflow 权威？

建议：否。Issue 继续存在，但只做投影和协作入口。

如果 Issue 仍是权威，NodeExecution 只会变成又一份 Shadow 数据，核心问题不变。

### Q2｜Workflow Definition 是否也要完全放进 Multica？

建议：作者态留 Studio；Multica 只保存每次 Run 的不可变、可执行 snapshot。

这样既不把 Studio 业务硬编码进底座，也防止运行中定义漂移。

### Q3｜是否给 Workflow Attempt 再建 per-task heartbeat/lease？

建议：V1 不建。复用现有 Task/Runtime liveness，Workflow 增加 deadline + fence。

等支持非 Task Queue executor 时，再按 executor kind 增加唯一 lease provider。

### Q4｜Retry 由谁负责？

建议：Workflow-owned Task 设置 `max_attempts=1`，Workflow policy 创建新 Attempt；Legacy Task 行为不变。

### Q5｜是否需要 independent verifier？

建议：PoC 必须需要。这是证明“从 Agent 自述驱动到证据驱动”的核心，不应降级为可选展示。

### Q6｜依赖释放用计数还是查询前驱？

建议：V1 在 per-run lock 下查询 normalized edge + predecessor state；不把易漂移计数作为唯一真相。

### Q7｜迁移要不要回填旧 Run？

建议：不把推断出来的旧历史回填成新权威。旧 Run 走完 Legacy；新 Run 从创建开始进入新 Runtime。

## 10. 可以后置的开放问题

这些不阻塞 PoC：

- 多 verifier 的 `all/any/quorum` policy；
- `any_success/all_terminal` 等 edge condition；
- Human Gate 的复杂组织授权；
- Artifact blob 存储与长期 retention；
- Controller artifact 不可变发布；
- 多区域和超大 DAG 优化；
- 是否向上游 Multica 拆分多个 PR。

## 11. 评审结论模板

评审后可直接记录：

```text
责任边界：接受 / 调整
唯一权威：接受 / 调整
Task 与 Workflow retry owner：接受 / 调整
Verification 独立性：接受 / 调整
并发模型（run lock + CAS + fence）：接受 / 调整
状态/事件事务边界：接受 / 调整
迁移方式（新 Run 切换，不双权威）：接受 / 调整
PoC 范围：接受 / 调整

需要修改的决策：
1. ...
2. ...

允许进入实现：是 / 否
```
