# 角色资产图与远程资源图片切页闪烁/慢加载修复计划 Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 让同一账号下已成功准备且授权仍有效的远程图片在重新挂载时立即获得正确展示地址，避免重复应用占位与准备排队，并补齐有限重试、账号隔离和首次下载瓶颈的验证。

**Architecture:** 在现有 `resources.ts` 内增加有界、仅内存、仅保存字符串和标量的 ready URL 缓存，复用资源访问描述的有效期和会话代次。`CachedResourceImage` 命中后直接输出图片，未命中继续使用现有视口门控和图片准备队列；两种加载失败入口使用同一轮限次恢复。首次冷加载单独测量、按实际存储配置处理 CDN，不以增加位图缓存解决网络瓶颈。

**Tech Stack:** React 19、TypeScript、Bun test、已有 happy-dom/React DOM、Axios adapter 测试替身、Go 资源访问与存储分发（本批默认只读）。不增加依赖。

---

## 0. 执行范围、现状和不可承诺事项

本文件是实施计划，所有任务初始均未完成，不代表代码已经修复。执行参考 @writing-plans、@ponytail、@executing-plans；工具中的技能名称若没有 `superpowers:` 前缀，使用实际安装的对应技能，不为满足名称安装新依赖。

本轮沿用当前工作区保存计划。实施前重新检查工作区；已有未提交业务修改不能靠创建一个只含 HEAD 的 worktree 丢掉，也不能混入本任务提交。需要隔离时通过 Codex `create_worktree` 创建，并明确需要带入的基线；不得自动 stash/reset 用户修改。

### 已核对的代码事实

| 位置 | 当前事实 | 对本计划的影响 |
| --- | --- | --- |
| `web/src/pages/projects/detail.tsx` 的 cast 条件分支 | 切换工作区视图会卸载角色面板 | 必须覆盖真实卸载/重新挂载，不只测试隐藏再显示 |
| `web/src/pages/projects/studio/panels.tsx` 的 `CastPanel` | 角色卡片和详情使用 `MediaThumb` | 不使用旧名称 `DramaCastPanel` / `DramaResourceImage` 编写测试 |
| `web/src/pages/projects/drama-shared.tsx:188` 的 `MediaThumb` | thumbnail 展示，失败后 1500ms、5000ms 重挂载，最多两次 | 新组件重试必须与外层预算一起验收 |
| `web/src/components/cached-resource-image.tsx:22` | 初始 displayed=null；identity 不含 variant；先 prepare 再挂远程 img | 需要首轮同步查询和 variant 身份隔离 |
| 同文件约 83 行 | 备用 src 准备成功后没有更新 candidate | 先修复再记录 ready，避免认证失败地址被当作成功地址 |
| `web/src/services/canvas-image-loader.ts:3` | 4 并发、48 个对象、16MP、60s；TTL 是调用时清理 | 常量和队列策略保持不变；不把 60s 描述成定时即时释放总内存 |
| `web/src/services/api/resources.ts:167` | 已有 refreshAt / expiresAt 缓存规则、持久地址缓存、批量访问 API | 不另建请求客户端或另一套签名 TTL 算法 |
| 同文件约 499 行 | forceRefresh 可复用普通在途请求；异步删除持久缓存后才登记请求 | 需要修复强刷时序和真正跳过持久缓存 |
| `web/src/lib/user-session.ts:23` | 账号切换调用 clearResourceAccessCache 再更新 scope | 复用 accessGeneration；单纯 scope 相等不能挡住 A→B→A |
| `backend/internal/assets/access.go:138` | thumbnail 当前回退 original，标记 thumbnail_not_ready | URL 缓存不会减少首次原图下载字节数 |
| `backend/internal/storage/objects.go:213` | 浏览器源站签名带 response-cache-control | 必须检查实际 GET 响应是否执行该策略，不能只看签名查询参数 |

前一轮实际运行 `bun test test/canvas-media-performance.test.ts`：**29 pass / 2 fail**，第 62、74 行源码正则提取为空。实施时先重现并定位；这不是两项浏览器运行失败，也不能直接删断言放行。

### 成功口径

1. ready 命中且有效时，首轮提交 DOM 有正确 `<img src>`，无应用 loadingFallback，不等待 IntersectionObserver 回调，不调用 prepareCanvasImage；后续 effect 也不能重复排队。
2. 首次、过期和淘汰后的加载继续走受控准备；错误可有限恢复，最终进入稳定失败态。
3. 不扩大 16MP / 60s / 48 对象 / 4 并发配置，不新增应用持有的 Image、ImageBitmap、Blob 或 Object URL 缓存。
4. ready 最多 500 条，按会话、资源、请求 variant 隔离；成功地址与其真实访问描述绑定，不能拿新描述的 TTL 延长旧 URL。
5. 实测真实页面切页体验，分别报告 DOM 就绪、图片 decode 完成、实际可见时间、网络请求与字节量。

**不承诺：** 任意设备和缓存状态下零帧空白、同一会话永久命中、浏览器总内存零增加、500 条 URL 小于 100KB、CDN 下载 3MB 必然毫秒级。`immutable` 不保证浏览器永不回收缓存；ready 记录也不是权限凭证或浏览器缓存驻留证明。

### 范围边界

- 主线改动：`resources.ts`、`cached-resource-image.tsx`、专项测试、必要文档。
- 保留 `MediaThumb` 两次外层重试，不顺带重构工作区面板。每次挂载最多一次自动强刷，外层完整失败周期最多三次挂载、三次自动强刷；一次强刷轮次可能因 thumbnail→original 发出两个不同 variant 的访问请求。
- 不改后端鉴权、资源模型、资源删除、公开/私有边界、用户 API 调用方式或 `RetainedCanvasImage`。不新建缩略图服务，不将图片转 Blob 展示，不提高队列并发。
- 首次网络提速是独立验收项。若没有可用 CDN 配置或线上访问条件，必须标记“待运维验证”，不能把整批宣称为全部完成。

## 1. 文件、依赖和提交策略

路径均相对于仓库根；执行前以符号定位为准，行号只用于导航。

| 文件 | 操作 | 原因 |
| --- | --- | --- |
| `web/src/services/api/resources.ts` | 修改 | ready 缓存、上下文、强刷合并与失效 |
| `web/src/components/cached-resource-image.tsx` | 修改 | 首轮展示、统一恢复、回退修正 |
| `web/test/canvas-media-performance.test.ts` | 修改 | 修复被证实的源码提取基线问题；更新强刷结构断言 |
| `web/test/resource-image-recovery.test.ts` | 新增 | 隔离子进程运行专项行为测试 |
| `web/test/fixtures/resource-image-recovery.harness.tsx` | 新增 | 实际 resources + 实际组件行为测试，替换外部 I/O |
| `docs/content/docs/progress/pending-test.mdx` | 追加本任务段落 | 区分实现、自动验证、用户验收 |
| `docs/content/docs/backend/http-api.mdx` | 最小增补 | 资源访问章节说明前端展示缓存和强刷语义；HTTP 协议不变 |
| `docs/plans/2026-11-05-resource-image-remount-recovery.md` | 更新状态 | 逐项记录真实结果、限制和证据，不存签名 URL |

