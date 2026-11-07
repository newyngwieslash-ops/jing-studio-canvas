---
title: 短剧制作写作技能兼容性排查与改进方案
description: 对话式总控技能（如 short-drama-director 多技能套装）在短剧制作非交互流水线中卡住的根因分析、子文件选用指南与代码层兼容方案
---

# 短剧制作写作技能兼容性排查与改进方案

本文记录【短剧制作】模块在挂载第三方多技能套装（以 `short-drama-director`「漫剧老李 AIGC 全流程 Skill · V7.4.26 多技能套装版」为例）时无法推进制作流程的根因排查、现有界面下的正确选用方式，以及代码层面的兼容改进方案。

---

## 1. 现象与现场还原

在【短剧制作】模块的三个文字步骤（`资产提取`、`分镜拆分`、`视频提示词`）中：
- 选用内置技能 `影策短剧制作工作流` 或 `jingstudio` 时，项目可正常完成「作品圣经（资产提取）→ 参考图生成 → 分镜拆分 → 视频提示词 → 段视频生成」全流程；
- 选用 `short-drama-director`（默认关联根目录 `SKILL.md`）时，项目始终卡在第一步「资产提取（作品圣经）」，无法进入后续阶段。

### 数据库任务记录还原

核对任务表（`tasks`）中 `source = "drama_production"` 的执行记录可见：当 `BiblePrompt` 挂载 `short-drama-director/SKILL.md` 时，上游文本模型 HTTP 调用本身返回 `succeeded`，但模型输出的 JSON 内容**拒绝对小说做角色/道具/场景提取，而是在 `logline`、`tone`、`visualRules` 字段中向用户发起「六道前置锁」追问，并将 `characters`、`props`、`scenes` 全部置为空数组 `[]`**：

```json
{
  "logline": "开工前请确认赛道：①国内剧 ②海外剧 ③解说叙事类 ④其他；素材来源已确定为小说改编。",
  "tone": "请确认视频模型：①Seedance 2.5 ②Seedance 2.0 ③MiniMax H3；以及交付形态：①离线看板 ②Markdown文档 ③两者都要。看板由Markdown编译生成。",
  "visualRules": "已确定古代宫廷实拍方向。请确认画风卡：①沿用所给真人实拍、电影级布光风格 ②在此基础上调整（请说明）③按推荐走。另请提供目标成片时长，以判断是否需要选择段缝衔接方式。",
  "characters": [],
  "props": [],
  "scenes": []
}
```

随后后端在 [`applyBible`](../../backend/internal/app/drama_orchestrator.go) 调用 [`ParseBible`](../../backend/internal/production/parse.go) 解析该 JSON 时，触发 `len(bible.Characters) == 0` 校验：

```go
if len(bible.Characters) == 0 {
    return bible, errors.New("作品圣经没有识别到角色")
}
```

作业因此以 `作品圣经解析失败：作品圣经没有识别到角色` 失败退出，整个短剧制作流水线停留在起点。

---

## 2. 根本原因分析（三重机制冲突）

| 维度 | `short-drama-director`（根 `SKILL.md`） | `影策短剧制作工作流` / `jingstudio` | 对【短剧制作】流水线的影响 |
| --- | --- | --- | --- |
| **交互假设** | 面向多轮对话 Agent，设「六道前置锁 + 画风锁」硬门，规定任一项未由用户勾选则**严禁开工、必须停下反问** | 允许沿用既有参数与原文直接产出草案，不强制阻塞等待用户问卷回复 | 【短剧制作】是服务端后台单轮批处理，无对话交互窗口；模型因遵守「未确认锁不得开工」而返回空角色数组 |
| **文件体量与前 16,000 字分布** | 根 `SKILL.md` 共 95,064 字；**前 16,000 字 100% 为「〇、任务前置强制锁定」拦截规则**，不含任何创作提取方法 | 全文分别为 4,144 字、9,491 字，均在 16,000 字上限内完整送达 | 后端 `MaxWritingSkillRunes = 16000` 截断后，模型收到的 `<skill>` 切片只有「不准开工、必须盘问锁」的禁令 |
| **包结构与文件职责** | 「1 总控 + 12 子技能」共 168 个文件，根 `SKILL.md` 只是路由器与门禁，真正方法在子目录 | 单核心文件或主说明直接承载创作规范 | 前端 [`PromptSkillModal`](../../web/src/pages/projects/prompt-skill.tsx) 选中技能时默认定位到根 `SKILL.md`，导致选到了总控门禁而非子技能 |

