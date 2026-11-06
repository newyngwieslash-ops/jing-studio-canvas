import { afterAll, beforeEach, expect, mock, test } from "bun:test";
import { Window } from "happy-dom";

const dom = new Window({ url: "http://localhost/" });
const originalGlobals = new Map<string, PropertyDescriptor | undefined>();
const installedGlobals = [
    "window",
    "document",
    "navigator",
    "localStorage",
    "HTMLElement",
    "HTMLImageElement",
    "SVGElement",
    "Element",
    "Node",
    "Event",
    "CustomEvent",
    "ShadowRoot",
    "MutationObserver",
    "ResizeObserver",
    "getComputedStyle",
    "IS_REACT_ACT_ENVIRONMENT",
] as const;

for (const key of installedGlobals) {
    originalGlobals.set(key, Object.getOwnPropertyDescriptor(globalThis, key));
}

for (const [key, value] of Object.entries({
    window: dom,
    document: dom.document,
    navigator: dom.navigator,
    localStorage: dom.localStorage,
    HTMLElement: dom.HTMLElement,
    HTMLImageElement: dom.HTMLImageElement,
    SVGElement: dom.SVGElement,
    Element: dom.Element,
    Node: dom.Node,
    Event: dom.Event,
    CustomEvent: dom.CustomEvent,
    ShadowRoot: dom.ShadowRoot,
    MutationObserver: dom.MutationObserver,
    ResizeObserver: dom.ResizeObserver,
    getComputedStyle: dom.getComputedStyle.bind(dom),
    IS_REACT_ACT_ENVIRONMENT: true,
})) {
    Object.defineProperty(globalThis, key, { configurable: true, writable: true, value });
}

export function deferred<T>() {
    let resolve!: (value: T | PromiseLike<T>) => void;
    let reject!: (reason?: unknown) => void;
    const promise = new Promise<T>((res, rej) => {
        resolve = res;
        reject = rej;
    });
    return { promise, resolve, reject };
}

type PersistedHooks = {
    onGet?: (scope: string, name: string, bucket: Map<string, string>) => Promise<string | null> | string | null;
    onSet?: (scope: string, name: string, value: string, bucket: Map<string, string>) => Promise<void> | void;
    onRemove?: (scope: string, name: string, bucket: Map<string, string>) => Promise<void> | void;
};

const persistedBuckets = new Map<string, Map<string, string>>();
let persistedHooks: PersistedHooks = {};

function getScopeBucket(scope: string) {
    let bucket = persistedBuckets.get(scope);
    if (!bucket) {
        bucket = new Map<string, string>();
        persistedBuckets.set(scope, bucket);
    }
    return bucket;
}

const userScopeModule = await import("@/lib/user-scope");

mock.module("@/lib/localforage-storage", () => {
    const storageFor = (explicitScope?: string) => ({
        getItem: async (name: string) => {
            const scope = explicitScope || userScopeModule.getActiveUserScope();
            const bucket = getScopeBucket(scope);
            if (persistedHooks.onGet) return await persistedHooks.onGet(scope, name, bucket);
            return bucket.get(name) ?? null;
        },
        setItem: async (name: string, value: string) => {
            if (userScopeModule.isUserScopedPersistenceSuppressed()) return;
            const scope = explicitScope || userScopeModule.getActiveUserScope();
            const bucket = getScopeBucket(scope);
            if (persistedHooks.onSet) {
                await persistedHooks.onSet(scope, name, value, bucket);
                return;
            }
            bucket.set(name, value);
        },
        removeItem: async (name: string) => {
            if (userScopeModule.isUserScopedPersistenceSuppressed()) return;
            const scope = explicitScope || userScopeModule.getActiveUserScope();
            const bucket = getScopeBucket(scope);
            if (persistedHooks.onRemove) {
                await persistedHooks.onRemove(scope, name, bucket);
                return;
            }
            bucket.delete(name);
        },
    });
    return {
        localForageStorageForScope: (scope?: string) => storageFor(scope),
        localForageStorage: storageFor(),
    };
});

type PrepareCall = {
    src: string;
    signal: AbortSignal;
    resolve: (image?: HTMLImageElement) => void;
    reject: (reason?: unknown) => void;
};

const prepareCalls: PrepareCall[] = [];
let prepareImpl: ((src: string, signal: AbortSignal) => Promise<HTMLImageElement>) | undefined;

mock.module("@/services/canvas-image-loader", () => ({
    prepareCanvasImage: (src: string, signal: AbortSignal) => {
        if (prepareImpl) return prepareImpl(src, signal);
        const d = deferred<HTMLImageElement>();
        const onAbort = () => d.reject(new DOMException("图片加载已取消", "AbortError"));
        if (signal.aborted) {
            onAbort();
            return d.promise;
        }
        signal.addEventListener("abort", onAbort, { once: true });
        prepareCalls.push({
            src,
            signal,
            resolve: (img) => {
                signal.removeEventListener("abort", onAbort);
                const el = img ?? (dom.document.createElement("img") as unknown as HTMLImageElement);
                d.resolve(el);
            },
            reject: (reason) => {
                signal.removeEventListener("abort", onAbort);
                d.reject(reason ?? new Error("图片加载失败"));
            },
        });
        return d.promise;
    },
    isCanvasImageActive: () => false,
    subscribeCanvasImageActivity: () => () => {},
}));

let resolveImageUrlImpl: ((storageKey: string, fallback?: string) => Promise<string>) | undefined;
mock.module("@/services/image-storage", () => ({
    resolveImageUrl: async (storageKey: string, fallback = "") => {
        if (resolveImageUrlImpl) return await resolveImageUrlImpl(storageKey, fallback);
        return fallback;
    },
}));

const React = await import("react");
const { act } = React;
const { createRoot } = await import("react-dom/client");
const { renderToStaticMarkup } = await import("react-dom/server");
const resources = await import("@/services/api/resources");
const { apiClient } = await import("@/services/api/request");
const { CachedResourceImage } = await import("@/components/cached-resource-image");
const { MediaThumb } = await import("@/pages/projects/drama-shared");

const originalAdapter = apiClient.defaults.adapter;
const realDateNow = Date.now.bind(Date);
const originalIntersectionObserver = Object.getOwnPropertyDescriptor(globalThis, "IntersectionObserver");

let activeRoot: ReturnType<typeof createRoot> | null = null;
let activeContainer: HTMLElement | null = null;

function resetHarnessState() {
    if (activeRoot) {
        act(() => {
            activeRoot?.unmount();
        });
        activeRoot = null;
    }
    if (activeContainer) {
        activeContainer.remove();
        activeContainer = null;
    }
    dom.document.body.innerHTML = "";
    dom.localStorage.clear();
    userScopeModule.setActiveUserScope("user-a");
    resources.clearResourceAccessCache();
    persistedBuckets.clear();
    persistedHooks = {};
    prepareCalls.length = 0;
    prepareImpl = undefined;
    resolveImageUrlImpl = undefined;
    apiClient.defaults.adapter = originalAdapter;
    Date.now = realDateNow;
    if (originalIntersectionObserver) {
        Object.defineProperty(globalThis, "IntersectionObserver", originalIntersectionObserver);
    } else {
        delete (globalThis as Record<string, unknown>).IntersectionObserver;
    }
}

beforeEach(() => {
    resetHarnessState();
});

afterAll(() => {
    resetHarnessState();
    mock.restore();
    for (const [key, descriptor] of originalGlobals) {
        if (descriptor) {
            Object.defineProperty(globalThis, key, descriptor);
        } else {
            delete (globalThis as Record<string, unknown>)[key];
        }
    }
});

function imageAccess(id: string, variant: "original" | "thumbnail" = "thumbnail", overrides: Partial<import("@/services/api/resources").ResourceAccess> = {}) {
    const now = Date.now();
    return {
        resourceId: id,
        requestedVariant: variant,
        actualVariant: "original" as const,
        url: `https://images.example.invalid/${id}-${variant}.png`,
        delivery: "origin" as const,
        issuedAt: new Date(now).toISOString(),
        expiresAt: new Date(now + 3_600_000).toISOString(),
        refreshAt: new Date(now + 2_000_000).toISOString(),
        revision: "test-revision",
        ...overrides,
    };
}

