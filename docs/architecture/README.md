# Archify 架构图与交付记录

正文从 [架构与技术原理](../content/docs/architecture/index.mdx) 开始，包含前端、后端与任务、Agent 与部署、短剧制作五篇文档。本目录保存 Archify 2.17 的可编辑规格、经过验证的独立 HTML，以及可移植的验证回执。

## 打开图表

| 图表 | 类型 | 可编辑规格 | HTML |
| --- | --- | --- | --- |
| 系统架构 | architecture | [system.architecture.json](diagrams/system.architecture.json) | [system.html](diagrams/system.html) |
| 生成任务与结果回读 | sequence | [generation.sequence.json](diagrams/generation.sequence.json) | [generation.html](diagrams/generation.html) |
| Agent 推理与审批续跑 | sequence | [agent.sequence.json](diagrams/agent.sequence.json) | [agent.html](diagrams/agent.html) |

下载或在本地浏览器打开 HTML 即可阅读，不需要产品账号、网络模型调用或开发服务器。GitHub 文件页通常只展示 HTML 源码，需下载后打开。HTML 内联 SVG 和阅读器；默认中文、经典样式、静态模式，可切换明暗和缩放。工具栏包含搜索、聚焦和导出能力，本次未逐项执行导出格式验收。

系统图概括主调用路径；Redis、对象存储、后台更新及全部业务域在正文展开。生成图中订阅和查询可与 Worker 执行并行。Agent 图选择“需要审批且用户批准”的路径，拒绝分支终结运行；媒体任务等待完成后再恢复 Pi。

## 已完成的检查

完整机器回执：[verification.json](verification.json)。其中 `delivery`、`automatedBrowser` 和 `perceptualReview` 分别保存三种独立证据。原始回执中的机器绝对路径已替换为仓库相对路径，浏览器可执行文件只保留文件名；测量值及规格/产物 SHA-256 不变。

| 图表 | 确定性校验 | 自动浏览器 | 截图视觉审查 | 布局修正轮次 |
| --- | --- | --- | --- | --- |
| 系统架构 | Showcase 9/9，0 错误、0 警告 | passed | passed | 1 |
| 生成任务 | Showcase 9/9，0 错误、0 警告 | passed | passed | 2 |
| Agent 审批 | Showcase 9/9，0 错误、0 警告 | passed | passed | 2 |

布局修正解决了关系标签压住节点、时序图上下留白过多导致首屏溢出的问题，没有缩小字体、隐藏溢出或修改阅读器。Agent 还经过一次独立的语义复核，修正“审批期间一直阻塞工具请求”的错误表述，并重新交付和采集证据。

自动浏览器使用 Chrome 检查 `READ / Still` 状态：

- 1440×900、1600×1000、1920×1080、2048×1320 的浅色页面均无横向或纵向溢出。
- 每张图采集 1440×900 和 2048×1320 的明暗截图，共 12 张；图片审查确认文本、关系、图例和说明卡片无裁切，最大尺寸下纵向布局完整。
- 自动回执的 `visualReview: pending` 是工具固定输出，保留原值；另一个 `perceptualReview.status: passed` 来自图像审查，不把自动测量冒充主观视觉验收。
- 本次检查没有覆盖所有交互菜单、键盘操作和 PNG/JPEG/WebP/SVG/WebM 导出流程，也不代表产品业务测试通过。

原始 `*.visual-check.json`、截图 PNG 和 contact sheet 留在本地同目录且被 Git 忽略；可重复运行下列命令重新生成。版本化交付保留三张 HTML、三份 JSON 及可移植回执，避免把机器路径和重复截图写入仓库。

## 字节与哈希