### 2.1 执行范式冲突：对话式「前置锁硬门」vs 非交互单轮 JSON 流水线

- **【短剧制作】是后台单轮结构化流水线**：[`BiblePrompt`](../../backend/internal/production/prompts.go) 和 [`ScriptPrompt`](../../backend/internal/production/prompts.go) 每次调用只发送一轮提示词（含 `作品名、画幅、视觉风格、对白语言、原文`），并要求模型直接输出固定结构的 JSON。
- **技能硬门禁覆盖了提取指令**：`short-drama-director/SKILL.md` 反复声明：
  - `动手前六道前置锁（① 赛道分流 ② 素材来源 ③ 模型版本 ④ 画幅 ⑤ 交付形态 ⑥ 段缝衔接）＋ 画风锁：任何一道未指定时，必须停下询问用户并把选项摆出来让他勾，禁止默认跳过、禁止替用户推断——这是硬门，不是建议`
  - `未指定时，唯一正确动作是停下询问用户——禁止以任何理由默认「国内剧」；赛道未确认，不进入 sd-script-engine 第 0 步模式选择、不生成任何 IP / 分集 / 正文`
- 由于系统生成的 `BiblePrompt` 不包含该技能私有的口令（如「赛道：①国内剧」「C 锁交付形态」「I 锁画风卡勾选」），模型为了同时遵守「只输出 JSON」和「未确认锁严禁生成正文」，只能将追问塞进文本字段并输出 `"characters": []`。

### 2.2 16,000 字截断使模型只看到「锁门禁」

后端 [`segment_writer.go`](../../backend/internal/production/segment_writer.go) 定义了写作技能注入上限：

```go
const MaxWritingSkillRunes = 16000
```

[`structuredSkillBlock`](../../backend/internal/production/prompts.go) 会将所选技能文件按 `MaxWritingSkillRunes` 截断。而 `short-drama-director` 的根 `SKILL.md` 长达 **95,064 字**，其前 16,000 字覆盖的章节列表为：
- `# 漫剧老李 AIGC 全流程 Skill · V7.4.26 多技能套装版`
- `## ★ 使用入口`
- `## 〇、 任务前置强制锁定：〇·0·赛道 / 〇·1·素材来源 / A·模型版本 / B·画幅 / C·交付形态 / D·段缝衔接方式`（含 `〇·0`、`〇·1`、`A`、`B`、`C`、`D` 各子节）

这意味着模型收到的 16,000 字 `<skill>` 块中，**完全没有出现任何关于资产提取、分镜拆分或提示词撰写的业务方法**，100% 都是前置锁拦截指令。

### 2.3 多文件套装的子技能与参考图语法差异

`short-drama-director` 包含 168 个文件，真正负责具体创作步骤的内容位于子目录中：
- **资产提取**：`sd-asset-engine/references/three-list-extraction.md`（12,247 字）、`sd-asset-engine/SKILL.md`（12,701 字）
- **分镜拆分**：`sd-storyboard-engine/SKILL.md`（16,093 字）
- **视频提示词投喂**：`sd-feed-engine/SKILL.md`（8,964 字）、`references/model-adapters.md`

此外，在「视频提示词」阶段，`short-drama-director` 体系要求首行输出 `> 素材形态：M1...`、文末附《素材接线单》，且资产编号固定写为 `图1 / @图1`；而【短剧制作】模块的 [`SegmentWriterPrompt`](../../backend/internal/production/segment_writer.go) 与 [`UnknownReferenceTokens`](../../backend/internal/production/segment_writer.go) 要求只输出提示词正文、参考图编号固定写为 `参考图1`（与系统在提交视频时自动拼接的参考图清单逐字对应）。

---

## 3. 现有版本下的正确配置方式（不改代码）

在当前界面中使用 `short-drama-director` 等多文件技能套装时，**不要直接使用默认选中的根目录 `SKILL.md`**。请在【短剧制作】的「制作步骤 → 文字步骤」点击「选择技能」，在弹窗中间的**「文件」列表**中按步骤选用对应的子技能文件：