test("基线：同资源同 variant 连续 getResourceAccess 共享缓存，clear 后重新请求", async () => {
    let requestCount = 0;
    apiClient.defaults.adapter = async (config) => {
        requestCount += 1;
        expect(config.url).toBe("/resources/access");
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        return {
            config,
            headers: {},
            status: 200,
            statusText: "OK",
            data: {
                code: 0,
                data: {
                    items: items.map((item) => ({
                        resourceId: item.resourceId,
                        access: imageAccess(item.resourceId, item.variant),
                    })),
                },
            },
        };
    };

    const first = await resources.getResourceAccess("resource:baseline-1", "display", "thumbnail");
    const second = await resources.getResourceAccess("resource:baseline-1", "display", "thumbnail");
    expect(first.url).toBe("https://images.example.invalid/baseline-1-thumbnail.png");
    expect(second.url).toBe(first.url);
    expect(requestCount).toBe(1);

    resources.clearResourceAccessCache();
    persistedBuckets.clear();
    const third = await resources.getResourceAccess("resource:baseline-1", "display", "thumbnail");
    expect(third.url).toBe(first.url);
    expect(requestCount).toBe(2);
});

test("Task 3：成功记录后同步命中，未记录不命中，clear 与 A→B→A 使旧 context 不能回填", async () => {
    const value = imageAccess("ready-behavior");
    apiClient.defaults.adapter = async (config) => {
        expect(config.url).toBe("/resources/access");
        return {
            config,
            headers: {},
            status: 200,
            statusText: "OK",
            data: { code: 0, data: { items: [{ resourceId: value.resourceId, access: value }] } },
        };
    };

    const key = "resource:ready-behavior";
    const contextA1 = resources.captureResourceAccessContext();
    expect(resources.peekReadyResourceImage(key, "thumbnail", contextA1)).toBeUndefined();

    const access = await resources.getResourceAccess(key, "display", "thumbnail");
    // 仅获取访问描述不自动进入 ready 缓存
    expect(resources.peekReadyResourceImage(key, "thumbnail", contextA1)).toBeUndefined();

    resources.rememberReadyResourceImage(key, "thumbnail", access, access, contextA1);
    const ready = resources.peekReadyResourceImage(key, "thumbnail", contextA1);
    expect(ready?.url).toBe(value.url);
    expect(ready?.expiresAt).toBe(new Date(value.refreshAt).getTime());
    expect(ready?.sourceVariant).toBe("thumbnail");
    expect(ready?.revision).toBe("test-revision");
    // 不同 variant 与不同 scope 隔离
    expect(resources.peekReadyResourceImage(key, "original", contextA1)).toBeUndefined();

    // A -> B -> A 切换后，旧 contextA1 的 generation 已失效，不能写入或读取
    userScopeModule.setActiveUserScope("user-b");
    resources.clearResourceAccessCache();
    userScopeModule.setActiveUserScope("user-a");
    resources.clearResourceAccessCache();

    const contextA2 = resources.captureResourceAccessContext();
    expect(resources.isResourceAccessContextCurrent(contextA1)).toBe(false);
    expect(resources.isResourceAccessContextCurrent(contextA2)).toBe(true);
    resources.rememberReadyResourceImage(key, "thumbnail", access, access, contextA1);
    expect(resources.peekReadyResourceImage(key, "thumbnail", contextA1)).toBeUndefined();
    expect(resources.peekReadyResourceImage(key, "thumbnail", contextA2)).toBeUndefined();
});

test("Task 3：500 条容量上限、touch LRU 顺序、不续期、TTL 过期、thumbnail->original 回退绑定及 forget 比较删除", async () => {
    const baseNow = realDateNow();
    let currentNow = baseNow;
    Date.now = () => currentNow;

    apiClient.defaults.adapter = async (config) => {
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        return {
            config,
            headers: {},
            status: 200,
            statusText: "OK",
            data: {
                code: 0,
                data: {
                    items: items.map((item) => {
                        if (item.resourceId === "fallback-res" && item.variant === "thumbnail") {
                            return { resourceId: item.resourceId, error: { msg: "缩略图不可用" } };
                        }
                        return {
                            resourceId: item.resourceId,
                            access: imageAccess(item.resourceId, item.variant, {
                                issuedAt: new Date(currentNow).toISOString(),
                                refreshAt: new Date(currentNow + 60_000).toISOString(),
                                expiresAt: new Date(currentNow + 120_000).toISOString(),
                            }),
                        };
                    }),
                },
            },
        };
    };

    const context = resources.captureResourceAccessContext();
    const keys500 = Array.from({ length: 500 }, (_, index) => `resource:item-${index + 1}`);
    const accesses500 = await Promise.all(keys500.map((key) => resources.getResourceAccess(key, "display", "thumbnail")));
    for (let i = 0; i < 500; i++) {
        resources.rememberReadyResourceImage(keys500[i], "thumbnail", accesses500[i], accesses500[i], context);
    }
    expect(resources.peekReadyResourceImage("resource:item-1", "thumbnail", context)?.url).toContain("item-1");

    // touch item-1：移到 LRU 尾部，但不延长 expiresAt
    const item1Before = resources.peekReadyResourceImage("resource:item-1", "thumbnail", context)!;
    currentNow = baseNow + 10_000;
    resources.touchReadyResourceImage("resource:item-1", "thumbnail", item1Before.url, context);
    const item1After = resources.peekReadyResourceImage("resource:item-1", "thumbnail", context)!;
    expect(item1After.expiresAt).toBe(item1Before.expiresAt);

    // 插入第 501 条时，item-1 因被 touch 保留，item-2 被淘汰
    const acc501 = await resources.getResourceAccess("resource:item-501", "display", "thumbnail");
    resources.rememberReadyResourceImage("resource:item-501", "thumbnail", acc501, acc501, context);
    expect(resources.peekReadyResourceImage("resource:item-1", "thumbnail", context)).toBeDefined();
    expect(resources.peekReadyResourceImage("resource:item-2", "thumbnail", context)).toBeUndefined();
    expect(resources.peekReadyResourceImage("resource:item-501", "thumbnail", context)).toBeDefined();

    // thumbnail 失败回退到 original 时，requestedAccess 为 undefined，sourceVariant 记录为 original
    await expect(resources.getResourceAccess("resource:fallback-res", "display", "thumbnail")).rejects.toThrow();
    const origAcc = await resources.getResourceAccess("resource:fallback-res", "display", "original");
    resources.rememberReadyResourceImage("resource:fallback-res", "thumbnail", origAcc, undefined, context);
    const fallbackReady = resources.peekReadyResourceImage("resource:fallback-res", "thumbnail", context);
    expect(fallbackReady?.url).toBe(origAcc.url);
    expect(fallbackReady?.sourceVariant).toBe("original");

    // forgetReadyResourceImage：错误的旧 URL 不误删当前有效项
    resources.forgetReadyResourceImage("resource:fallback-res", "thumbnail", "https://images.example.invalid/wrong.png", context);
    expect(resources.peekReadyResourceImage("resource:fallback-res", "thumbnail", context)).toBeDefined();
    resources.forgetReadyResourceImage("resource:fallback-res", "thumbnail", origAcc.url, context);
    expect(resources.peekReadyResourceImage("resource:fallback-res", "thumbnail", context)).toBeUndefined();

    // 推进时间至 item-1 的 refreshAt 过期边界后，peek 返回 undefined
    currentNow = baseNow + 60_001;
    expect(resources.peekReadyResourceImage("resource:item-1", "thumbnail", context)).toBeUndefined();

    // 旧 access 对象不能借新同键描述的 TTL 写入
    const staleKey = "resource:stale-descriptor";
    const oldAcc = await resources.getResourceAccess(staleKey, "display", "thumbnail");
    currentNow += 120_000;
    persistedBuckets.clear();
    const newAcc = await resources.getResourceAccess(staleKey, "display", "thumbnail", "", { forceRefresh: true });
    expect(newAcc).not.toBe(oldAcc);
    resources.rememberReadyResourceImage(staleKey, "thumbnail", oldAcc, oldAcc, context);
    expect(resources.peekReadyResourceImage(staleKey, "thumbnail", context)).toBeUndefined();
});