| 文件 | 字节 | SHA-256 |
| --- | ---: | --- |
| system.architecture.json | 3069 | `26544ac71ace5d65f56327c7ebc22ba10c13bddecceb58ab73ef227a000bf6ae` |
| system.html | 806005 | `2f3e8a632b7dfdbb539d646dbcabb015e95f7949793d6444de6006748e4ba701` |
| generation.sequence.json | 2427 | `f259faf6ce322c52d52dd365853a5e72e155f88e22ae85fd09ae45db9ffc9cd6` |
| generation.html | 805119 | `9a161ee19af569cf4d76b59e7a7329907e1b4ebd71d2582e6f876b661dc87a2f` |
| agent.sequence.json | 2601 | `99a89a5cd20f012c819df09f2f5eea7cb8b695cbb168a42e347840d36ebcd31c` |
| agent.html | 806648 | `53673ca18a81da12b546174a01d1dc6eaa2f954c35f0870386ffc7e36540fc56` |

`.gitattributes` 对图表 JSON/HTML 固定 LF，防止 Windows 检出时换行转换破坏字节回执。不得在 deliver 后手工格式化或修补 HTML。

## 重现与更新

前提：已安装 Archify Skill，Node 可启动子进程；浏览器检查还需要 Chrome/Chromium。以下 PowerShell 命令在仓库根目录执行。`$archifyRoot` 指向本机技能目录，不需要向项目安装新依赖。

```powershell
$archifyRoot = Join-Path $env:USERPROFILE '.codex/skills/archify'

node "$archifyRoot/bin/archify.mjs" validate architecture docs/architecture/diagrams/system.architecture.json --repo-root . --quality showcase --json
node "$archifyRoot/bin/archify.mjs" deliver architecture docs/architecture/diagrams/system.architecture.json docs/architecture/diagrams/system.html --repo-root . --quality showcase --json
node "$archifyRoot/bin/archify.mjs" visual-check docs/architecture/diagrams/system.html --json

node "$archifyRoot/bin/archify.mjs" validate sequence docs/architecture/diagrams/generation.sequence.json --quality showcase --json
node "$archifyRoot/bin/archify.mjs" deliver sequence docs/architecture/diagrams/generation.sequence.json docs/architecture/diagrams/generation.html --quality showcase --json
node "$archifyRoot/bin/archify.mjs" visual-check docs/architecture/diagrams/generation.html --json

node "$archifyRoot/bin/archify.mjs" validate sequence docs/architecture/diagrams/agent.sequence.json --quality showcase --json
node "$archifyRoot/bin/archify.mjs" deliver sequence docs/architecture/diagrams/agent.sequence.json docs/architecture/diagrams/agent.html --quality showcase --json
node "$archifyRoot/bin/archify.mjs" visual-check docs/architecture/diagrams/agent.html --json
```

逐条检查退出码。validate 失败先修规格；deliver 失败不得检查同路径的旧 HTML 并声称新图通过。只有 deliver 成功后才运行 visual-check，随后打开新截图审查；更新 `verification.json` 和本表，保留自动浏览器与视觉审查的不同结论。

系统图来源基准为 `21adab22d65c6cf572f9fd6a910b459c82051c53`，`local-only` 来源模式仍要求本地 Git origin 匹配和该提交对象存在。浅克隆缺少基准提交或 fork 的 origin 不同会无法验证；应根据实际检出的源码重新选择来源基准，不删除来源合同来绕过校验。来源验证证明 7 个文件可定位，不是完整业务语义的自动证明。

## 验证边界

五篇专题及本交付说明共 6 个文档中的 177 个相对链接已检查存在；新 MDX 的 frontmatter、JSON 解析、三组规格/产物字节与 SHA-256、浏览器回执绑定，以及本次修改的差异空白检查通过。链接检查证明本次工作树中的目标存在，不承诺旧文档目录的所有链接或线上页面均可访问。

本次不修改业务实现、不启动开发服务器、不调用真实模型、不运行数据库/容器/部署。没有执行产品全量 lint、类型检查、测试或构建。当前检出缺少 `docs/package.json` 和文档站配置，因此没有声称 MDX 网站构建通过；文档通过仓库阅读入口交付。

正文包含源码相对链接、故障边界和测试入口。现有约定与实现的差异已单列，包括任务数据库队列、资源删除 Outbox、普通出站的遗留开关、错误响应调用点和编辑器插件形态；这些差异没有在文档任务中被顺带修复。
