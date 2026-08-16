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

## 6. 一致性证明的核心链条

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

## 7. 稳定性证明的核心链条

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

## 8. 我建议本轮明确拍板的问题

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

## 9. 可以后置的开放问题

这些不阻塞 PoC：

- 多 verifier 的 `all/any/quorum` policy；
- `any_success/all_terminal` 等 edge condition；
- Human Gate 的复杂组织授权；
- Artifact blob 存储与长期 retention；
- Controller artifact 不可变发布；
- 多区域和超大 DAG 优化；
- 是否向上游 Multica 拆分多个 PR。

## 10. 评审结论模板

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
