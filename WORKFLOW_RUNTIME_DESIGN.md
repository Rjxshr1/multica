# Multica Workflow Runtime 架构设计

> 状态：设计评审稿，不包含实现
>
> 日期：2026-08-16
>
> 源码基线：`multica-ai/multica@4d495056e8ac638a8cd29784cc97128e26875c96`
> 设计目标：把 Workflow Studio 已经验证过、但仍依赖 metadata、评论和外围守护的可靠执行语义，下沉为 Multica 原生 Workflow Runtime。

## 一句话结论

保留 Workflow Studio 对业务流程的定义权；把运行状态裁决、Attempt、结构化证据、独立验证、CAS、fencing、依赖释放和事务 Outbox 集成进 Multica 核心。

Agent 从“状态写者”降为“结果与证据提交者”，Issue 从 Workflow Source of Truth 降为协作与展示投影。

## 文档导航

1. [当前架构与问题](docs/workflow-runtime/01-current-architecture.md)

   说明 Studio 现网链路、Multica 已有底座原语、当前权威事实分散在哪里，以及为什么外围补丁无法彻底解决问题。

2. [目标架构与责任边界](docs/workflow-runtime/02-target-architecture-and-boundaries.md)

   这是本轮最重要的评审文档。它回答“系统分几层、哪些保留在上层、哪些进入 Multica、改什么、为什么”。

3. [详细技术方案](docs/workflow-runtime/03-detailed-technical-design.md)

   给出数据模型、状态机、事务、CAS、fencing、依赖释放、Task Queue 集成、Verification、Outbox、失败恢复和 API 设计。

4. [演进与落地路线](docs/workflow-runtime/04-evolution-plan.md)

   描述从最小 PoC、Shadow 验证到按新 Run 切换权威的步骤、门槛和回滚方式。

5. [架构评审提纲](docs/workflow-runtime/05-review-guide.md)

   把关键设计决策、开放问题和建议评审顺序压缩成一份清单。

## 本轮已经做出的关键决策

| 决策 | 结论 |
|---|---|
| Workflow 业务定义放在哪里 | 保留在 Workflow Studio；Multica 保存每次 Run 的不可变执行快照 |
| Workflow 运行事实放在哪里 | Multica 新增 Workflow Runtime 结构化表，作为唯一权威 |
| Issue 的角色 | 人类协作、检索和展示投影；不再承载 authoritative phase/gate/generation |
| Agent 的权限 | 只能提交 Attempt Result、Artifact 和 Verification Result；不能直接推进 Node |
| 谁推进状态 | Multica 内的确定性 Workflow Reducer；每条命令在数据库事务内裁决 |
| 如何防并发错推 | per-run 事务锁 + Node revision CAS + Attempt fencing token |
| 如何防“Task completed = Workflow done” | Task 完成只令 Attempt 进入 `result_submitted`，Node 必须经过 Verification 才能 `succeeded` |
| 如何执行 Agent 任务 | 复用现有 `agent_task_queue`、Runtime Pool、prepare lease、runtime heartbeat 和 recovery |
| 是否再建一套运行租约 | V1 不复制 Task Queue 的执行活性判断；Workflow 层只新增防旧 Attempt 回写的 fence 与期限 |
| 如何释放 DAG 依赖 | Node 验证通过的同一事务内，以结构化边和前驱状态计算并释放后继 |
| 如何保证事件不丢 | 状态变更、Event 和 Outbox 同事务提交；发布端至少一次，消费端幂等 |
| 如何迁移现网 | 旧 Run 继续 Legacy；新 Run 可按开关进入新 Runtime；Shadow 期只比较，不建立双权威 |

## 范围

本设计覆盖：

- WorkflowRun、NodeExecution、ExecutionAttempt；
- ArtifactManifest、VerificationResult；
- 事务化状态机、CAS、fencing、幂等命令；
- Task Queue 生命周期集成；
- 原生 DAG 依赖释放；
- Retry、Recovery、Event Log、Outbox；
- Issue/Comment/metadata 投影；
- 从 Legacy/Shadow 到新 Runtime 的演进方式。

本设计暂不覆盖：

- Workflow Studio UI 重写；
- Planner 算法和 Prompt 设计重写；
- Runtime Pool、机器分发或模型供应商重新实现；
- 完整多租户计费、跨区域一致性；
- Controller artifact 的不可变发布体系；
- 直接替换线上现有 Run。

## 评审时最值得先确认的三件事

1. **责任边界是否正确**：业务语义留在 Studio，可靠执行原语下沉 Multica。
2. **权威是否唯一**：NodeExecution 是运行状态唯一权威，Issue/metadata/Comment 都是投影。
3. **完成定义是否可信**：Agent 运行成功、证据提交、独立验证、Node 成功是四件不同的事。

确认这三件事后，表结构和 API 细节都可以继续迭代；如果这三件事不成立，就不应进入实现。