test("Task 4：forceRefresh 绕过持久缓存（含 removeItem 失败）、合并并发强刷、使关联 ready 失效并拦截跨会话污染", async () => {
    let postSeq = 0;
    apiClient.defaults.adapter = async (config) => {
        postSeq += 1;
        const seq = postSeq;
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        return {
            config,
            headers: {},
            status: 200,
            statusText: "OK",
            data: {
                code: 0,
                data: {
                    items: items.map((item) => ({
                        resourceId: item.resourceId,
                        access: imageAccess(item.resourceId, item.variant, {
                            url: `https://images.example.invalid/${item.resourceId}-${item.variant}-v${seq}.png`,
                            revision: `rev-${seq}`,
                        }),
                    })),
                },
            },
        };
    };

    // 1. 持久缓存有旧 URL，且 removeItem 抛错时，forceRefresh 仍必须跳过 readPersistedDisplayAccess 并 POST 获取新值
    const key1 = "resource:refresh-persist";
    const first = await resources.getResourceAccess(key1, "display", "thumbnail");
    expect(first.url).toContain("-v1.png");
    expect(postSeq).toBe(1);

    persistedHooks.onRemove = async () => {
        throw new Error("IndexedDB remove failed");
    };
    const refreshed1 = await resources.getResourceAccess(key1, "display", "thumbnail", "", { forceRefresh: true });
    expect(refreshed1.url).toContain("-v2.png");
    expect(postSeq).toBe(2);
    persistedHooks.onRemove = undefined;

    // 2. 普通请求卡在持久读取时，两个强刷紧接着进入，以及后续新普通请求进入：
    //    - 强刷立即登记并合并为同一次后端 POST
    //    - 强刷期间 peek/remember 被挡住
    //    - 旧持久读取完成后不作为强刷或新普通读取的结果
    const key2 = "resource:refresh-coalesce";
    const initial2 = await resources.getResourceAccess(key2, "display", "thumbnail");
    const ctx2 = resources.captureResourceAccessContext();
    resources.rememberReadyResourceImage(key2, "thumbnail", initial2, initial2, ctx2);
    expect(resources.peekReadyResourceImage(key2, "thumbnail", ctx2)?.url).toBe(initial2.url);

    // 清空内存但保留持久缓存（值为 initial2），让下一次普通读取卡在 persisted getItem
    // 借由过期时间推进触发内存 miss 而持久缓存仍有效
    const holdGet = deferred<string | null>();
    const savedPersisted = getScopeBucket("user-a").get("resource-access-v1:refresh-coalesce:thumbnail") ?? null;
    expect(savedPersisted).not.toBeNull();

    // 写入一条即将过期的内存项或让普通请求读取持久缓存：用新资源 key2b 预先种入持久缓存
    const key2b = "resource:refresh-inflight";
    const stalePersistedEntry = {
        value: imageAccess("refresh-inflight", "thumbnail", { url: "https://images.example.invalid/persisted-stale.png" }),
        expiresAt: Date.now() + 600_000,
    };
    getScopeBucket("user-a").set("resource-access-v1:refresh-inflight:thumbnail", JSON.stringify(stalePersistedEntry));

    let interceptGet = true;
    const getStarted = deferred<void>();
    persistedHooks.onGet = async (_scope, name, bucket) => {
        if (interceptGet && name.includes("refresh-inflight")) {
            getStarted.resolve();
            return await holdGet.promise;
        }
        return bucket.get(name) ?? null;
    };

    const normalPending = resources.getResourceAccess(key2b, "display", "thumbnail");
    await getStarted.promise;
    const postBeforeForce = postSeq;
    const force1 = resources.getResourceAccess(key2b, "display", "thumbnail", "", { forceRefresh: true });
    const force2 = resources.getResourceAccess(key2b, "display", "thumbnail", "", { forceRefresh: true });
    const normalAfterForce = resources.getResourceAccess(key2b, "display", "thumbnail");
    expect(force1).toBe(force2);
    expect(normalAfterForce).toBe(force1);
    let forceSettled = false;
    void force1.then(() => { forceSettled = true; });
    await Promise.resolve();
    expect(postSeq).toBe(postBeforeForce);
    expect(forceSettled).toBe(false);

    // 释放卡住的普通持久读取（返回旧持久值）
    interceptGet = false;
    holdGet.resolve(JSON.stringify(stalePersistedEntry));
    const normalRes = await normalPending;
    expect(normalRes.url).toBe("https://images.example.invalid/persisted-stale.png");

    const [fRes1, fRes2, afterRes] = await Promise.all([force1, force2, normalAfterForce]);
    expect(postSeq).toBe(postBeforeForce + 1);
    expect(fRes1.url).toContain("refresh-inflight-thumbnail-v");
    expect(fRes1.url).not.toBe("https://images.example.invalid/persisted-stale.png");
    expect(fRes2.url).toBe(fRes1.url);
    expect(afterRes.url).toBe(fRes1.url);
    persistedHooks.onGet = undefined;

    // 3. sourceVariant=original 的 ready thumbnail：当 original 触发强刷时，依赖它的 ready thumbnail 一并失效
    const key3 = "resource:fallback-dep";
    const origAcc3 = await resources.getResourceAccess(key3, "display", "original");
    const ctx3 = resources.captureResourceAccessContext();
    resources.rememberReadyResourceImage(key3, "thumbnail", origAcc3, undefined, ctx3);
    expect(resources.peekReadyResourceImage(key3, "thumbnail", ctx3)?.url).toBe(origAcc3.url);

    const refreshOrigPromise = resources.getResourceAccess(key3, "display", "original", "", { forceRefresh: true });
    // 强刷刚登记（尚未完成）时，关联的 thumbnail ready 已被立即失效且不可被 remember
    expect(resources.peekReadyResourceImage(key3, "thumbnail", ctx3)).toBeUndefined();
    resources.rememberReadyResourceImage(key3, "thumbnail", origAcc3, undefined, ctx3);
    expect(resources.peekReadyResourceImage(key3, "thumbnail", ctx3)).toBeUndefined();
    await refreshOrigPromise;

    // 4. 删除真正开始后切账号，完成后必须拒绝旧结果且不能继续 POST。
    const holdRemove = deferred<void>();
    const removeStarted = deferred<void>();
    persistedHooks.onRemove = async (_scope, name, bucket) => {
        removeStarted.resolve();
        await holdRemove.promise;
        bucket.delete(name);
    };
    const abortingForce = resources.getResourceAccess(key3, "display", "original", "", { forceRefresh: true });
    await removeStarted.promise;
    userScopeModule.setActiveUserScope("user-b");
    resources.clearResourceAccessCache();
    userScopeModule.setActiveUserScope("user-a");
    resources.clearResourceAccessCache();
    const postBeforeRelease = postSeq;
    holdRemove.resolve();
    await expect(abortingForce).rejects.toThrow("资源请求已因账号切换失效");
    expect(postSeq).toBe(postBeforeRelease);
    persistedHooks.onRemove = undefined;
});

test("Task 4：强刷排入微任务后会话失效，不得开始持久删除", async () => {
    let removeCalls = 0;
    let postCalls = 0;
    persistedHooks.onRemove = async (_scope, name, bucket) => {
        removeCalls += 1;
        bucket.delete(name);
    };
    apiClient.defaults.adapter = async () => {
        postCalls += 1;
        throw new Error("过时请求不应访问网络");
    };
    const request = resources.getResourceAccess("resource:stale-microtask", "display", "original", "", { forceRefresh: true });
    resources.clearResourceAccessCache();
    userScopeModule.setActiveUserScope("user-b");
    await expect(request).rejects.toThrow("资源请求已因账号切换失效");
    expect(removeCalls).toBe(0);
    expect(postCalls).toBe(0);
});

