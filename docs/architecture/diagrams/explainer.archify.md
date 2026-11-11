# 影策（晶电影工作室）如何把创作意图变成受控的影视与电商产物？ {#yingce-architecture}

影策将无限画布、生成队列、Pi Agent、短剧与电商收敛在 **Go 组合根与 SQL 状态机** 下执行。模型路由、扣费结算与资源授权均在服务端闭环。

- 前端按用户 scope 隔离 React Query 与 localforage，生成结果用租约 fence 去重后再写回画布。[](source:materializer)
- 异步任务以数据库 Task 行与 45 秒租约为持久真相源，Redis 仅承载并发槽、限流与熔断协调。[](source:task-claim)
- Pi Agent 运行在独立 Node 进程，经 `/model`、`/tool`、`/event` 三条桥回调 Go 校验权限与审批。[](source:pi-coordinator)
- 短剧以剧情段（`DramaSegment`）为出片单位，按四类并发池调度，并在循环外后台抽取尾帧。[](source:drama-dispatch)

@answer scope="v1.8.2 基线 · Go + React + Node Pi · SQLite/PostgreSQL"
@meta locale=zh-CN repo_url=https://github.com/newyngwieslash-ops/jing-studio-canvas.git repo_revision=6ab49b692d6ebfd267826d7117f11100f8128af0
@source main-entry title="Go 服务端启动与目录初始化" path=backend/cmd/server/main.go lines=42:120
@source materializer title="前端生成结果物化与租约去重" path=web/src/services/generation-task-materializer.ts lines=67:89
@source task-claim title="数据库任务领取与租约条件更新" path=backend/internal/repository/repository_tasks.go lines=53:101
@source pi-coordinator title="Pi Agent 协调器与审批后恢复" path=backend/internal/app/cloud_agent_pi_coordinator.go lines=27:94
@source tool-policy title="Agent 工具分类与可见性策略" path=backend/internal/agent/tool/policy.go lines=40:90
@source media-batch title="Agent 批量媒体生成与试跑状态机" path=backend/internal/app/cloud_agent_media_batch.go lines=26:90
@source drama-dispatch title="短剧四类并发池与集号优先级" path=backend/internal/app/drama_dispatch.go lines=14:60
@source drama-tail title="短剧段视频后台尾帧抽取与租约" path=backend/internal/app/drama_tail_worker.go lines=12:76
@source ark-portraits title="方舟素材库三类素材组与安全边界" path=backend/internal/app/ark_portraits.go lines=35:65

## 系统由哪些运行单元组成？ {#system-topology}

@diagram architecture id=system-map source=system.architecture.json title="影策核心运行拓扑"

@steps id=system-steps
| step-workbench | 创作工作台 | 承载无限画布、短剧制作台、电商五步工作区与 3D 预演台，按账号 scope 隔离本地快照。 | system-map | workbench |
| step-gateway | Web 入口 | 托管 SPA 静态资源，仅对 `/tasks/:id/text-events`、`/agent/runs/:id/events` 和实时 WebSocket 关闭代理缓冲。 | system-map | web-gateway |
| step-backend | 业务与任务服务 | 执行 `CreateTask` 准入、模型路由冻结、积分预留、Worker 租约调度、短剧编排与资源签名分发。 | system-map | business-api |
| step-pi | Agent 运行时 | 运行会话循环与上下文压缩，不直接访问数据库或外部模型，所有调用经回调桥回到 Go。 | system-map | pi-runtime |

## 一次生成任务如何从准入走到画布落盘？ {#task-lifecycle}

@sequence id=task-seq title="生成任务准入、租约执行与前端物化"
| actor ui | 创作前端 | frontend | source:materializer |
| actor api | 任务 API | backend | source:main-entry |
| actor sql | 任务数据库 | database | source:task-claim |
| actor worker | 任务 Worker | backend |
| actor provider | 模型上游 | external |
| phase | 1. 准入与持久化 |
| ui | api | POST /api/tasks 提交意图 | emphasis |
| api | sql | 冻结模型路由、预留积分并写 Task | emphasis |
| api | ui | 返回 queued 任务记录 | return |
| phase | 2. 租约领取与执行 |
| worker | sql | ClaimNextTask 领 45 秒租约 | emphasis |
| worker | provider | 协议适配调用或异步回查 | default |
| provider | worker | 返回生成内容或媒体地址 | return |
| worker | sql | taskLeaseWriter 校验 owner 写终态 | emphasis |
| phase | 3. 观察与幂等物化 |
| ui | api | 轮询或订阅 /text-events SSE | default |
| api | ui | 返回 READY 结果与资源 ID | return |

@code id=claim-code source=task-claim language=go

## Pi Agent 与批量生成如何受控执行？ {#agent-governance}