只读核对：`web/src/services/canvas-image-loader.ts`、`web/src/lib/user-scope.ts`、`web/src/lib/user-session.ts`、`web/src/lib/localforage-storage.ts`、`web/src/pages/projects/drama-shared.tsx`、`web/src/components/canvas/canvas-node-media-content.tsx`、`backend/internal/assets/access.go`、`backend/internal/storage/delivery.go`、`backend/internal/storage/objects.go`、`.env.example`、`docker-compose.dev.yml`、`web/package.json`、`web/tsconfig.json`。

任务依赖：1 → 2 → 3 → 4 → 5 → 6 → 7 → 8。任务 9 的只读测量可与前端实现独立进行，配置修改依赖实际域名/鉴权/环境资料。任务 10 汇总 8、9；无法执行的环境验收必须保持未勾选。

每个编号 Step 是单个操作，通常 2–5 分钟；完整测试和 build 可能更久。Task 内实现检查点逐个完成，不把所有测试写完后一次性替换整个组件。

提交使用 `<type>(<scope>): <业务模块> - <解决的问题>`，可附 `Tested:` / `Not-tested:` / `Constraint:` Lore trailers。只暂存本任务 hunk，已有修改文件用 `git add -p`；禁止 `git add .`。以下提交标题是实施检查点，本计划编写阶段不执行提交。

## Task 1：固定测试基线并明确已有失败原因

**Files:**
- Modify（仅在重现后）: `web/test/canvas-media-performance.test.ts:13`
- Read: `web/test/helpers/module-group-source.ts`
- Read: `web/src/components/canvas/canvas-node-media-content.tsx:590`

**Step 1 — 记录工作区基线。** 在仓库根运行 `git status --short` 和 `git diff --stat`，记录本任务将触碰文件是否已有修改。特别注意 pending-test.mdx 和 http-api.mdx 已有用户修改。无需复制数据库或真实配置。

**Step 2 — 重现现有测试。** 在 `web/` 运行：

```powershell
bun test test/canvas-media-performance.test.ts
```

预期基于前轮结果为 29 pass / 2 fail；若环境已变化，以实际结果为准。确认失败属于源码提取而非实现中不存在相应函数。

**Step 3 — 验证换行假设。** 对比 `moduleGroupSource(...)` 原字符串和 `.replace(/\r\n/g, "\n")` 后的两个正则匹配结果。仅在规范化后匹配恢复时，将测试文件中的读取改为：

```ts
const canvasNodeContentSource = moduleGroupSource("components/canvas/canvas-node-content.tsx").replace(/\r\n/g, "\n");
```

不全仓转换换行、不改生产代码、不删除断言。若仍提取为空，检查当前函数签名/导出边界，修正提取范围，记录真实原因。

**Step 4 — 运行同一测试。** 预期现有 31 项全部通过；未通过不得称基线已恢复。

**Step 5 — 单独提交。** `test(media): 图片展示 - 避免源码换行差异导致回归检查失真`。只包含已经证实必要的测试改动。

## Task 2：建立隔离的实际行为测试入口

**Files:**
- Create: `web/test/resource-image-recovery.test.ts`
- Create: `web/test/fixtures/resource-image-recovery.harness.tsx`
- Reference: `web/test/cloud-agent-panel-sync.test.ts`
- Reference: `web/test/fixtures/cloud-agent-panel-sync.harness.tsx`
- Reference: `web/test/canvas-image-geometry.test.ts:66`

**Step 1 — 新增外层测试入口。** 使用现有子进程模式隔离 mock.module，代码如下：

```ts
import { expect, test } from "bun:test";
import { resolve } from "node:path";

test("资源图片缓存、重挂载与恢复行为", () => {
    const result = Bun.spawnSync(
        [process.execPath, "test", "./test/fixtures/resource-image-recovery.harness.tsx"],
        { cwd: resolve(import.meta.dir, ".."), timeout: 30_000 },
    );
    const output = new TextDecoder().decode(result.stdout) + new TextDecoder().decode(result.stderr);
    expect(result.exitCode, output).toBe(0);
    expect(output).toContain("0 fail");
});
```

**Step 2 — 运行入口确认缺少 fixture 失败。** 在 `web/` 运行 `bun test test/resource-image-recovery.test.ts`，预期非零，原因必须是 fixture 尚不存在。

**Step 3 — 建立 fixture 最小环境。** 复用现有 harness 的 happy-dom 全局安装和动态 import 顺序：先 Window/DOM 全局，再 mock localforage 适配器和图片准备 I/O，最后 import React、React DOM、真实 resources.ts、真实 CachedResourceImage。保留真实 getResourceAccess，通过 apiClient.defaults.adapter 模拟 `/resources/access` 信封，不能 mock 掉被测强刷函数。

持久缓存替身必须按 scope 分桶，支持可控 get/remove/set 延迟与失败；图片准备替身必须记录 URL、signal 和手动 resolve/reject，不自动立即成功。用于竞态的 deferred 使用：

```ts
function deferred<T>() {
    let resolve!: (value: T | PromiseLike<T>) => void;
    let reject!: (reason?: unknown) => void;
    const promise = new Promise<T>((res, rej) => { resolve = res; reject = rej; });
    return { promise, resolve, reject };
}
```

每项测试重置 scope、accessCache、持久 Map、mock 调用和根节点；恢复 Axios adapter、时间替身、Observer 和所有全局属性。测试不访问真实网络/真实账号存储。不要写长 sleep；只对既有 16ms 批量窗口使用短有界等待，其余通过 deferred/act 驱动。

**Step 4 — 添加一个真实 API 基线测试。** 同资源同 variant 连续 getResourceAccess 两次，adapter 请求一次且返回同一 URL；clear 后再请求计数增加。该测试验证 fixture 本身可用，不提前依赖尚未实现的 ready helpers。

**Step 5 — 运行入口确认通过并提交。** 预期子进程 0 fail。提交 `test(media): 资源图片 - 隔离缓存与组件行为测试环境`。

## Task 3：增加有界 ready URL 缓存和会话代次保护

**Files:**
- Modify: `web/src/services/api/resources.ts` 的 accessCache、accessGeneration、clearResourceAccessCache 周边
- Test: `web/test/fixtures/resource-image-recovery.harness.tsx`