for (const pendingOperation of ["remove", "set", "expired-read"] as const) {
    test(`Task 4：跨会话 ${pendingOperation} 与新会话持久写入按同键串行`, async () => {
        setupDefaultVersioningAdapter();
        const id = `serialized-${pendingOperation}`;
        const key = `resource:${id}`;
        const persistedKey = `resource-access-v1:${id}:original`;
        const started = deferred<void>();
        const release = deferred<void>();
        let held = false;
        let freshWriteStarted = false;
        if (pendingOperation === "expired-read") {
            getScopeBucket("user-a").set(persistedKey, JSON.stringify({ value: imageAccess(id, "original"), expiresAt: Date.now() - 1 }));
        }
        persistedHooks.onRemove = async (_scope, name, bucket) => {
            if (pendingOperation !== "set" && !held) {
                held = true;
                started.resolve();
                await release.promise;
            }
            bucket.delete(name);
        };
        persistedHooks.onSet = async (_scope, name, value, bucket) => {
            if (pendingOperation === "set" && !held) {
                held = true;
                started.resolve();
                await release.promise;
            } else {
                freshWriteStarted = true;
            }
            bucket.set(name, value);
        };
        const stale = resources.getResourceAccess(key, "display", "original", "", { forceRefresh: pendingOperation === "remove" });
        const staleResult = stale.then(() => null, (error) => error);
        await started.promise;
        resources.clearResourceAccessCache();
        userScopeModule.setActiveUserScope("user-b");
        resources.clearResourceAccessCache();
        userScopeModule.setActiveUserScope("user-a");
        const fresh = resources.getResourceAccess(key, "display", "original", "", { forceRefresh: true });
        let overlapped = false;
        try {
            await new Promise((resolve) => setTimeout(resolve, 80));
            overlapped = freshWriteStarted;
        } finally {
            release.resolve();
        }
        const error = await staleResult;
        const access = await fresh;
        expect(overlapped).toBe(false);
        expect(error?.name).toBe("AbortError");
        expect(JSON.parse(getScopeBucket("user-a").get(persistedKey)!).value.url).toBe(access.url);
        expect(await resources.getResourceAccess(key, "display", "original")).toBe(access);
    });
}

async function flushAsync(ms = 25) {
    await act(async () => {
        await new Promise((resolve) => setTimeout(resolve, ms));
    });
}

function mount(node: React.ReactNode) {
    if (!activeContainer) {
        activeContainer = dom.document.createElement("div");
        dom.document.body.appendChild(activeContainer);
        activeRoot = createRoot(activeContainer);
    }
    act(() => {
        activeRoot!.render(node);
    });
    return activeContainer;
}

function unmountActive() {
    if (activeRoot) {
        act(() => {
            activeRoot!.unmount();
        });
        activeRoot = null;
    }
    if (activeContainer) {
        activeContainer.remove();
        activeContainer = null;
    }
}

test("Task 5：首次加载准备完成后卸载，重新初始渲染与挂载直接输出 img（不等待 IO、不重复 prepare/API），且隔离 variant/备用 src/loading 属性", async () => {
    let accessPostCount = 0;
    apiClient.defaults.adapter = async (config) => {
        accessPostCount += 1;
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        return {
            config,
            headers: {},
            status: 200,
            statusText: "OK",
            data: {
                code: 0,
                data: {
                    items: items.map((item) => ({
                        resourceId: item.resourceId,
                        access: imageAccess(item.resourceId, item.variant),
                    })),
                },
            },
        };
    };

    // 安装不会主动触发 isIntersecting 的 IntersectionObserver 替身
    const observerCallbacks: Array<(entries: Array<{ isIntersecting: boolean }>) => void> = [];
    Object.defineProperty(globalThis, "IntersectionObserver", {
        configurable: true,
        writable: true,
        value: class {
            private cb: (entries: Array<{ isIntersecting: boolean }>) => void;
            constructor(cb: (entries: Array<{ isIntersecting: boolean }>) => void) {
                this.cb = cb;
                observerCallbacks.push(cb);
            }
            observe() {}
            disconnect() {}
        },
    });

    // 1. 首次冷挂载：未进入视口前显示 loadingFallback，不请求
    const container = mount(
        <CachedResourceImage
            storageKey="resource:cast-1"
            variant="thumbnail"
            alt="沈彻"
            loadingFallback={<span data-testid="loading">加载中</span>}
            fallback={<span data-testid="fallback">失败</span>}
        />,
    );
    expect(container.querySelector('[data-testid="loading"]')).not.toBeNull();
    expect(container.querySelector("img")).toBeNull();
    expect(prepareCalls.length).toBe(0);

    // 触发进入视口 -> 批量请求 getResourceAccess -> 进入 prepareCanvasImage
    act(() => {
        observerCallbacks[0]?.([{ isIntersecting: true }]);
    });
    await flushAsync(25);
    expect(accessPostCount).toBe(1);
    expect(prepareCalls.length).toBe(1);
    expect(prepareCalls[0].src).toBe("https://images.example.invalid/cast-1-thumbnail.png");

    // 完成 prepare -> 显示 <img> 且写入 ready
    await act(async () => {
        prepareCalls[0].resolve();
    });
    const firstImg = container.querySelector("img");
    expect(firstImg?.getAttribute("src")).toBe("https://images.example.invalid/cast-1-thumbnail.png");
    expect(firstImg?.getAttribute("loading")).toBe("lazy");
    expect(container.querySelector('[data-testid="loading"]')).toBeNull();

    // 卸载组件（模拟切换到其他工作区模块）
    unmountActive();

    // 2. 使用 renderToStaticMarkup 验证首轮初始渲染（effects 未执行）已直接输出正确 <img src>，无 loadingFallback
    const staticHtml = renderToStaticMarkup(
        <CachedResourceImage
            storageKey="resource:cast-1"
            variant="thumbnail"
            alt="沈彻"
            loadingFallback={<span data-testid="loading">加载中</span>}
            fallback={<span data-testid="fallback">失败</span>}
        />,
    );
    expect(staticHtml).toContain('src="https://images.example.invalid/cast-1-thumbnail.png"');
    expect(staticHtml).toContain('loading="lazy"');
    expect(staticHtml).not.toContain("加载中");

    // 3. 在 happy-dom 中真正重新挂载（此时 IntersectionObserver 不触发任何回调），仍直接渲染 img，且不增加 prepareCalls 或 API 请求
    const remounted = mount(
        <CachedResourceImage
            storageKey="resource:cast-1"
            variant="thumbnail"
            alt="沈彻"
            eager
            loading="eager"
            loadingFallback={<span data-testid="loading">加载中</span>}
            fallback={<span data-testid="fallback">失败</span>}
        />,
    );
    await flushAsync(25);
    const secondImg = remounted.querySelector("img");
    expect(secondImg?.getAttribute("src")).toBe("https://images.example.invalid/cast-1-thumbnail.png");
    expect(secondImg?.getAttribute("loading")).toBe("eager");
    expect(remounted.querySelector('[data-testid="loading"]')).toBeNull();
    expect(prepareCalls.length).toBe(1);
    expect(accessPostCount).toBe(1);

    // 4. 同一组件实例切换 variant="original"（未 ready）：不复用 thumbnail 的 ready，重新受 IO 门控
    mount(
        <CachedResourceImage
            storageKey="resource:cast-1"
            variant="original"
            alt="沈彻"
            loadingFallback={<span data-testid="loading">加载中</span>}
            fallback={<span data-testid="fallback">失败</span>}
        />,
    );
    expect(remounted.querySelector("img")).toBeNull();
    expect(remounted.querySelector('[data-testid="loading"]')).not.toBeNull();

    // 5. 备用 src 回退：当远程 original prepare 失败但备用 src prepare 成功时，展示备用 src，且不把失败的远程地址或非授权备用 src 记入 ready
    act(() => {
        observerCallbacks[observerCallbacks.length - 1]?.([{ isIntersecting: true }]);
    });
    await flushAsync(25);
    expect(prepareCalls.length).toBe(2);
    expect(prepareCalls[1].src).toBe("https://images.example.invalid/cast-1-original.png");

    // 重新挂载一个带备用 src 的 original 实例测试备用 src 传播
    unmountActive();
    prepareCalls.length = 0;
    const withBackup = mount(
        <CachedResourceImage
            storageKey="resource:cast-backup"
            src="https://cdn.backup.invalid/backup.png"
            variant="original"
            eager
            alt="备用"
            loadingFallback={<span data-testid="loading">加载中</span>}
        />,
    );
    await flushAsync(25);
    expect(prepareCalls.length).toBe(1);
    expect(prepareCalls[0].src).toBe("https://images.example.invalid/cast-backup-original.png");
    await act(async () => {
        prepareCalls[0].reject(new Error("主地址解码失败"));
    });
    await flushAsync(5);
    expect(prepareCalls.length).toBe(2);
    expect(prepareCalls[1].src).toBe("https://cdn.backup.invalid/backup.png");
    await act(async () => {
        prepareCalls[1].resolve();
    });
    expect(withBackup.querySelector("img")?.getAttribute("src")).toBe("https://cdn.backup.invalid/backup.png");
    expect(resources.peekReadyResourceImage("resource:cast-backup", "original", resources.captureResourceAccessContext())).toBeUndefined();

    // 6. 本地 image: 和普通 src 展示正常但不写入 ready Map
    unmountActive();
    prepareCalls.length = 0;
    resolveImageUrlImpl = async () => "blob:http://localhost/local-img-1";
    const localContainer = mount(<CachedResourceImage storageKey="image:local-1" src="blob:http://localhost/old" alt="本地" />);
    await flushAsync(10);
    expect(prepareCalls.length).toBe(1);
    await act(async () => {
        prepareCalls[0].resolve();
    });
    expect(localContainer.querySelector("img")?.getAttribute("src")).toBe("blob:http://localhost/local-img-1");
    expect(resources.peekReadyResourceImage("image:local-1", "original", resources.captureResourceAccessContext())).toBeUndefined();
});