@sequence id=agent-seq title="Pi Agent 回调桥、审批暂停与续跑"
| actor client | Agent 面板 | frontend |
| actor go | Go 协调器与桥接 | backend | source:pi-coordinator |
| actor node | Node Pi 进程 | backend |
| actor llm | 文本模型渠道 | external |
| client | go | POST /api/agent/runs 提交消息 | emphasis |
| go | node | 启动会话并注入冻结快照 | emphasis |
| node | go | POST /model 请求推理 | default |
| go | llm | 平台任务与渠道协议调用 | default |
| llm | go | 返回文本流与工具调用 | return |
| go | node | 转发模型事件 | return |
| node | go | POST /tool 校验 29 项工具策略 | security |
| go | client | 写 waiting_approval 并推送事件 | dashed |
| go | node | 返回 pause 立即结束本轮进程 | return |
| client | go | POST /decision 提交 approve | security |
| go | node | 执行收费任务或批次后恢复运行 | emphasis |

| 对比维度 | 单条媒体工具 `generate_media` | 批量媒体工具 `generate_media_batch` |
| --- | --- | --- |
| 单次审批覆盖范围 | 单个画布节点 | 最多 40 个占位节点 {good:一次审批} |
| 试跑（Canary）门禁 | 无独立试跑阶段 | 首条完成后进 `canary_done` 待确认 {good:防批量跑偏} |
| 准入与扣费检查 | `CreateTask` 单次校验 | 逐条走 `CreateTask` 校验配额与报价 {good:不放宽安全边界} |
| 运行时进程占用 | 后台 waiter 等完成后恢复 Pi | 返回占位文本释放 Pi，批次结束后替换真实回执 {good:不阻塞进程} |

## 短剧流水线与电商工作区如何持续编排？ {#domain-pipelines}

@workflow id=drama-flow title="短剧服务端调度与后台尾帧抽取"
| lane ingest | 1. 设定与拆集 |
| lane script | 2. 剧本与切段 |
| lane media | 3. 分池出片与合成 |
| novel | 章节原文导入 | frontend | ingest |
| bible | 作品圣经与设定 | backend | ingest |
| plan | 分批规划分集 | backend | ingest |
| shots | 分镜与台词核对 | backend | script |
| segment | 按模型上限切段 | backend | script |
| prompt | 段视频提示词 | backend | script |
| gate | 样片与媒体放行? | security | media |
| video | 段视频生成 | backend | media |
| tail | 后台尾帧抽取 | backend | media |
| render | 时间线成片合成 | backend | media |
| novel > bible | 提取设定 |
| bible > plan | 游标推进 |
| plan > shots | 逐集改编 |
| shots > segment | 4至15秒目标 |
| segment > prompt | 挂载写作技能 |
| prompt > gate | 审阅关口 |
| gate > video | 批准 | emphasis |
| video > tail | 异步抽帧 | dashed |
| tail > video | 下一段首帧 |
| video > render | 全段就绪 | emphasis |

@steps id=drama-steps
| step-segment | 按模型上限切段 | `SegmentShotsForVideo` 结合模型时长能力与用户设定的 `target_segment_seconds`（4 至 15 秒）将多个镜头合并为 `DramaSegment`。 | drama-flow | segment |
| step-gate | 样片与媒体放行? | 手动媒体模式与第 1 集样片关口阻止后续集提前消耗视频积分；文字剧本与段提示词仍可先行生成。 | drama-flow | gate |
| step-tail | 后台尾帧抽取 | `drama_tail_worker.go` 在调度主循环外按视频资源去重抽帧（单进程并发 4、租期 8 分钟），避免下载与 ffmpeg 阻塞其他作品派发。 | drama-flow | tail |

## 数据所有权与架构核查清单 {#ownership-checklist}

| 核心域 | 权威真相源 | 派生投影或缓存 | 关键一致性规则 |
| --- | --- | --- | --- |
| 画布编辑 | 服务端已确认 revision + 本地 mutation journal | `localforage` 快照、WebSocket 光标投影 | 实时光标直接操作 DOM，不代替版本确认；Agent patch 校验连续 `baseRevision` |
| 生成任务 | SQL `Task` 表 + `lease_owner` | 前端 React Query、SSE 增量缓存 | 旧 owner 写进度或终态被 `taskLeaseWriter` 拒绝；回存失败走 `MediaRecoveryJSON` 重试保存 |
| 云端 Agent | `CloudAgentRun` + `SessionJSONL` + `Checkpoint` | 聊天面板消息流、待发队列 outbox | 远程 `yingce-agent` 失败绝不回退本地 embedded；画布修改生成可验证 `undo` 凭据 |
| 短剧与方舟素材 | `DramaProduction` 族表 / `ArkPortraitGroup` | 精修画布节点、素材库 `ark` 分类 | 画布精修视频须显式采用才写回短剧并重抽尾帧；`asset://` 引用强校验归属与有效期 |

- [ ] 检查新增业务 API 是否统一走 `web/src/services/api/request.ts` 的 `http` 解包，不手写第二套信封解析。
- [ ] 检查任务与短剧写入是否携带租约或 `revision` 条件更新，防止超时恢复后的旧协程覆盖新状态。
- [ ] 检查 Agent 新增工具是否已在 `backend/internal/agent/tool/registry.go` 与 `cloud_agent_tool_bindings.go` 同步登记。
- [ ] 检查短剧挂载的写作技能是否为单轮非交互规范（16000 字符上限），避免对话式前置锁导致返回空角色或空分镜数组。