**Step 1 — 写真实缓存行为失败测试。** 通过真实 getResourceAccess 获得描述后调用下列拟新增接口；不得直接写模块私有 Map。覆盖：同步读取、未成功记录不命中、500 条上限、提交使用后的 LRU 顺序、过期、clear、不同 scope/variant、旧 context 拒绝写入、错误 URL 不误删新项。

测试采用真实未来时间，示例完整描述：

```ts
function imageAccess(id: string, variant: "original" | "thumbnail" = "thumbnail") {
    const now = Date.now();
    return {
        resourceId: id,
        requestedVariant: variant,
        actualVariant: "original" as const,
        url: `https://images.example.invalid/${id}.png`,
        delivery: "origin" as const,
        issuedAt: new Date(now).toISOString(),
        expiresAt: new Date(now + 3_600_000).toISOString(),
        refreshAt: new Date(now + 2_000_000).toISOString(),
        revision: "test-revision",
    };
}
```

**Step 2 — 运行 RED。** `bun test test/resource-image-recovery.test.ts`，预期 helper 尚未实现导致失败。

**Step 3 — 添加上下文和缓存类型。** 在 resources.ts 内复用现有 generation，而非新增独立全局 epoch：

```ts
export type ResourceAccessContext = Readonly<{ scope: string; generation: number }>;
export type ReadyResourceImage = Readonly<{
    url: string;
    expiresAt: number;
    sourceKey: string;
    sourceVariant: ResourceAccessVariant;
    revision: string;
}>;
const MAX_READY_RESOURCE_IMAGES = 500;
const readyResourceImages = new Map<string, ReadyResourceImage>();

export function captureResourceAccessContext(): ResourceAccessContext {
    return { scope: getActiveUserScope(), generation: accessGeneration };
}

export function isResourceAccessContextCurrent(context: ResourceAccessContext) {
    return context.scope === getActiveUserScope() && context.generation === accessGeneration;
}

function displayAccessKey(scope: string, id: string, variant: ResourceAccessVariant) {
    return `${scope}:${id}:display:${variant}:`;
}
```

仅将 getResourceAccess 的 display 空 downloadName 分支与该 key 保持一致，不重构其他 purpose 的键格式。

**Step 4 — 实现以下明确接口和约束。** 每个 helper 完成后运行相关测试：

```ts
export function peekReadyResourceImage(
    storageKey: string | undefined, variant: ResourceAccessVariant, context: ResourceAccessContext,
): ReadyResourceImage | undefined;

export function rememberReadyResourceImage(
    storageKey: string, variant: ResourceAccessVariant, access: ResourceAccess,
    requestedAccess: ResourceAccess | undefined, context: ResourceAccessContext,
): void;

export function touchReadyResourceImage(
    storageKey: string, variant: ResourceAccessVariant, expectedURL: string, context: ResourceAccessContext,
): void;

export function forgetReadyResourceImage(
    storageKey: string, variant: ResourceAccessVariant, failedURL: string, context: ResourceAccessContext,
): void;
```

上面是新增接口合同，不是可直接粘贴的空实现。具体实现顺序固定为：

Task 3 先实现不依赖请求类型的缓存行为，使本任务可独立编译和通过测试；下面涉及“强刷正在进行”的检查在 Task 4 引入 `{ promise, forceRefresh }` 后补入，并由 Task 4 的竞态测试锁定，不在旧 Promise 类型上读取不存在的字段。

- `peek`：确认 context 当前、storageKey 是 resource、条目未到期、请求键及 sourceKey 均无强刷正在进行，返回只读条目，否则 undefined。渲染阶段不更新时间、不重排 Map、不删除条目。
- `remember`：确认 context 当前；resourceId 匹配；成功 access 对应的 sourceKey 为其 requestedVariant 的 display key；要求 `accessCache.get(sourceKey)?.value === access` 且 entry 尚未过期；不得有相关强刷进行中。还必须验证请求 variant 当前有效描述与本轮 requestedAccess 是同一对象；请求 variant 获取失败时本轮证明为 undefined，此时只允许该键仍无有效描述。保存 resolveResourceAccessURL(access.url)、**原 entry.expiresAt**、sourceKey/sourceVariant/revision；不重新调用 TTL 计算函数。不接受任意 fallback URL。
- 保存前清掉过期项、删除同键旧项；size>=500 时删除最早 Map key；然后 set。记录完成时已在过期边界的地址可以继续由当前组件展示，但不能成为下一次挂载的快路径。
- `touch`：只在组件提交后执行，context、key、expectedURL、有效期均匹配才 delete/set；不延长 expiresAt。
- `forget`：context 当前且现存条目 URL 等于 failedURL 才删除。迟到 error 不得删除新 URL。
- `clearResourceAccessCache`：现有 generation++ 后一并清 ready Map。保留已有请求取消和用户隔离逻辑。

`thumbnail` 请求因失败转为 `original` 后：ready key 仍为调用方的 thumbnail，sourceKey/sourceVariant 记录真正成功的 original，TTL 来自 original 描述。公开 CDN 无 expiresAt 时同样复制 accessCache 的最终 deadline，不自行指定无限期。

requestedAccess 必须在本轮请求 variant 的访问调用成功时捕获，失败则为 undefined，并跨 original 回退保留；不能在准备完成时重新从缓存读取冒充本轮证明。它用于挡住“thumbnail 强刷已经结束，旧 original prepare 才返回”的迟到覆盖，不需要另建无界的逐资源版本 Map。若新的请求 variant 描述已取代旧对象，宁可跳过一次 ready 记录，也不能给旧完成事件套用新描述。

**Step 5 — 运行 GREEN。** 增加 501 个条目并验证第一个淘汰；touch 第一个后再插入，应淘汰第二个。advance 时间至 refreshAt 后 miss；访问和 touch 不续期；A→B→A 后旧 token 不能记录；旧 access 对象不能借新同键描述的 TTL 写入。

**Step 6 — 提交。** `fix(media): 资源图片 - 在有效授权内复用已准备的展示地址`。

## Task 4：让 forceRefresh 真正绕过旧地址并合并并发

**Files:**
- Modify: `web/src/services/api/resources.ts:149` 的 accessRequests 类型及 `getResourceAccess`
- Modify: `web/test/canvas-media-performance.test.ts:121` 的相关结构断言
- Test: `web/test/fixtures/resource-image-recovery.harness.tsx`

**Step 1 — 写失败测试，按事件顺序验证。**

| 场景 | 必须断言 |
| --- | --- |
| 持久缓存有旧 URL，removeItem 失败 | 强刷仍 POST，不能回读旧持久值 |
| 普通请求卡在持久读取，两个强刷紧接着进入 | 强刷登记立即发生；两个强刷共享一个后端刷新；旧读取完成不作为强刷最终结果 |
| 普通请求成功/失败后强刷 | 两种都继续执行一次刷新，不吞 AbortError 的会话失效意义 |
| 强刷等待旧请求时又普通读取 | 新普通调用加入强刷，不得到旧 accessCache 值 |
| 强刷中切账号或 A→B→A | 旧操作不删除新账号缓存、不入旧 batch、不发布 ready |
| sourceVariant=original 的 ready thumbnail | original 强刷使依赖它的 ready 失效 |

**Step 2 — 运行 RED。** `bun test test/resource-image-recovery.test.ts`，确认失败指向当前强刷复用/持久回读时序，不是 fixture 错误。

**Step 3 — 改用有状态的在途记录。** 仅改变 accessRequests；resourceRequests 继续保持原状：

```ts
type ResourceAccessRequest = { promise: Promise<ResourceAccess>; forceRefresh: boolean };
const accessRequests = new Map<string, ResourceAccessRequest>();