test("Task 6：首次 prepare 失败、ready img error、跨实例新 ready 复用、同 URL 强刷、二次失败终态（含 fallback=null）及 MediaThumb 三轮预算", async () => {
    const requestedPayloads: Array<{ resourceId: string; variant: "original" | "thumbnail" }> = [];
    const seqByKey = new Map<string, number>();
    let sameUrlMode = false;
    let failAllAccess = false;

    apiClient.defaults.adapter = async (config) => {
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        for (const item of items) requestedPayloads.push({ resourceId: item.resourceId, variant: item.variant });
        if (failAllAccess) {
            return {
                config,
                headers: {},
                status: 500,
                statusText: "Internal Server Error",
                data: { code: 500, msg: "刷新失败" },
            };
        }
        return {
            config,
            headers: {},
            status: 200,
            statusText: "OK",
            data: {
                code: 0,
                data: {
                    items: items.map((item) => {
                        const k = `${item.resourceId}:${item.variant}`;
                        const next = (seqByKey.get(k) ?? 0) + 1;
                        seqByKey.set(k, next);
                        const suffix = sameUrlMode ? "same" : `v${next}`;
                        return {
                            resourceId: item.resourceId,
                            access: imageAccess(item.resourceId, item.variant, {
                                url: `https://images.example.invalid/${item.resourceId}-${item.variant}-${suffix}.png`,
                                revision: `rev-${next}`,
                            }),
                        };
                    }),
                },
            },
        };
    };

    // 1. 首次 prepare 失败 -> 自动消费唯一刷新预算并 forceRefresh -> 新 URL prepare 成功 -> 显示图片且不触发 onError
    let errorCount = 0;
    const c1 = mount(
        <CachedResourceImage
            storageKey="resource:rec-1"
            variant="original"
            eager
            alt="恢复1"
            loadingFallback={<span data-testid="loading">加载中</span>}
            fallback={<span data-testid="fallback">失败</span>}
            onError={() => {
                errorCount += 1;
            }}
        />,
    );
    await flushAsync(25);
    expect(prepareCalls.length).toBe(1);
    expect(prepareCalls[0].src).toBe("https://images.example.invalid/rec-1-original-v1.png");

    // 首次 prepare 失败
    await act(async () => {
        prepareCalls[0].reject(new Error("首次解码失败"));
    });
    await flushAsync(25);
    // 自动触发一次 forceRefresh 并进入第二次 prepare
    expect(prepareCalls.length).toBe(2);
    expect(prepareCalls[1].src).toBe("https://images.example.invalid/rec-1-original-v2.png");
    await act(async () => {
        prepareCalls[1].resolve();
    });
    expect(c1.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/rec-1-original-v2.png");
    expect(errorCount).toBe(0);

    // 同一轮挂载已经消费过唯一恢复预算；若此时 DOM <img> 再次触发 error，不得无限强刷，必须移除失败 img 进入 fallback 并通知一次 onError
    act(() => {
        c1.querySelector("img")!.dispatchEvent(new dom.Event("error", { bubbles: true }));
    });
    await flushAsync(25);
    expect(c1.querySelector("img")).toBeNull();
    expect(c1.querySelector('[data-testid="fallback"]')).not.toBeNull();
    expect(prepareCalls.length).toBe(2);
    expect(errorCount).toBe(1);

    // 2. ready 命中直出 <img> 后触发 DOM error（且强刷返回相同时间窗 URL），恢复一次成功；若刷新后的 img 再次 error 且 fallback=null，也必须卸载失败 img
    unmountActive();
    prepareCalls.length = 0;
    requestedPayloads.length = 0;
    sameUrlMode = true;
    const ctx2 = resources.captureResourceAccessContext();
    const acc2 = await resources.getResourceAccess("resource:rec-same", "display", "thumbnail");
    resources.rememberReadyResourceImage("resource:rec-same", "thumbnail", acc2, acc2, ctx2);
    requestedPayloads.length = 0;

    let sameErrorCalls = 0;
    const c2 = mount(
        <CachedResourceImage
            storageKey="resource:rec-same"
            variant="thumbnail"
            eager
            alt="同URL恢复"
            loadingFallback={<span data-testid="loading">加载中</span>}
            fallback={null}
            onError={() => {
                sameErrorCalls += 1;
            }}
        />,
    );
    await flushAsync(10);
    // ready 命中首帧直出，无 prepare
    expect(c2.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/rec-same-thumbnail-same.png");
    expect(prepareCalls.length).toBe(0);

    // 触发第一次 DOM img error -> 进入 refreshing（显示 loadingFallback），即便新旧 URL 相同也正常完成一次强刷准备
    act(() => {
        c2.querySelector("img")!.dispatchEvent(new dom.Event("error", { bubbles: true }));
    });
    expect(c2.querySelector("img")).toBeNull();
    expect(c2.querySelector('[data-testid="loading"]')).not.toBeNull();
    expect(sameErrorCalls).toBe(0);

    await flushAsync(25);
    expect(requestedPayloads).toEqual([{ resourceId: "rec-same", variant: "thumbnail" }]);
    expect(prepareCalls.length).toBe(1);
    await act(async () => {
        prepareCalls[0].resolve();
    });
    expect(c2.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/rec-same-thumbnail-same.png");

    // 恢复后的 img 再次触发 DOM error -> 预算已耗尽，即使 fallback=null 也必须卸载失败 img 并调用一次 onError
    act(() => {
        c2.querySelector("img")!.dispatchEvent(new dom.Event("error", { bubbles: true }));
    });
    expect(c2.querySelector("img")).toBeNull();
    expect(sameErrorCalls).toBe(1);
    expect(prepareCalls.length).toBe(1);
    sameUrlMode = false;

    // 3. 跨实例新 ready 复用：实例已显示 v1 URL，另一实例已将 ready 更新为 v2 URL；此时当前实例 img error 消费唯一预算直接采用 v2，不发后端强刷
    unmountActive();
    prepareCalls.length = 0;
    const ctx3 = resources.captureResourceAccessContext();
    const acc3v1 = await resources.getResourceAccess("resource:rec-peer", "display", "thumbnail");
    resources.rememberReadyResourceImage("resource:rec-peer", "thumbnail", acc3v1, acc3v1, ctx3);

    const c3 = mount(
        <CachedResourceImage
            storageKey="resource:rec-peer"
            variant="thumbnail"
            eager
            alt="跨实例复用"
            fallback={<span data-testid="fallback">失败</span>}
        />,
    );
    await flushAsync(10);
    expect(c3.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/rec-peer-thumbnail-v1.png");

    // 模拟另一实例已经强刷成功并写入了 v2 ready
    const acc3v2 = await resources.getResourceAccess("resource:rec-peer", "display", "thumbnail", "", { forceRefresh: true });
    resources.rememberReadyResourceImage("resource:rec-peer", "thumbnail", acc3v2, acc3v2, ctx3);
    requestedPayloads.length = 0;
    prepareCalls.length = 0;

    // 当前实例旧 v1 img 触发 error -> 检测到有效且不同于失败 URL 的 v2 ready，直接采用 v2 并消费预算，不再发起强刷
    act(() => {
        c3.querySelector("img")!.dispatchEvent(new dom.Event("error", { bubbles: true }));
    });
    await flushAsync(10);
    expect(c3.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/rec-peer-thumbnail-v2.png");
    expect(requestedPayloads.length).toBe(0);

    // 若 v2 再次 error -> 预算已耗尽，直接进入终态 fallback，不得再次借跨实例换图绕过上限
    act(() => {
        c3.querySelector("img")!.dispatchEvent(new dom.Event("error", { bubbles: true }));
    });
    expect(c3.querySelector("img")).toBeNull();
    expect(c3.querySelector('[data-testid="fallback"]')).not.toBeNull();

    // 4. MediaThumb 外层 1500ms / 5000ms 重试预算：配合内部单次恢复，最多三轮挂载后稳定停在 failed，不无限计时
    unmountActive();
    prepareCalls.length = 0;
    requestedPayloads.length = 0;
    failAllAccess = true;

    const scheduledTimeouts: Array<{ delay: number; fn: () => void }> = [];
    const origSetTimeout = dom.setTimeout.bind(dom);
    const origWinSetTimeout = window.setTimeout.bind(window);
    window.setTimeout = ((fn: TimerHandler, delay?: number, ...args: unknown[]) => {
        if (delay === 1500 || delay === 5000) {
            scheduledTimeouts.push({ delay, fn: () => (fn as (...a: unknown[]) => void)(...args) });
            return scheduledTimeouts.length as unknown as ReturnType<typeof setTimeout>;
        }
        return origWinSetTimeout(fn, delay, ...args);
    }) as typeof window.setTimeout;

    try {
        const cm = mount(
            <MediaThumb
                resourceId="thumb-budget"
                alt="角色缩略图"
                loading={<span data-testid="thumb-loading">加载中</span>}
                failed={<span data-testid="thumb-failed">最终失败</span>}
            />,
        );
        // MediaThumb 默认非 eager，触发 IO（thumbnail+original 正常尝试 + 一次强刷轮次 = 4 个 16ms 批次窗口）
        await flushAsync(150);
        // 第 1 轮挂载失败后注册 1500ms 定时器
        expect(scheduledTimeouts.map((t) => t.delay)).toEqual([1500]);

        act(() => {
            scheduledTimeouts[0].fn();
        });
        await flushAsync(150);
        // 第 2 轮挂载失败后注册 5000ms 定时器
        expect(scheduledTimeouts.map((t) => t.delay)).toEqual([1500, 5000]);

        act(() => {
            scheduledTimeouts[1].fn();
        });
        await flushAsync(150);
        // 第 3 轮挂载失败后不再注册新定时器，稳定显示最终失败
        expect(scheduledTimeouts.map((t) => t.delay)).toEqual([1500, 5000]);
        expect(cm.querySelector('[data-testid="thumb-failed"]')).not.toBeNull();
    } finally {
        window.setTimeout = origWinSetTimeout;
        dom.setTimeout = origSetTimeout;
        failAllAccess = false;
    }
});

function setupDefaultVersioningAdapter(requestedPayloads?: Array<{ resourceId: string; variant: "original" | "thumbnail" }>) {
    const seqByKey = new Map<string, number>();
    apiClient.defaults.adapter = async (config) => {
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        for (const item of items) requestedPayloads?.push({ resourceId: item.resourceId, variant: item.variant });
        return {
            config,
            headers: {},
            status: 200,
            statusText: "OK",
            data: {
                code: 0,
                data: {
                    items: items.map((item) => {
                        const k = `${item.resourceId}:${item.variant}`;
                        const next = (seqByKey.get(k) ?? 0) + 1;
                        seqByKey.set(k, next);
                        return {
                            resourceId: item.resourceId,
                            access: imageAccess(item.resourceId, item.variant, {
                                url: `https://images.example.invalid/${item.resourceId}-${item.variant}-v${next}.png`,
                                revision: `rev-${next}`,
                            }),
                        };
                    }),
                },
            },
        };
    };
}

test("Task 7 [R1]：A 开始 prepare → clear/B → clear/A → 原 prepare resolve，不写 ready、不恢复旧图", async () => {
    setupDefaultVersioningAdapter();
    const container = mount(
        <CachedResourceImage
            storageKey="resource:r1"
            variant="thumbnail"
            eager
            alt="R1"
            loadingFallback={<span data-testid="loading">加载中</span>}
        />,
    );
    await flushAsync(25);
    expect(prepareCalls.length).toBe(1);
    const stalePrepare = prepareCalls[0];

    // 切到 user-b 再切回 user-a
    userScopeModule.setActiveUserScope("user-b");
    resources.clearResourceAccessCache();
    userScopeModule.setActiveUserScope("user-a");
    resources.clearResourceAccessCache();

    await act(async () => {
        stalePrepare.resolve();
    });
    expect(resources.peekReadyResourceImage("resource:r1", "thumbnail", resources.captureResourceAccessContext())).toBeUndefined();
    expect(container.querySelector("img")).toBeNull();
});

test("Task 7 [R2]：图 A 开始加载 → 同实例 props 换图 B → A resolve/error，B 图和 B ready 不变", async () => {
    setupDefaultVersioningAdapter();
    const container = mount(<CachedResourceImage storageKey="resource:r2-a" variant="thumbnail" eager alt="R2" />);
    await flushAsync(25);
    expect(prepareCalls.length).toBe(1);
    const prepareA = prepareCalls[0];

    // 同实例切换 storageKey 为 r2-b
    mount(<CachedResourceImage storageKey="resource:r2-b" variant="thumbnail" eager alt="R2" />);
    await flushAsync(25);
    expect(prepareCalls.length).toBe(2);
    const prepareB = prepareCalls[1];

    await act(async () => {
        prepareB.resolve();
    });
    expect(container.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/r2-b-thumbnail-v1.png");

    // 迟到的 A resolve 不影响 B，也不写入 r2-a ready
    await act(async () => {
        prepareA.resolve();
    });
    const ctx = resources.captureResourceAccessContext();
    expect(container.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/r2-b-thumbnail-v1.png");
    expect(resources.peekReadyResourceImage("resource:r2-b", "thumbnail", ctx)?.url).toBe("https://images.example.invalid/r2-b-thumbnail-v1.png");
    expect(resources.peekReadyResourceImage("resource:r2-a", "thumbnail", ctx)).toBeUndefined();
});

test("Task 7 [R3]：thumbnail 开始 → variant 换 original → 旧结果返回，original 不显示旧 thumbnail 地址", async () => {
    setupDefaultVersioningAdapter();
    const container = mount(<CachedResourceImage storageKey="resource:r3" variant="thumbnail" eager alt="R3" />);
    await flushAsync(25);
    expect(prepareCalls.length).toBe(1);
    const prepareThumb = prepareCalls[0];

    // 同实例切换 variant 为 original
    mount(<CachedResourceImage storageKey="resource:r3" variant="original" eager alt="R3" />);
    await flushAsync(25);
    expect(prepareCalls.length).toBe(2);
    const prepareOrig = prepareCalls[1];

    await act(async () => {
        prepareThumb.resolve();
    });
    // original 仍在等待 prepareOrig，不能显示旧 thumbnail 地址
    expect(container.querySelector("img")).toBeNull();

    await act(async () => {
        prepareOrig.resolve();
    });
    expect(container.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/r3-original-v1.png");
});

test("Task 7 [R4]：准备/刷新中 unmount → promise resolve/reject，无后续状态、ready 或外部事件发布", async () => {
    setupDefaultVersioningAdapter();
    let errorCalled = 0;
    mount(
        <CachedResourceImage
            storageKey="resource:r4"
            variant="thumbnail"
            eager
            alt="R4"
            onError={() => {
                errorCalled += 1;
            }}
        />,
    );
    await flushAsync(25);
    expect(prepareCalls.length).toBe(1);
    const inFlight = prepareCalls[0];

    unmountActive();
    await act(async () => {
        inFlight.resolve();
    });
    expect(resources.peekReadyResourceImage("resource:r4", "thumbnail", resources.captureResourceAccessContext())).toBeUndefined();
    expect(errorCalled).toBe(0);
});

test("Task 7 [R5 & R6]：两实例同时 error 共享单次在途强刷；恢复出新 URL 后迟到旧 error 不删新 ready、不发额外强刷", async () => {
    const requestedPayloads: Array<{ resourceId: string; variant: "original" | "thumbnail" }> = [];
    setupDefaultVersioningAdapter(requestedPayloads);

    const ctx = resources.captureResourceAccessContext();
    const initialAcc = await resources.getResourceAccess("resource:r56", "display", "thumbnail");
    resources.rememberReadyResourceImage("resource:r56", "thumbnail", initialAcc, initialAcc, ctx);
    requestedPayloads.length = 0;

    const container = mount(
        <div>
            <CachedResourceImage data-inst="1" storageKey="resource:r56" variant="thumbnail" eager alt="实例1" />
            <CachedResourceImage data-inst="2" storageKey="resource:r56" variant="thumbnail" eager alt="实例2" />
        </div>,
    );
    await flushAsync(10);
    const img1 = container.querySelector('img[data-inst="1"]')!;
    const img2 = container.querySelector('img[data-inst="2"]')!;
    expect(img1.getAttribute("src")).toBe("https://images.example.invalid/r56-thumbnail-v1.png");
    expect(img2.getAttribute("src")).toBe("https://images.example.invalid/r56-thumbnail-v1.png");

    // R6：两个实例同时触发 error -> 共享同一次在途 forceRefresh POST
    act(() => {
        img1.dispatchEvent(new dom.Event("error", { bubbles: true }));
        img2.dispatchEvent(new dom.Event("error", { bubbles: true }));
    });
    await flushAsync(25);
    expect(requestedPayloads).toEqual([{ resourceId: "r56", variant: "thumbnail" }]);
    expect(prepareCalls.length).toBe(2);

    await act(async () => {
        prepareCalls[0].resolve();
        prepareCalls[1].resolve();
    });
    const newImg1 = container.querySelector('img[data-inst="1"]')!;
    const newImg2 = container.querySelector('img[data-inst="2"]')!;
    expect(newImg1.getAttribute("src")).toBe("https://images.example.invalid/r56-thumbnail-v2.png");
    expect(newImg2.getAttribute("src")).toBe("https://images.example.invalid/r56-thumbnail-v2.png");
    expect(resources.peekReadyResourceImage("resource:r56", "thumbnail", ctx)?.url).toBe("https://images.example.invalid/r56-thumbnail-v2.png");

    // 旧 URL 的 compare-delete 不能删除新值；实际组件事件路径另有跨实例用例覆盖。
    requestedPayloads.length = 0;
    resources.forgetReadyResourceImage("resource:r56", "thumbnail", "https://images.example.invalid/r56-thumbnail-v1.png", ctx);
    expect(resources.peekReadyResourceImage("resource:r56", "thumbnail", ctx)?.url).toBe("https://images.example.invalid/r56-thumbnail-v2.png");
    expect(requestedPayloads.length).toBe(0);
});

test("Task 7 [R7]：React StrictMode 下 setup/cleanup/setup 不重复消费恢复预算，也不将有效图片标为失败", async () => {
    setupDefaultVersioningAdapter();
    const container = mount(
        <React.StrictMode>
            <CachedResourceImage
                storageKey="resource:r7"
                variant="thumbnail"
                eager
                alt="R7"
                fallback={<span data-testid="fallback">失败</span>}
            />
        </React.StrictMode>,
    );
    await flushAsync(25);
    const activeCall = prepareCalls[prepareCalls.length - 1];
    expect(activeCall).toBeDefined();

    // 首次 thumbnail prepare 失败 -> 回退到 original；original 再失败 -> 预算未被 StrictMode cleanup 消耗，正常触发一次强刷恢复
    await act(async () => {
        activeCall.reject(new Error("首次缩略图失败"));
    });
    await flushAsync(25);
    const fallbackOrigCall = prepareCalls[prepareCalls.length - 1];
    expect(fallbackOrigCall.src).toBe("https://images.example.invalid/r7-original-v1.png");

    await act(async () => {
        fallbackOrigCall.reject(new Error("首次原图回退失败"));
    });
    await flushAsync(25);
    const refreshCall = prepareCalls[prepareCalls.length - 1];
    expect(refreshCall.src).toBe("https://images.example.invalid/r7-thumbnail-v2.png");
    await act(async () => {
        refreshCall.resolve();
    });
    expect(container.querySelector("img")?.getAttribute("src")).toBe("https://images.example.invalid/r7-thumbnail-v2.png");
    expect(container.querySelector('[data-testid="fallback"]')).toBeNull();
});

test("Task 7 [R8a & R8b]：original 回退完成前若相关强刷已开始或新 thumbnail 描述已生效，旧结果不能回填覆盖 ready", async () => {
    let thumbFail = true;
    let seq = 0;
    apiClient.defaults.adapter = async (config) => {
        seq += 1;
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        return {
            config,
            headers: {},
            status: 200,
            statusText: "OK",
            data: {
                code: 0,
                data: {
                    items: items.map((item) => {
                        if (item.variant === "thumbnail" && thumbFail) {
                            return { resourceId: item.resourceId, error: { msg: "缩略图暂不可用" } };
                        }
                        return {
                            resourceId: item.resourceId,
                            access: imageAccess(item.resourceId, item.variant, {
                                url: `https://images.example.invalid/${item.resourceId}-${item.variant}-v${seq}.png`,
                                revision: `rev-${seq}`,
                            }),
                        };
                    }),
                },
            },
        };
    };

    const ctx = resources.captureResourceAccessContext();

    // R8a：thumbnail 失败回退到 original，在 original prepare 完成前，original 强刷已发起且未决 -> remember 被拦截
    await expect(resources.getResourceAccess("resource:r8a", "display", "thumbnail")).rejects.toThrow();
    const origAcc8a = await resources.getResourceAccess("resource:r8a", "display", "original");
    const pendingRefresh8a = resources.getResourceAccess("resource:r8a", "display", "original", "", { forceRefresh: true });
    resources.rememberReadyResourceImage("resource:r8a", "thumbnail", origAcc8a, undefined, ctx);
    expect(resources.peekReadyResourceImage("resource:r8a", "thumbnail", ctx)).toBeUndefined();
    await pendingRefresh8a;

    // R8b：thumbnail 初始失败（requestedAccess=undefined），回退 original；但在旧 original prepare 完成前，thumbnail 强刷已成功并写入新 ready -> 旧 original 不能覆盖新 ready
    await expect(resources.getResourceAccess("resource:r8b", "display", "thumbnail")).rejects.toThrow();
    const origAcc8b = await resources.getResourceAccess("resource:r8b", "display", "original");
    thumbFail = false;
    const newThumbAcc8b = await resources.getResourceAccess("resource:r8b", "display", "thumbnail", "", { forceRefresh: true });
    resources.rememberReadyResourceImage("resource:r8b", "thumbnail", newThumbAcc8b, newThumbAcc8b, ctx);
    expect(resources.peekReadyResourceImage("resource:r8b", "thumbnail", ctx)?.url).toBe(newThumbAcc8b.url);

    // 此时迟到的旧 original 尝试以 requestedAccess=undefined 写入 ready，因 currentRequested !== undefined 被拒绝
    resources.rememberReadyResourceImage("resource:r8b", "thumbnail", origAcc8b, undefined, ctx);
    expect(resources.peekReadyResourceImage("resource:r8b", "thumbnail", ctx)?.url).toBe(newThumbAcc8b.url);
    expect(resources.peekReadyResourceImage("resource:r8b", "thumbnail", ctx)?.sourceVariant).toBe("thumbnail");
});

test("取消缩略图准备并切账号后，不再以新账号请求原图", async () => {
    const requests: Array<{ resourceId: string; variant: "original" | "thumbnail" }> = [];
    setupDefaultVersioningAdapter(requests);
    const started = deferred<void>();
    prepareImpl = async (_url, signal) => {
        started.resolve();
        return new Promise<HTMLImageElement>((_resolve, reject) => {
            signal.addEventListener("abort", () => reject(new DOMException("取消", "AbortError")), { once: true });
        });
    };
    mount(<CachedResourceImage storageKey="resource:cancelled-thumb" variant="thumbnail" eager alt="取消" />);
    await act(async () => { await started.promise; });
    expect(requests).toEqual([{ resourceId: "cancelled-thumb", variant: "thumbnail" }]);
    unmountActive();
    resources.clearResourceAccessCache();
    userScopeModule.setActiveUserScope("user-b");
    await flushAsync(50);
    expect(requests).toEqual([{ resourceId: "cancelled-thumb", variant: "thumbnail" }]);
});

test("访问描述未决时卸载，迟到的缩略图失败不再请求原图", async () => {
    const started = deferred<void>();
    const response = deferred<void>();
    const requests: string[] = [];
    apiClient.defaults.adapter = async (config) => {
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: string }>;
        requests.push(...items.map((item) => item.variant));
        started.resolve();
        await response.promise;
        return { config, headers: {}, status: 200, statusText: "OK", data: { code: 0, data: {
            items: items.map((item) => ({ resourceId: item.resourceId, error: { msg: "缩略图不可用" } })),
        } } };
    };
    mount(<CachedResourceImage storageKey="resource:cancelled-access" variant="thumbnail" eager alt="取消" />);
    await act(async () => { await started.promise; });
    unmountActive();
    response.resolve();
    await flushAsync(50);
    expect(requests).toEqual(["thumbnail"]);
    expect(prepareCalls).toHaveLength(0);
});

test("缩略图获取和原图解码均失败时，每个恢复轮次仅请求原图一次", async () => {
    const variants: string[] = [];
    const attempted = deferred<void>();
    let prepareCount = 0;
    apiClient.defaults.adapter = async (config) => {
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        return { config, headers: {}, status: 200, statusText: "OK", data: { code: 0, data: {
            items: items.map((item) => {
                variants.push(item.variant);
                return item.variant === "thumbnail"
                    ? { resourceId: item.resourceId, error: { msg: "缩略图不可用" } }
                    : { resourceId: item.resourceId, access: imageAccess(item.resourceId, "original") };
            }),
        } } };
    };
    prepareImpl = async () => {
        if (++prepareCount === 2) attempted.resolve();
        throw new Error("原图解码失败");
    };
    const container = mount(<CachedResourceImage storageKey="resource:bounded-original" variant="thumbnail" eager alt="失败" fallback={<span data-testid="terminal" />} />);
    await act(async () => { await attempted.promise; });
    await flushAsync(50);
    expect(variants).toEqual(["thumbnail", "original", "thumbnail", "original"]);
    expect(container.querySelector("[data-testid=terminal]")).not.toBeNull();
});

for (const local of [false, true]) {
    test(`${local ? "本地" : "远程"}图片恢复期间重复 error 不提前通知终态失败`, async () => {
        setupDefaultVersioningAdapter();
        const key = local ? "image:duplicate-error" : "resource:duplicate-error";
        const oldURL = local ? "blob:http://localhost/old" : "https://images.example.invalid/duplicate-error-thumbnail-v1.png";
        if (local) {
            resolveImageUrlImpl = async () => oldURL;
            prepareImpl = async () => dom.document.createElement("img") as unknown as HTMLImageElement;
        } else {
            const access = await resources.getResourceAccess(key, "display", "thumbnail");
            resources.rememberReadyResourceImage(key, "thumbnail", access, access, resources.captureResourceAccessContext());
        }
        let errors = 0;
        const container = mount(<CachedResourceImage storageKey={key} variant="thumbnail" eager alt="恢复" onError={() => { errors++; }} />);
        await flushAsync(0);
        const recovery = deferred<HTMLImageElement>();
        const started = deferred<void>();
        prepareImpl = async () => { started.resolve(); return recovery.promise; };
        resolveImageUrlImpl = async () => "blob:http://localhost/new";
        const img = container.querySelector("img")!;
        expect(img.getAttribute("src")).toBe(oldURL);
        act(() => {
            img.dispatchEvent(new dom.Event("error", { bubbles: true }));
            img.dispatchEvent(new dom.Event("error", { bubbles: true }));
        });
        expect(errors).toBe(0);
        await act(async () => { await started.promise; });
        await act(async () => { recovery.resolve(dom.document.createElement("img") as unknown as HTMLImageElement); });
        const restored = container.querySelector("img")!;
        expect(restored.getAttribute("src")).not.toBe(oldURL);
        expect(errors).toBe(0);
        act(() => {
            restored.dispatchEvent(new dom.Event("error", { bubbles: true }));
            restored.dispatchEvent(new dom.Event("error", { bubbles: true }));
        });
        expect(errors).toBe(1);
        expect(container.querySelector("img")).toBeNull();
    });
}

test("两个真实组件先后 error 时，后者复用已恢复地址且迟到重复事件不误报失败", async () => {
    const requests: Array<{ resourceId: string; variant: "original" | "thumbnail" }> = [];
    setupDefaultVersioningAdapter(requests);
    const key = "resource:peer-events";
    const context = resources.captureResourceAccessContext();
    const initial = await resources.getResourceAccess(key, "display", "thumbnail");
    resources.rememberReadyResourceImage(key, "thumbnail", initial, initial, context);
    let errors = 0;
    let loads = 0;
    const container = mount(<>
        <CachedResourceImage data-peer="first" storageKey={key} variant="thumbnail" eager alt="先恢复" />
        <CachedResourceImage data-peer="second" storageKey={key} variant="thumbnail" eager alt="后恢复" onError={() => { errors++; }} onLoad={() => { loads++; }} />
    </>);
    const oldSecond = container.querySelector("[data-peer=second]")!;
    const started = deferred<void>();
    prepareImpl = async () => {
        started.resolve();
        return dom.document.createElement("img") as unknown as HTMLImageElement;
    };
    act(() => { container.querySelector("[data-peer=first]")!.dispatchEvent(new dom.Event("error", { bubbles: true })); });
    await act(async () => { await started.promise; });
    const fresh = resources.peekReadyResourceImage(key, "thumbnail", context)!;
    expect(fresh.url).not.toBe(initial.url);
    expect(oldSecond.getAttribute("src")).toBe(initial.url);
    requests.length = 0;
    act(() => {
        oldSecond.dispatchEvent(new dom.Event("error", { bubbles: true }));
        oldSecond.dispatchEvent(new dom.Event("error", { bubbles: true }));
    });
    await flushAsync(25);
    const second = container.querySelector("[data-peer=second]")!;
    expect(second.getAttribute("src")).toBe(fresh.url);
    expect(resources.peekReadyResourceImage(key, "thumbnail", context)?.url).toBe(fresh.url);
    expect(requests).toHaveLength(0);
    expect(errors).toBe(0);
    act(() => { second.dispatchEvent(new dom.Event("load", { bubbles: true })); });
    expect(loads).toBe(1);
});

test("真实组件的旧 original 准备完成，不能覆盖新 thumbnail 的 ready 描述", async () => {
    let thumbnailFailed = true;
    apiClient.defaults.adapter = async (config) => {
        const items = JSON.parse(String(config.data || "[]")) as Array<{ resourceId: string; variant: "original" | "thumbnail" }>;
        return { config, headers: {}, status: 200, statusText: "OK", data: { code: 0, data: {
            items: items.map((item) => item.variant === "thumbnail" && thumbnailFailed
                ? { resourceId: item.resourceId, error: { msg: "缩略图不可用" } }
                : { resourceId: item.resourceId, access: imageAccess(item.resourceId, item.variant) }),
        } } };
    };
    const prepared = deferred<HTMLImageElement>();
    const started = deferred<void>();
    prepareImpl = async (url) => {
        expect(url).toContain("-original.png");
        started.resolve();
        return prepared.promise;
    };
    const key = "resource:late-original-component";
    mount(<CachedResourceImage storageKey={key} variant="thumbnail" eager alt="原图回退" />);
    await act(async () => { await started.promise; });
    thumbnailFailed = false;
    const context = resources.captureResourceAccessContext();
    const fresh = await resources.getResourceAccess(key, "display", "thumbnail", "", { forceRefresh: true });
    resources.rememberReadyResourceImage(key, "thumbnail", fresh, fresh, context);
    await act(async () => { prepared.resolve(dom.document.createElement("img") as unknown as HTMLImageElement); });
    expect(resources.peekReadyResourceImage(key, "thumbnail", context)?.url).toBe(fresh.url);
    expect(resources.peekReadyResourceImage(key, "thumbnail", context)?.sourceVariant).toBe("thumbnail");
});

