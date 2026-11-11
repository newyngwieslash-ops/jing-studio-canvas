# Archify 架构图、一页式解说页与交付记录

正文从 [架构与技术原理](../content/docs/architecture/index.mdx) 开始，包含架构总览、前端架构、后端与任务、Agent 与部署、短剧制作五篇专题文档。本目录保存 Archify 3.2.0 的可编辑规格、经过验证的独立 HTML（含一页式解说页 Explainer 与三张原生架构/时序图），以及可移植的机器验证回执。

## 打开图表与解说页

| 产物 | 类型 | 可编辑规格 | HTML |
| --- | --- | --- | --- |
| 架构与技术原理解说页 | explainer | [explainer.archify.md](diagrams/explainer.archify.md) | [explainer.html](diagrams/explainer.html) |
| 系统架构 | architecture | [system.architecture.json](diagrams/system.architecture.json) | [system.html](diagrams/system.html) |
| 生成任务与结果回读 | sequence | [generation.sequence.json](diagrams/generation.sequence.json) | [generation.html](diagrams/generation.html) |
| Agent 推理与审批续跑 | sequence | [agent.sequence.json](diagrams/agent.sequence.json) | [agent.html](diagrams/agent.html) |

下载或在本地浏览器打开 HTML 即可阅读，不需要产品账号、网络模型调用或开发服务器。GitHub 文件页通常只展示 HTML 源码，需下载后打开。原生图表与解说页均内联 SVG、样式与阅读器；支持明暗主题切换、缩放、步骤联动、源码行范围悬停查看和导出。

- **解说页（`explainer.html`）**：结论先行，将核心运行拓扑、生成任务时序、数据库租约源码摘录、Pi Agent 回调与单条/批量生成对比、短剧分池与后台尾帧工作流、数据所有权表及架构核查清单整合在一页中。
- **原生图表（`system.html`、`generation.html`、`agent.html`）**：分别聚焦系统运行边界、任务准入与租约执行、Pi Agent 三条回调桥与审批续跑。Redis 协调、对象存储、方舟素材库、电商知识库、Host Updater 及全部业务域细节在五篇正文中展开。

## 已完成的检查

完整可移植回执：[verification.json](verification.json)。原始回执中的机器绝对路径已统一替换为仓库相对路径，规格与产物的字节数及 SHA-256 保持不变。

| 产物 | `validate` | `deliver` | `check` | `browser-check` | 诊断/警告 |
| --- | --- | --- | --- | --- | --- |
| `explainer.html` | pass | pass | pass | not-run（Explainer 默认不启动浏览器） | 0 错误、0 警告 |
| `system.html` | pass（Showcase） | pass | pass（含 provenance） | pass（1440×900 至 2048×1320） | 0 错误、0 警告 |
| `generation.html` | pass（Showcase） | pass | pass（含 provenance） | pass（1440×900 至 2048×1320） | 0 错误、0 警告 |
| `agent.html` | pass（Showcase） | pass | pass（含 provenance） | pass（1440×900 至 2048×1320） | 0 错误、0 警告 |

本地 `.delivery.json`、`.finalize*.json`、`.browser-check.json` 与 `.visual-check.*` 被 `.gitignore` 忽略，避免将本机绝对路径提交入库。

## 字节与哈希

| 文件 | 字节 | SHA-256 |
| --- | ---: | --- |
| `explainer.archify.md` | 9361 | `ba41f2da41967bfa56e72bc262d3d8c4e87717a741442984def9c9fa164bfff0` |
| `explainer.html` | 2053454 | `3a2fdfbe702acd6076eb19e1809f46ed419ac547e6507d760ea6a0247998a0df` |
| `system.architecture.json` | 3336 | `649ea75d6eae5fdea072dc95a6ccb7498c67afc228a65d05b94f1bd5faa4d6da` |
| `system.html` | 840076 | `46e937370b0f44e518adddeb483fee87cde5e774287bda90a241d54e85646d2f` |
| `generation.sequence.json` | 3134 | `7e6655e0299912ee052232ac32115e7ed9013e577f18499a7abb45328e2fefcd` |
| `generation.html` | 833957 | `4ae79a05ecf1a056e9d75ab36cb9e6dccf30a66897f48df1dce977f83b90589a` |
| `agent.sequence.json` | 3174 | `75d47bfd25fe1caeb1b5dd23f31027f44c84c70e38c498d032696c6472bea860` |
| `agent.html` | 835593 | `ca9328b914b465f4abc67be0abbd9d6c05dee3641cd52d3f1f6c25cbb450afd4` |

`.gitattributes` 对 `docs/architecture/diagrams/*.{json,md,html}` 固定 `eol=lf`，防止 Windows 检出时换行转换破坏字节回执。切勿在 `finalize` 后手工格式化或修补生成的 HTML。

## 重现与更新

前提：本机已安装 Archify 3.2 Skill，Node 可启动子进程；原生图表的 `browser-check` 还需本机可用 Chrome/Chromium。由于同一物理输出目录共享 `.archify-delivery-lock.json` 互斥锁，以下命令在仓库根目录**串行**执行：

```powershell
$archifyBin = Join-Path $env:USERPROFILE '.gemini\config\skills\archify\bin\archify.mjs'

node $archifyBin finalize architecture docs/architecture/diagrams/system.architecture.json docs/architecture/diagrams/system.html --repo-root . --quality showcase --out-dir .local/cache/archify/system --json
node $archifyBin finalize sequence docs/architecture/diagrams/generation.sequence.json docs/architecture/diagrams/generation.html --repo-root . --quality showcase --out-dir .local/cache/archify/generation --json
node $archifyBin finalize sequence docs/architecture/diagrams/agent.sequence.json docs/architecture/diagrams/agent.html --repo-root . --quality showcase --out-dir .local/cache/archify/agent --json
node $archifyBin finalize explainer docs/architecture/diagrams/explainer.archify.md docs/architecture/diagrams/explainer.html --repo-root . --out-dir .local/cache/archify/explainer --json
```

源码引用基准为 `6ab49b692d6ebfd267826d7117f11100f8128af0`（`local-only` 模式校验本地 Git 对象与被引用文件行范围存在性）。浅克隆缺少该提交或 fork 的 `origin` 不同时，按当前检出仓库的实际 `origin` 与 `HEAD` 更新 `meta.repository` 或 `@meta` 后重新运行 `finalize`。