function invalidateReadyResourceAccess(key: string) {
    for (const [readyKey, ready] of readyResourceImages) {
        if (readyKey === key || ready.sourceKey === key) readyResourceImages.delete(readyKey);
    }
}
```

同步判断顺序：

```ts
const pending = accessRequests.get(key);
if (pending && (!forceRefresh || pending.forceRefresh)) return pending.promise;
if (!forceRefresh) {
    const cached = accessCache.get(key);
    if (cached && cached.expiresAt > Date.now()) return cached.value;
}
if (forceRefresh) invalidateReadyResourceAccess(key);
```

**Step 4 — 调整异步请求生命周期。** 用 `Promise.resolve().then(async () => { ... })` 创建 entry.promise，再同步 `accessRequests.set(key, entry)`，保证第一次 await 前已经登记。异步分支按以下顺序逐步实现，保留原 ApiError 文案转换：

1. 强刷且 pending 是普通请求：等待 pending.promise settle；普通网络失败可继续，随后立刻 `assertResourceRequestCurrent(generation, scope)`，会话失效必须退出。
2. 强刷：删除当前 accessCache；再次按 key/sourceKey 清 ready；尝试删除持久 display 地址，删除失败不终止网络刷新；await 后再检查 generation/scope。
3. **只有非强刷**才能 `readPersistedDisplayAccess`。await 后检查 generation/scope，再返回持久值或入 batch。
4. 强刷和普通 miss 共用 `enqueueResourceAccess`，入队前检查上下文。等待旧请求 settle 后才复用同一 batch key，避免 pendingResourceAccess.set 覆盖旧 resolver。
5. finally 仅当 `accessRequests.get(key) === entry` 时删除自己。clear 后的旧 finally 不能清掉新 entry。

同一 key 的强刷串行在普通请求之后，避免引入第二套 batch/request-id 协议；现有网络 POST timeout 继续使用 15 秒，**它不涵盖 IndexedDB 读写及等待旧请求的总时间**。补充持久 I/O 长时间未决时的卸载/切账号测试：允许旧 Promise 仍未决，但一旦释放不得入旧 batch 或发布旧结果。整轮存储 I/O 超时不属于本批保证，不得直接超时后复用旧 batch key 导致 resolver 丢失；若真实验收发现此类卡住，再单独设计有界持久读取，不能用网络超时冒充修复。强刷等待阶段 ready 的 peek/remember 必须被强刷标记挡住。描述的后台签名时间窗可能返回**相同 URL**，不得仅因 URL 相同拒绝这次重试。

**Step 5 — 运行 GREEN 和消费者回归。**

```powershell
bun test test/resource-image-recovery.test.ts test/canvas-image-geometry.test.ts test/canvas-media-performance.test.ts
```

现有源码断言改为新 entry 结构需要的最小约束；行为正确性以真实并发测试为准，不能保留“只看到 inFlightRefresh 字符串就证明合并”的结论。

**Step 6 — 提交。** `fix(resources): 资源访问 - 避免强刷复用失效的持久地址`。

## Task 5：首轮展示快路径与成功 URL 准确记录

**Files:**
- Modify: `web/src/components/cached-resource-image.tsx:22`
- Test: `web/test/fixtures/resource-image-recovery.harness.tsx`
- Read: `web/src/components/asset-media-preview.tsx`、`web/src/components/canvas/canvas-node-content.tsx`、`web/src/components/assets/asset-library-picker-modal.tsx`

**Step 1 — 写重挂载失败测试。** 完成首次 getResourceAccess→prepare→显示后卸载。使用同一 JS 会话下真实组件的 `renderToStaticMarkup` 验证重新初始渲染包含正确 src、无 loadingFallback（effects 不运行）；再于 happy-dom 真正重新挂载并用 act 刷新 effects，验证 prepare 和 API 次数没有增加。Observer 替身不发 isIntersecting 回调，ready 命中仍有 img。同步静态渲染只作为初始输出测试，不是实际页面验收；不能用 flushSync 返回后的 DOM 假定 effect 尚未执行。

另写切换 variant、切换资源 props、eager/显式 loading、备用 src 成功的测试。组件测试 mock prepare 只证明调度，不冒充浏览器解码测试。

**Step 2 — 运行 RED。** `bun test test/resource-image-recovery.test.ts`，预期重挂载首轮没有 img 或发生重复 prepare。

**Step 3 — 修复成功地址传播。** 当前备用分支必须变为：

```ts
if (candidate !== src && src) {
    await prepareCanvasImage(src, controller.signal);
    candidate = src;
} else {
    throw error;
}
```

异步加载同时保留请求 variant 的 requestedAccess 和最终成功使用的 ResourceAccess，两者在 original 回退时可能不同。只有 candidate 等于最终描述解析后的 URL，且本轮准备成功、上下文仍当前，才能调用 rememberReadyResourceImage。仅 src 的远程图片、image: 本地图片继续展示，但不写此缓存。

**Step 4 — 增加同步 peek 与 effect 短路。**

- identity 加入 scope、generation、resource/storageKey/src 和 variant。使用明确分隔/数组序列化，不重复拼 scope。
- context 对象每次 render 可以重新捕获，但 effect 依赖使用 scope/generation 等稳定标量；不要将每次新建的 context/attempt 对象直接放入依赖，导致重复清理和加载。
- render 读取当前 context 和 ready；展示优先级：当前身份终态失败 → null/fallback；恢复中 → loadingFallback；当前成功 displayed → img；有效 ready → img；否则 loadingFallback。失败图不能因为 cachedSrc 仍存在继续挂着。
- effect 若 ready 命中，只做提交后的 touch，然后 return；不得 getResourceAccess、prepare 或重置为加载态。非首次 state initializer 也要支持同一实例 props 改变。
- 未命中继续 IO/rootMargin=240px 门控；门控状态绑定当前 identity，旧资源 nearViewport 不给新资源无条件放行。
- 直接挂载的远程图片默认 `loading="lazy"`，`eager=true` 时默认 eager；显式 loading 属性优先：`loading={loading ?? (eager ? "eager" : "lazy")}`。将 loading 从 rest props 解构，避免 spread 顺序覆盖。不要把 decoding 改成全局 sync。
- 冷加载仍走现有 prepare；不触碰 canvas-image-loader 常量与缓存。

**Step 5 — 运行 GREEN。** 验证原图/缩略图不会串；成功候选记录正确；lazy/eager 不改变调用方属性；ready 过期后重新走正常加载；本地 Blob 和普通 src 不进入 ready Map。

**Step 6 — 提交。** `fix(media): 图片展示 - 返回页面时避免重复准备已就绪资源`。

## Task 6：统一两种失败入口，并限制自动刷新次数

**Files:**
- Modify: `web/src/components/cached-resource-image.tsx` 的 load effect 与 handleImgError
- Test: `web/test/fixtures/resource-image-recovery.harness.tsx`
- Read: `web/src/pages/projects/drama-shared.tsx:188`
- Reference: `web/src/components/canvas/canvas-node-media-content.tsx:634` 的有限刷新模式

**Step 1 — 写失败用例。** 首次 prepare 失败、ready img error、刷新失败、刷新同 URL、刷新后 img 又失败、thumbnail 获取/解码失败回退 original、fallback=null。每例断言 prepare URL 序列、API payload variant/次数、最终 DOM 和 onError 通知次数。

**Step 2 — 运行 RED。** `bun test test/resource-image-recovery.test.ts`，确认当前失败入口未统一或预算未限制。

**Step 3 — 使用组件本地轮次状态实现有限恢复。** 不创建通用加载框架。每轮由 identity + 单调 attempt token 标识；保存刷新预算为 0/1、当前尝试 URL、来源 variant、AbortController。资源/variant/会话改变或外层重新挂载才开启新轮次；普通 render 和重复 error 事件不重置预算。

| 当前状态/事件 | 动作 | 下一状态 |
| --- | --- | --- |
| ready 命中 | 挂 img，不准备 | showing |
| cold miss | IO 后获取描述、prepare | showing 或 prepare-failed |
| prepare-failed | 排除取消/过时；执行现有 thumbnail→original 回退；仍失败且预算为 0 时同步消费预算 | refreshing |
| showing 的当前 img error | compare-and-delete ready；清当前失败 displayed；若预算为 0 则同步消费 | refreshing |
| refreshing | 先强刷调用方请求 variant，捕获 requestedAccess，再 prepare；必要时强刷 original 回退；允许新旧 URL 相同 | showing 或 failed |
| refreshing 收到重复 error | 不再发请求，不增加预算 | refreshing |
| 已消费预算后再次失败 | 移除失败 img、进入稳定 fallback | failed |
| 任意 await 后已取消/身份变化 | 不 setState、不写 ready、不调用外部错误回调 | abandoned |

消费刷新预算前再 peek 一次：若同资源/variant 已有另一实例成功记录的、不同于失败 URL 的有效 ready，则消费本轮唯一恢复预算并采用该地址，不删除它、不再强刷，也不重置本轮预算。若预算已经耗尽，则仍按终态失败处理，不能以跨实例换图绕开重试上限。同 URL 的新签名/相同签名不靠字符串差异判定刷新成功，仍由有限准备和实际 img 事件决定。

这里“回退”不是第二次自动恢复轮次：原请求 thumbnail 失败可试一次 original；恢复轮次中任何被重新尝试的来源变体都要 forceRefresh，不能刷新 thumbnail 后又拿旧 original。即使命中的 ready 上次来源是 original，恢复仍先重新请求调用方 variant 并捕获 requestedAccess，失败才转 original，这样记录时有完整的请求描述证明。ready 条目的 sourceVariant 能识别上次成功的来源。一次恢复轮次最多每个必要 variant 一次强刷，不允许循环往返。

**Step 4 — 处理准备缓存仍命中失败 URL 的终态。** 即使强刷后 prepare 返回既有位图，实际 img 仍可能失败；此时不得重开预算或宣告成功。第二次 img error 清 ready 并进入 failed。无需扩大或新增底层位图缓存失效 API 来保证无上限重试。

**Step 5 — 保留外部事件语义。** onLoad 继续透传，若内部组合则在实际事件后调用一次；onError 在当前 DOM 加载错误最终恢复失败时调用一次，不在一次可恢复错误时提前通知调用方隐藏图。prepare 的内部失败不伪造 React SyntheticEvent。已有本地 blob 恢复分支保留，并补相同的卸载/身份检查和可取消 controller。fallback=null 也必须卸掉失败 img，不能无限 error。

**Step 6 — 运行 GREEN。** 使用 fake timer 验证 MediaThumb 的 1500/5000ms 重挂载：最多三轮，最后 failed，不无限计时。若导入真实 MediaThumb 引入其他业务依赖，沿用隔离 fixture 的外部依赖 mock，不复制一个假的 MediaThumb 代替验证。

**Step 7 — 提交。** `fix(media): 图片恢复 - 在签名失效时有限刷新并稳定退出失败态`。

## Task 7：补齐跨组件和会话竞态回归

**Files:**
- Test: `web/test/fixtures/resource-image-recovery.harness.tsx`
- Modify（仅测试指出缺口时）: `web/src/services/api/resources.ts`、`web/src/components/cached-resource-image.tsx`

**Step 1 — 每个场景各写一个失败测试，单独执行。**

| ID | 精确时序 | 断言 |
| --- | --- | --- |
| R1 | A 开始 prepare → clear/B → clear/A → 原 prepare resolve | 不写 ready、不恢复旧图 |
| R2 | 图 A 开始加载 → 同实例 props 换图 B → A resolve/error | B 图和 B ready 不变 |
| R3 | thumbnail 开始 → variant 换 original → 旧结果返回 | original 不显示旧 thumbnail 地址 |
| R4 | 准备/刷新中 unmount → promise resolve/reject | 无后续状态、ready 或外部事件发布 |
| R5 | 两实例使用旧 URL → 一个恢复出新 URL → 旧 error 迟到 | 新 ready 不被删除；过时事件不发额外强刷 |
| R6 | 两实例同时 error → 两者请求强刷 | 同 key 在途强刷只发一次，两个实例各自结果正确 |
| R7 | React StrictMode effect setup/cleanup/setup | 不重复消费预算，不因 cleanup 将有效图片标记失败 |
| R8a | original 回退结果完成前，相关 key 强刷开始且仍未决 | 旧结果不能把刚失效的 ready 填回去 |
| R8b | thumbnail 强刷成功并写入新 ready 后，旧 original prepare 才完成 | requestedAccess 证明已失效，旧结果不能覆盖新 ready |

**Step 2 — 用 context + 当前轮次校验收敛缺口。** 每个 await 后以及写 ready、setDisplayed、setFailedIdentity、外部 callback 前，检查 controller、context、当前 identity/attempt。旧事件还要核对它对应的 URL 与当前尝试 URL。禁止仅检查 getActiveUserScope()===scope。

检查 `completeResourceAccessBatchEntry` 的持久化后失效分支：旧请求不得无条件 `accessCache.delete(entry.key)` 删除 A→B→A 后的新项；仅当当前值仍是该请求写入的 cachedEntry 时删除。持久存储继续按账号隔离并验证 deadline，不承诺 clear 能撤销已经提交给 IndexedDB 的旧事务或清除所有浏览器历史缓存。

**Step 3 — 运行合并行为测试。**

```powershell
bun test test/resource-image-recovery.test.ts test/canvas-image-geometry.test.ts test/canvas-media-performance.test.ts
```

预期全部通过；mock 不污染后续测试。若生产代码无需调整，此任务只提交测试。

**Step 4 — 提交。** `test(media): 图片会话 - 防止迟到加载结果覆盖当前展示`。

## Task 8：静态检查、构建与真实页面验收

**Files:**
- Update evidence: 本计划和 `docs/content/docs/progress/pending-test.mdx`（最终由 Task 10 汇总）
- Product files: 无预设新增改动；仅修复本批实际失败

**Step 1 — 运行专项组合。** 在 `web/` 执行 Task 7 的命令；记录真实计数、失败及耗时。不要因有新测试而修改旧测试以隐藏实现错误。

**Step 2 — 运行静态检查。** `bun run lint`。记录是否是已有问题；不要格式化全仓。

**Step 3 — 运行构建。** `bun run build`。该脚本已包含 `tsc --noEmit && vite build`，无需无理由重复 typecheck。预期退出码 0；测试文件不在 tsconfig include 内，因此专项 Bun 测试不可省略。

**Step 4 — 检查真实预览环境。** 优先使用已运行的实例；检查端口和登录条件。当前只编写计划，不启动服务。后续实施进入浏览器联调阶段时，若需启动，先读取 `.env.example` 与 Compose；后端数据目录必须显式指向 Git 忽略的 `.local/project-workbench-debug`，不使用 backend/data、不改真实数据库。不要依赖当前不存在的 `docs/AGENT_LOCAL_DEVELOPMENT.md`。

**Step 5 — 完成浏览器矩阵。** 使用真实短剧设定路由、素材库、一个使用 CachedResourceImage 的画布预览和主画布图片；不得创建独立 HTML 代替真实页面。

| 场景 | 操作 | 通过标准/记录 |
| --- | --- | --- |
| 角色图热返回 | 至少 16 张不同资源加载完成，切其他模块再返回，重复 10 次 | 有效 ready 项不显示应用黑底加载占位；不重新加入 prepare 队列；记录可见时间和网络字节 |
| 位图缓存超时 | 离开至少 65 秒再返回 | ready 仍有效时走同步 src；不依赖原 prepare 位图驻留 |
| 像素预算淘汰 | 加载不同图片使总像素超过 16MP，再回角色页 | 同上；记录实际行为，不推断浏览器一定无网络 |
| ready 条数淘汰 | 行为测试覆盖 501 条；真实大列表滚动 | 淘汰项正常冷加载，页面无持续增长的应用图片引用 |
| 浏览器缓存未命中 | 保持 JS 会话，测试环境禁用浏览器 HTTP 缓存后返回 | 不崩溃、不无限重试；允许网络与解码耗时，不要求秒开 |
| 签名失效 | 测试环境模拟旧地址失效或缩短受控测试 TTL | 有限强刷成功/稳定失败；原图回退正确；不改生产 TTL |
| 账号与卸载 | 加载中切账号、退出、快速切页 | 无前账号图片短暂替换当前资源、无旧写入 |
| 视口 | 大量离屏已命中项，滚动到可见区域 | 默认 lazy；无所有离屏图片同时无条件 eager 的回归 |
| 页面与主题 | 角色卡片/详情/历史图/素材库，明暗主题，窄屏滚动与弹窗 | 比例、alt、加载/失败状态、焦点与外部 onLoad/onError 正常 |
| 画布 | 拖动缩放、打开预览、切回工作区 | 无明显新增解码卡顿；记录同设备前后帧时间/长任务 |

浏览器观察中区分：memory/disk cache、304、完整下载；Resource Timing transferSize=0 单独不能证明命中（跨域限制也可能置零）。使用 Network 面板与 decode/截图录屏联合判断。只保存脱敏结果，不把带签名的 HAR/URL 写入仓库。

若执行视觉改动，按仓库约定调用 @visual-verdict，并把结论记录到对应 `.omx/state/<scope>/ralph-progress.json`；当前任务不修改视觉设计。没有真实浏览器证据时保留“待验收”，不得把 happy-dom 通过写成零闪烁通过。

**Step 6 — 审查 diff。** 在仓库根运行：

```powershell
git diff --check
git diff -- web/src/services/api/resources.ts web/src/components/cached-resource-image.tsx web/test/canvas-media-performance.test.ts
git status --short
```

逐一查看新增测试文件；确认 canvas-image-loader 常量无改动、无新依赖、无账号/签名/构建产物。存在已有无关 diff 时按文件和 hunk 对比基线，不能要求整个工作区只剩本任务文件。

## Task 9：首次冷加载测量与 CDN 条件实施

**Files:**
- Read: `backend/internal/assets/access.go`、`backend/internal/storage/delivery.go`、`backend/internal/storage/objects.go`
- Read: `web/src/pages/admin/settings/storage-settings-page.tsx` 及 `storage-settings-model.ts`
- Evidence: 本计划的脱敏验收记录；不新增后端代码或迁移

**Step 1 — 确认实际环境与配置归属。** 通过已授权后台只读查看实际 provider、s3Preset、CDNBaseURL、cdnAuthMode、requireCDN、资源所属存储配置；区分系统存储、用户自有存储与历史配置。只记录是否配置和非敏感属性，不导出密钥或完整 system_settings。没有环境访问权时标记待执行，不推测线上值。

**Step 2 — 测量当前真实 GET。** 同一浏览器、网络、资源，记录响应 Content-Length/实际传输字节、Cache-Control、TTFB、下载时间、decode 时间、delivery 和 actualVariant。分别测试浏览器缓存关闭的冷加载、启用后的第二次访问、16 张图页面完整加载；至少 5 组，报告中位数和范围，不对 5 组数据包装精确 P95。

**Step 3 — 判断是否需要配置变更。** 若已有合格 CDN，则验证路由与缓存命中；若 origin 慢而没有可用 CDN，列出所需绑定域名、HTTPS、回源、访问鉴权资料。`235KB/s` 是待复核历史说法，不能未经测量认定为供应商强制单流限速。

**Step 4 — 具备真实配置且已获该环境修改授权后操作。** 在后台填写绑定域名和匹配 provider 的鉴权模式，记录原非敏感配置以便回退；保持资源原有私有性，不为提速擅自切 public。`SignCDNURL` 的 qiniu 分支要求实际 provider 支持，不因 s3Preset 名称含 qiniu 就直接套用。域名必须已经完成绑定、证书、回源与鉴权配置，不能猜一个域名填入。

**Step 5 — 配置后重测。** `POST /api/resources/access` 必须返回正确 CDN host、`delivery="cdn"`、有效期限与可加载地址。实际图片 GET 返回预期缓存头；私有地址的有效/过期请求分别符合策略。再执行同一组冷/暖/16图测试，报告前后绝对值和比例，不以 delivery 字段代替测速。

**Step 6 — 回退或记录剩余瓶颈。** 若地址错误、鉴权失败或不改善，恢复本次调整的分发字段并验证原路径。若主要成本仍是大原图/解码，明确后续应单独评估真实 thumbnail 生成与分发；本批不偷偷扩大到后端图像处理链。若只有前端修复完成，交付必须注明首次冷加载尚未解决/尚未证实改善。

验收标准：配置和权限正确；重复访问能按实际响应缓存；在同条件实测中性能改善或如实说明没有改善。具体速度目标在基线后写入记录，不承诺固定毫秒数。

## Task 10：文档、交付与最终检查点

**Files:**
- Modify: `docs/content/docs/progress/pending-test.mdx`
- Modify: `docs/content/docs/backend/http-api.mdx:169`
- Update: 本计划

**Step 1 — 在 pending-test 追加独立条目。** 不覆盖已有段落，以下文本随实际结果填写而不是提前宣称通过：

```markdown
## 角色资产图与远程资源图片返回页面加载

