# 短剧制作：右侧直接选择单段目标时长（15 秒 / 30 秒）

## 目标

在 `/projects/new` 右侧栏加一个「单段时长」快捷选择：智能 / 15 秒 / 30 秒。它只是写入现有 `settings.storyboard` 的另一个入口，后端、提示词、切段算法都不改。

## 设计要点（依据前几轮排查）

- 时长只有一个来源：后端 `StoryboardSettings.SegmentLimits`（`backend/internal/production/storyboard.go#L191-L213`）。剧本提示词、切段、时长校验、预估都读它，所以**不新建链路**。
- 选项与 storyboard 字段的映射（纯前端）：

| 选项 | 写入 `storyboard` | 说明 |
| --- | --- | --- |
| 智能 | `segmentMode: "auto"` | 跟随模型上限（MiniMax-H3 为 30 秒），即现状 |
| 15 秒 | `segmentMode: "range"`、`segmentMaxSeconds: 15`、`segmentMinSeconds: min(当前最短, 15)` | 用 `range` 不用 `fixed`：`range` 保留按场景切段，`fixed` 只按时长装箱（`timing.go#L149`），会在场景中间切开 |
| 30 秒 | 同上，最长设为 30 | — |
| 自定义（只读） | 不写 | 中间栏「分镜规则」里配成别的值时显示「自定义 · 见分镜规则」 |

- 最短秒数必须先夹到不超过 N：`normalizeStoryboard` 遇到 min > max 会把两者对调（`storyboard.ts#L76-L77`），不夹的话会得到 15–20 秒这种反向结果。
- N 大于当前模型 `maxVideoSeconds` 时，该选项置灰，提示「当前模型单次最长 X 秒」。没选模型时不限制。后端本来也会按模型上限截断，这里只是不让用户误选。

## 任务

- [x] 1. `web/src/lib/drama/storyboard.ts`：新增 `SEGMENT_TARGETS = [15, 30]`、`segmentTargetOf(settings): "auto" | number | "custom"`、`withSegmentTarget(settings, target)`，按上表映射。→ 验证：任务 2 的测试通过
- [x] 2. `web/src/lib/drama/storyboard.test.ts`：补用例：最短 20 时选 15 秒得到 15–15（不对调）；选 30 秒后切回智能，数值保留；`segmentTargetOf` 能识别 auto / 15 / 30 / custom。→ 验证：`cd web && bun test src/lib/drama/storyboard.test.ts`
- [x] 3. `web/src/pages/projects/storyboard-rules.tsx`：新增导出组件 `SegmentTargetField({ value, onChange, maxVideoSeconds })`，沿用 `VideoResolutionField` 的 `drama-tabs drama-res-tabs` + `role="radio"` + `aria-checked` 写法，不加新的 CSS 选择器。→ 验证：键盘 Tab 能聚焦，置灰项不可点
- [x] 4. `web/src/pages/projects/create.tsx`：在「单集时长 / 对白语言」那一行下面插入「单段时长」，绑定已有的 `storyboard` / `setStoryboard` 和 `videoOption?.maxVideoSeconds`。→ 验证：切到 15 秒后，中间栏「分镜规则」同步显示「时长范围 8 至 15 秒」，反过来改分镜规则也会同步
- [x] 5. 预估联动：不用改代码，`estimateQuery` 的 key 已包含 `JSON.stringify(storyboard)`（`create.tsx#L175`）。→ 验证：切换 15 / 30 秒后，底部预计积分会重新请求
- [x] 6. 文档：`docs/content/docs/progress/pending-test.mdx` 记一条待测；`docs/content/docs/overview/features.mdx` 的短剧设置补一句。→ 验证：`cd docs && bun run types:check`
- [x] 7. 格式化本次改动的文件：`cd web && ./node_modules/.bin/prettier --write <上述 4 个文件>`；再跑 `bun run typecheck`、`bun run lint`。→ 验证：全部通过
- [ ] 8. 端到端验证：用 MiniMax-H3 新建一部短剧并选 15 秒，跑完分镜拆分。→ 验证：剧本页每个剧情段 ≤ 15 秒；段视频请求里的 `videoSeconds` ≤ 15（`SnapVideoSeconds` 只向上取整到模型档位）

## 完成标准

- [ ] 右侧选 15 秒后，新作品的剧本提示词写的是「每个剧情段 8–15 秒」，切出的剧情段都不超过 15 秒
- [ ] 选「智能」时行为和现在完全一致
- [ ] 推送前 `bun scripts/verify-before-push.mjs` 通过

## 不在本方案（说明原因）

- **后端不改**：`storyboard` 已经随 `settings` 保存和规范化，30 秒超出模型上限时 `SegmentLimits` 会自动截断。
- **项目内设置页（`studio/panels.tsx#L741`）不加快捷入口**：那里已经直接展示完整的「分镜规则」。需要的话，同一个组件一行就能接上。
- **MiniMax-H3 选 2K 只能 15 秒**：能力配置的时长没有跟分辨率联动，选 2K + 30 秒仍会发出超过 15 秒的请求。建议后台单独配一条 2K 线路，时长上限设为 15。要在代码层解决，需要另起方案改能力模型。
- **已改编的集不会自动重切**：要在剧本页点「重新改编」。这和现有分镜规则的行为一致。
- **单个超长台词镜头**仍可能独占一段并超过 15 秒（按模型档位取整）。快节奏下单句最多 16 字、约 4 秒，很少出现。