1. **资产提取（作品圣经）**：
   - 推荐选择：`sd-asset-engine/references/three-list-extraction.md`（12,247 字）
   - 原因：专门定义「角色外观描述 / 独立道具设计清单 / 独立场景设计清单」三清单提取原则与人·物·场分离标准，不含对话式前置锁门禁，且完整处于 16,000 字上限以内。
2. **分镜拆分（每集剧本）**：
   - 推荐选择：`sd-storyboard-engine/SKILL.md`（16,093 字）或 `references/cinematic-dramaturgy-rules.md`（8,295 字）
   - 原因：直接提供短剧机位体系、镜头经济性与台词承接规则，不含立项六道锁拦截。
3. **视频提示词（每个剧情段）**：
   - 推荐选择：保持**「内置规则」**或使用 **`影策短剧制作工作流/SKILL.md`**；若需引用该套装的表演/运镜细则，可选择 `references/acting-direction.md` 或 `references/blocking-direction.md`。
   - 原因：避免 `sd-feed-engine` 中的 `图N` 编号规则、首行形态声明及文末《素材接线单》干扰系统对 `参考图N` 的自动绑定与字数预算控制。

---

## 4. 代码层兼容改进建议

为避免用户将面向对话式 Agent 的技能（含前置锁、问卷反问、多阶段报账门禁）直接挂载到【短剧制作】后台流水线时再次发生静默产空或卡死，建议从以下三个位置增强系统兼容性：

### 4.1 后端：在技能注入块中显式声明「非交互流水线免锁合同」

修改 [`backend/internal/production/prompts.go`](../../backend/internal/production/prompts.go) 的 `structuredSkillBlock` 与 [`backend/internal/production/segment_writer.go`](../../backend/internal/production/segment_writer.go) 的 `SegmentWriterPrompt`：

- **现状**：只说明了 `技能与 JSON 结构冲突时以 JSON 结构为准，技能里要求的其他输出格式不要输出`，未覆盖技能里的「前置锁未确认时禁止生成正文、停下询问用户」规则。
- **改进**：在 `<skill>` 外层包裹说明中明确加入非交互执行约束：
  1. 当前运行在**服务端非交互式批处理流水线**中，用户无法在当前步骤回答任何追问或勾选选项；
  2. 技能中要求的所有「前置锁 / 立项锁 / 赛道分流 / 素材来源 / 模型版本 / 画幅 / 交付形态 / 段缝衔接 / 画风卡确认 / 报账确认 / 阶段门停顿」，均视为**已由外层系统参数锁定并通过**，未指明项一律按原文与系统给定的画幅、风格、时长自动适用；
  3. **严禁停下询问用户，严禁在字段中填写待确认提示词或将 `characters` / `shots` 输出为空数组等待用户回复**；必须直接基于给定原文完成本步骤完整产出；
  4. 在 `SegmentWriterPrompt` 中补充：即使技能规定了其他图片编号写法（如 `图N`、`@ImageN`）、首行素材形态声明或文末《素材接线单》，也必须服从下方【硬性规则】，只输出纯净提示词正文并固定使用素材稿给定的 `参考图N` 编号。

### 4.2 后端：增强 `ParseBible` / `ParseScript` 对「反问型空结果」的错误提示

在 [`backend/internal/production/parse.go`](../../backend/internal/production/parse.go) 中：
- 当 `ParseBible` 发现 `len(bible.Characters) == 0`（或 `ParseScript` 发现 `len(shots) == 0`），且 `logline` / `tone` / `visualRules` / `synopsis` 中包含明显的交互追问特征词（如 `请确认`、`请选择`、`请先确认`、`待确认`）时，返回更具指导性的错误说明（提示“所选技能触发了交互式追问而未提取角色，请改选具体业务子文件或更换非阻塞技能”），便于用户在界面队列中一眼识别原因。

### 4.3 前端：技能文件选择器增加多文件套装与截断显著提示

在 [`web/src/pages/projects/prompt-skill.tsx`](../../web/src/pages/projects/prompt-skill.tsx) 的 `PromptSkillModal` 中：
- 当所选技能包包含多个 `.md` 文件且当前选中的 `SKILL.md` 超过 `16,000` 字时，在预览栏上方给出醒目提示：说明当前文件超出 16,000 字上限将被截断，若该文件为多技能套装的总控目录/前置门禁，建议在中间「文件」列选择对应步骤的子技能文件（如资产提取、分镜拆分子文件）。