- 已实现：[仅在代码完成后填写] 有效 ready URL 复用、有限刷新、变体与会话隔离；不扩大应用位图缓存。
- 已验证：[填写实际命令、计数、浏览器范围；未运行的明确注明]。
- [ ] 16 张不同资源加载完成后切页返回，不重复出现应用加载占位。
- [ ] 离开超过 60 秒、位图缓存淘汰、授权地址失效后行为符合预期。
- [ ] 账号切换、缩略图回退、素材库与画布预览无回归。
- [ ] 实际部署的首次冷加载及 CDN 冷暖缓存指标完成记录。
- 边界：浏览器缓存未命中、ready 过期/淘汰时允许重新加载；不保证任意条件下首帧像素已绘制。
```

**Step 2 — 增补资源访问专题。** 说明 ready 缓存仅内存且不替代授权；强刷绕过内存/持久地址但服务端可能返回同一签名；thumbnail 目前可能返回原图；HTTP 缓存与应用 ready、解码缓存是不同层。不要改变 API 请求/响应表、不要在根 README 重复计划。

**Step 3 — 检查文档和改动范围。** Markdown/MDX 无语法性复杂结构；仅文字与列表无需重跑整个文档站构建。若实际增添 MDX 组件/import，则在 docs/ 运行 `bun run types:check`。再次检查敏感信息及已有改动保留情况。

**Step 4 — 最终提交。** 对共享文档使用 `git add -p`，提交 `docs(media): 图片加载 - 明确缓存边界与部署验收项`，trailers 填写已执行和未执行验证。无需为“频繁提交”重复提交空检查点。

**Step 5 — 输出交付。** 列出实际 changed files、删除/复用的逻辑、专项测试与 lint/build 结果、浏览器证据、CDN 状态、剩余风险。分开报告“前端修复已完成”和“部署首次下载改善已证实”，不将任一代替另一。

## 最终验收清单

- [x] Task 1 当前基线失败已定位并合理修复；未删除有意义断言。（`web/test/canvas-media-performance.test.ts` 的 `moduleGroupSource` CRLF 换行规范化后 31 pass / 0 fail）
- [x] ready 仅保存地址元数据，500 条有界；渲染读取无续期和 LRU 写副作用。
- [x] scope/resource/requestedVariant 隔离；sourceVariant 回退关系正确；clear/generation 覆盖 A→B→A。
- [x] 同步快路径首轮有 src；后续 effect 不重复 getResourceAccess/prepare。
- [x] 首次准备失败和实际 img error 共用有限恢复预算；最终失败含 fallback=null 正确退出。
- [x] 强刷跳过持久读，合并并发；旧 pending/finally/error 不破坏新结果。
- [x] 成功备用 src 被正确展示；只记录真正成功且仍有效的访问描述地址。
- [x] 本地 image:/blob、普通 src、显式 loading/eager、onLoad/onError 不回归。
- [x] 原图片准备缓存与并发常量未变，无新依赖和新持久图片缓存。
- [x] 专项测试、lint、build 通过，或明确记录非本批阻塞而不宣称全部通过。（边界修复后复跑 `bun test test/resource-image-recovery.test.ts test/canvas-image-geometry.test.ts test/canvas-media-performance.test.ts` 43 pass / 0 fail，隔离子进程包含 24 项行为测试；`bun run lint` 0 error；`bun run build` 0 error，保留现有分包警告）
- [ ] 真实页面浏览器矩阵完成并记录；无浏览器条件时保留待验收。（待桌面浏览器手工验收）
- [ ] 首次下载瓶颈已测量；CDN 已验证或明确仍待运维，未虚构速度收益。（已实测开发库七牛 S3 源站 `yingce-dev.s3.cn-south-1.qiniucs.com` 返回 `200 OK`、`Cache-Control: private, max-age=14400, immutable`、`X-Kodo-Force-Download: true`，单张 3.03MB PNG TTFB `1.79s`、总下载 `4.25s~12.88s`；当前 `cdnBaseUrl=""`，真实 CDN 域名与鉴权绑定待运维配置后复测）
- [x] 文档增补本批修复与验证记录，未回滚已有修改；改动仍在工作区，未创建提交。

### 边界复核后的修复记录

- 修正取消控制流：远程访问描述与准备步骤前后校验 signal、组件身份及会话代次，AbortError 不再触发 original 回退。
- 修正重复原图强刷：已经回退 original 的失败直接结束该轮，不重新进入 original 分支。
- 修正错误事件竞态：恢复与终态在事件入口同步更新 ref；重复 error 不提前通知调用方，采用其他实例的新地址后忽略旧地址事件。
- 修正持久缓存竞态：异步主体入口先校验会话；资源持久缓存按 scope/key 串行执行读/删/写，跨 clear 保留已开始事务的顺序，结束即清理队列。
- 新增 11 项回归：取消准备、取消访问描述、单轮原图请求预算、远程/本地重复错误、两个真实组件错误复用与 onLoad、组件迟到 original 写入、失效微任务，以及跨会话 remove/set/过期删除顺序。原 Task 4 补真实 started barrier，R5 不再用未派发事件声称组件已验证。
- 取消、重复错误与请求预算的 5 项新测试在修复前失败、修复后通过；资源侧 4 项竞态测试同样先失败后通过。最终组合测试、lint 和 build 通过。
- 真实浏览器矩阵、CDN 配置与首次下载提速不在这次边界修复中宣称完成；存储永久未决仍可能阻塞同键队列。

## 依据与交接

浏览器语义依据：[MDN HTMLImageElement.decode](https://developer.mozilla.org/en-US/docs/Web/API/HTMLImageElement/decode)、[MDN Cache-Control](https://developer.mozilla.org/en-US/docs/Web/HTTP/Reference/Headers/Cache-Control)。代码事实以实施时仓库为准。

执行方式可选：本会话按任务实施并在独立切片使用原生子代理复核；或由用户另开会话通过 @executing-plans 逐项执行。共享 resources.ts 与 CachedResourceImage 的实现任务按依赖串行，不让多个实现者同时修改同一文件。无需为写计划创建新会话或启动应用。

## 附录 A：ready 缓存关键实现参考

以下是 Task 3/4 合并后的完整 helper 函数参考，依赖上文定义的类型、Map、context、displayAccessKey 及仓库已有函数。Task 3 单独落地时先不加 `.forceRefresh` 判断；Task 4 更换在途记录类型时补入。以行为测试为准，不为逐字匹配代码添加字符串断言。

```ts
export function peekReadyResourceImage(
    storageKey: string | undefined, variant: ResourceAccessVariant, context: ResourceAccessContext,
): ReadyResourceImage | undefined {
    if (!isResourceAccessContextCurrent(context)) return undefined;
    const id = resourceIdFromStorageKey(storageKey);
    if (!id) return undefined;
    const key = displayAccessKey(context.scope, id, variant);
    const ready = readyResourceImages.get(key);
    if (!ready || ready.expiresAt <= Date.now()) return undefined;
    if (accessRequests.get(key)?.forceRefresh || accessRequests.get(ready.sourceKey)?.forceRefresh) return undefined;
    return ready;
}

export function rememberReadyResourceImage(
    storageKey: string, variant: ResourceAccessVariant, access: ResourceAccess,
    requestedAccess: ResourceAccess | undefined, context: ResourceAccessContext,
): void {
    if (!isResourceAccessContextCurrent(context)) return;
    const id = resourceIdFromStorageKey(storageKey);
    if (!id || access.resourceId !== id) return;
    const key = displayAccessKey(context.scope, id, variant);
    const sourceKey = displayAccessKey(context.scope, id, access.requestedVariant);
    if (accessRequests.get(key)?.forceRefresh || accessRequests.get(sourceKey)?.forceRefresh) return;
    const now = Date.now();
    const source = accessCache.get(sourceKey);
    if (!source || source.value !== access || source.expiresAt <= now) return;
    const requested = accessCache.get(key);
    const currentRequested = requested && requested.expiresAt > now ? requested.value : undefined;
    if (currentRequested !== requestedAccess) return;
    const url = resolveResourceAccessURL(access.url);
    if (!url || /^(blob|data):/i.test(url)) return;
    for (const [oldKey, oldEntry] of readyResourceImages) {
        if (oldEntry.expiresAt <= now) readyResourceImages.delete(oldKey);
    }
    readyResourceImages.delete(key);
    while (readyResourceImages.size >= MAX_READY_RESOURCE_IMAGES) {
        const oldest = readyResourceImages.keys().next().value;
        if (oldest === undefined) break;
        readyResourceImages.delete(oldest);
    }
    readyResourceImages.set(key, {
        url,
        expiresAt: source.expiresAt,
        sourceKey,
        sourceVariant: access.requestedVariant,
        revision: access.revision,
    });
}

export function touchReadyResourceImage(
    storageKey: string, variant: ResourceAccessVariant, expectedURL: string, context: ResourceAccessContext,
): void {
    const ready = peekReadyResourceImage(storageKey, variant, context);
    if (!ready || ready.url !== expectedURL) return;
    const key = displayAccessKey(context.scope, resourceIdFromStorageKey(storageKey), variant);
    readyResourceImages.delete(key);
    readyResourceImages.set(key, ready);
}

export function forgetReadyResourceImage(
    storageKey: string, variant: ResourceAccessVariant, failedURL: string, context: ResourceAccessContext,
): void {
    if (!isResourceAccessContextCurrent(context)) return;
    const id = resourceIdFromStorageKey(storageKey);
    if (!id) return;
    const key = displayAccessKey(context.scope, id, variant);
    if (readyResourceImages.get(key)?.url === failedURL) readyResourceImages.delete(key);
}
```

`remember` 只接受访问描述，成功准备的事实由唯一组件调用点提供，不能在 getResourceAccess 返回时自动记录。cache helper 不创建 Image、不解码、不发请求。

## 附录 B：一个可落地的真实缓存测试

放入 Task 2 的隔离 fixture，使用前述 imageAccess 工厂。后续按 Task 3–7 的矩阵逐个补例；这个测试不能代替 DOM、竞态或浏览器验证。

```ts
test("成功记录后同步命中，clear 使旧 context 不能回填", async () => {
    const resources = await import("@/services/api/resources");
    const { apiClient } = await import("@/services/api/request");
    const adapter = apiClient.defaults.adapter;
    resources.clearResourceAccessCache();
    const value = imageAccess("ready-behavior");
    apiClient.defaults.adapter = async (config) => {
        expect(config.url).toBe("/resources/access");
        return {
            config, headers: {}, status: 200, statusText: "OK",
            data: { code: 0, data: { items: [{ resourceId: value.resourceId, access: value }] } },
        };
    };
    try {
        const key = "resource:ready-behavior";
        const context = resources.captureResourceAccessContext();
        expect(resources.peekReadyResourceImage(key, "thumbnail", context)).toBeUndefined();
        const access = await resources.getResourceAccess(key, "display", "thumbnail");
        resources.rememberReadyResourceImage(key, "thumbnail", access, access, context);
        const ready = resources.peekReadyResourceImage(key, "thumbnail", context);
        expect(ready?.url).toBe(value.url);
        expect(ready?.expiresAt).toBe(new Date(value.refreshAt).getTime());
        resources.clearResourceAccessCache();
        resources.rememberReadyResourceImage(key, "thumbnail", access, access, context);
        expect(resources.peekReadyResourceImage(key, "thumbnail", context)).toBeUndefined();
        expect(resources.peekReadyResourceImage(key, "thumbnail", resources.captureResourceAccessContext())).toBeUndefined();
    } finally {
        apiClient.defaults.adapter = adapter;
        resources.clearResourceAccessCache();
    }
});
```
