import { useEffect, useRef, useState, type ImgHTMLAttributes, type ReactNode } from "react";

import {
    captureResourceAccessContext,
    forgetReadyResourceImage,
    getResourceAccess,
    isResourceAccessContextCurrent,
    peekReadyResourceImage,
    rememberReadyResourceImage,
    resolveResourceAccessURL,
    resourceIdFromStorageKey,
    touchReadyResourceImage,
    type ResourceAccess,
    type ResourceAccessContext,
    type ResourceAccessVariant,
} from "@/services/api/resources";
import { resolveImageUrl } from "@/services/image-storage";
import { prepareCanvasImage } from "@/services/canvas-image-loader";

type CachedResourceImageProps = Omit<ImgHTMLAttributes<HTMLImageElement>, "src"> & {
    storageKey?: string;
    src?: string;
    fallback?: ReactNode;
    loadingFallback?: ReactNode;
    eager?: boolean;
    variant?: ResourceAccessVariant;
};

/**
 * 远程资源图片统一使用 OSS/CDN 授权地址。
 * Blob 缓存仍可用于导出、抽帧等字节处理，但不作为媒体展示 src，避免把
 * `blob:http(s)://...` 泄露到节点、素材库和浏览器媒体链路中。
 */
export function CachedResourceImage({
    storageKey,
    src = "",
    fallback = null,
    loadingFallback = fallback,
    eager = false,
    variant = "original",
    loading,
    onError,
    ...props
}: CachedResourceImageProps) {
    const resourceId = resourceIdFromStorageKey(storageKey);
    const remoteResource = Boolean(resourceId);
    const localImageResource = Boolean(storageKey && storageKey.startsWith("image:"));
    const context = captureResourceAccessContext();
    const { scope, generation } = context;
    const identity = JSON.stringify([scope, generation, resourceId || storageKey || src, variant]);
    const targetRef = useRef<HTMLSpanElement>(null);
    const [viewportIdentity, setViewportIdentity] = useState(eager || !remoteResource ? identity : "");
    const nearViewport = eager || !remoteResource || viewportIdentity === identity;
    const [displayed, setDisplayed] = useState<{ identity: string; src: string } | null>(null);
    const displayedRef = useRef(displayed);
    displayedRef.current = displayed;
    const [refreshingIdentity, setRefreshingIdentity] = useState("");
    const refreshingIdentityRef = useRef(refreshingIdentity);
    refreshingIdentityRef.current = refreshingIdentity;
    const [failedIdentity, setFailedIdentity] = useState("");
    const failedIdentityRef = useRef(failedIdentity);
    failedIdentityRef.current = failedIdentity;
    const identityRef = useRef(identity);
    identityRef.current = identity;
    const onErrorRef = useRef(onError);
    onErrorRef.current = onError;
    const budgetStateRef = useRef<{ identity: string; used: boolean }>({ identity, used: false });
    if (budgetStateRef.current.identity !== identity) {
        budgetStateRef.current = { identity, used: false };
    }
    const recoveryControllerRef = useRef<AbortController | null>(null);

    const ready = remoteResource && storageKey ? peekReadyResourceImage(storageKey, variant, context) : undefined;
    const failed = failedIdentity === identity;
    const refreshing = refreshingIdentity === identity;
    const currentDisplayedSrc = displayed?.identity === identity ? displayed.src : "";
    const cachedSrc = failed || refreshing ? "" : currentDisplayedSrc || ready?.url || "";
    const remoteLoading = loading ?? (eager ? "eager" : "lazy");

    useEffect(() => {
        return () => {
            recoveryControllerRef.current?.abort();
            recoveryControllerRef.current = null;
        };
    }, [identity]);

    useEffect(() => {
        if (!remoteResource || eager || Boolean(ready)) {
            setViewportIdentity(identity);
            return;
        }
        const image = targetRef.current;
        if (!image || typeof IntersectionObserver === "undefined") {
            setViewportIdentity(identity);
            return;
        }
        const observer = new IntersectionObserver(
            (entries) => {
                if (entries.some((entry) => entry.isIntersecting)) {
                    setViewportIdentity(identity);
                    observer.disconnect();
                }
            },
            { rootMargin: "240px" },
        );
        observer.observe(image);
        return () => observer.disconnect();
    }, [eager, identity, Boolean(ready), remoteResource]);

    const runRemoteFetchAndPrepare = async (
        key: string,
        forceRefresh: boolean,
        signal: AbortSignal,
        onCandidate?: (url: string) => void,
    ): Promise<{ candidate: string; activeAccess?: ResourceAccess; requestedAccess?: ResourceAccess }> => {
        let candidate = src;
        let requestedAccess: ResourceAccess | undefined;
        let activeAccess: ResourceAccess | undefined;
        const options = forceRefresh ? { forceRefresh: true } : {};
        const assertCurrent = () => {
            if (signal.aborted || identityRef.current !== identity || !isResourceAccessContextCurrent({ scope, generation })) {
                throw new DOMException("图片加载已取消", "AbortError");
            }
        };
        const fetchAccess = async (requestedVariant: ResourceAccessVariant) => {
            assertCurrent();
            const access = await getResourceAccess(key, "display", requestedVariant, "", options);
            assertCurrent();
            return access;
        };
        try {
            const access = await fetchAccess(variant);
            requestedAccess = access;
            activeAccess = access;
            candidate = resolveResourceAccessURL(access.url);
        } catch (error) {
            assertCurrent();
            if (variant !== "thumbnail" || (error instanceof Error && error.name === "AbortError")) throw error;
            const access = await fetchAccess("original");
            activeAccess = access;
            candidate = resolveResourceAccessURL(access.url);
        }
        if (!candidate) throw new Error("图片地址为空");
        onCandidate?.(candidate);
        try {
            await prepareCanvasImage(candidate, signal);
        } catch (error) {
            assertCurrent();
            if (error instanceof Error && error.name === "AbortError") throw error;
            if (variant !== "thumbnail") {
                if (!forceRefresh && candidate !== src && src) {
                    onCandidate?.(src);
                    await prepareCanvasImage(src, signal);
                    candidate = src;
                    activeAccess = undefined;
                } else {
                    throw error;
                }
            } else {
                if (activeAccess?.requestedVariant === "original") throw error;
                const access = await fetchAccess("original");
                const originalURL = resolveResourceAccessURL(access.url);
                if (!originalURL || (!forceRefresh && originalURL === candidate)) throw error;
                onCandidate?.(originalURL);
                await prepareCanvasImage(originalURL, signal);
                candidate = originalURL;
                activeAccess = access;
            }
        }
        assertCurrent();
        return { candidate, activeAccess, requestedAccess };
    };

    useEffect(() => {
        if (failedIdentityRef.current === identity || refreshingIdentityRef.current === identity) return;
        const effectContext: ResourceAccessContext = { scope, generation };
        if (remoteResource && storageKey) {
            const hit = peekReadyResourceImage(storageKey, variant, effectContext);
            if (hit) {
                touchReadyResourceImage(storageKey, variant, hit.url, effectContext);
                if (displayedRef.current?.identity !== identity || displayedRef.current.src !== hit.url) {
                    setDisplayed({ identity, src: hit.url });
                }
                setFailedIdentity((current) => (current === identity ? "" : current));
                return;
            }
        }
        if (remoteResource && !nearViewport) return;
        if (displayedRef.current?.identity === identity && displayedRef.current.src) return;

        const controller = new AbortController();
        let cancelled = false;
        const isCurrent = () =>
            !cancelled &&
            !controller.signal.aborted &&
            identityRef.current === identity &&
            isResourceAccessContextCurrent(effectContext);

        const commitSuccess = (candidate: string, activeAccess?: ResourceAccess, requestedAccess?: ResourceAccess) => {
            if (remoteResource && storageKey && activeAccess && candidate === resolveResourceAccessURL(activeAccess.url)) {
                rememberReadyResourceImage(storageKey, variant, activeAccess, requestedAccess, effectContext);
            }
            setDisplayed({ identity, src: candidate });
            refreshingIdentityRef.current = "";
            setRefreshingIdentity((current) => (current === identity ? "" : current));
            setFailedIdentity((current) => (current === identity ? "" : current));
        };

        const load = async () => {
            if (remoteResource && resourceId && storageKey) {
                let firstAttemptFailedUrl = "";
                try {
                    const res = await runRemoteFetchAndPrepare(storageKey, false, controller.signal, (url) => {
                        firstAttemptFailedUrl = url;
                    });
                    if (isCurrent()) commitSuccess(res.candidate, res.activeAccess, res.requestedAccess);
                    return;
                } catch (firstError) {
                    if (!isCurrent()) return;
                    if (budgetStateRef.current.identity === identity && budgetStateRef.current.used) {
                        throw firstError;
                    }
                    const peerHit = peekReadyResourceImage(storageKey, variant, effectContext);
                    budgetStateRef.current = { identity, used: true };
                    if (peerHit && peerHit.url !== firstAttemptFailedUrl) {
                        touchReadyResourceImage(storageKey, variant, peerHit.url, effectContext);
                        setDisplayed({ identity, src: peerHit.url });
                        setFailedIdentity((current) => (current === identity ? "" : current));
                        return;
                    }
                    refreshingIdentityRef.current = identity;
                    setRefreshingIdentity(identity);
                    const refreshed = await runRemoteFetchAndPrepare(storageKey, true, controller.signal);
                    if (isCurrent()) commitSuccess(refreshed.candidate, refreshed.activeAccess, refreshed.requestedAccess);
                    return;
                }
            }

            let candidate = src;
            if (localImageResource && storageKey) {
                candidate = (await resolveImageUrl(storageKey, src)) || src;
            }
            if (!isCurrent()) return;
            if (!candidate) throw new Error("图片地址为空");
            try {
                await prepareCanvasImage(candidate, controller.signal);
            } catch (error) {
                if (!isCurrent()) return;
                if (candidate !== src && src) {
                    await prepareCanvasImage(src, controller.signal);
                    candidate = src;
                } else {
                    throw error;
                }
            }
            if (isCurrent()) commitSuccess(candidate);
        };

        void load().catch(() => {
            if (isCurrent()) {
                refreshingIdentityRef.current = "";
                setRefreshingIdentity((current) => (current === identity ? "" : current));
                if (displayedRef.current?.identity !== identity) setFailedIdentity(identity);
            }
        });
        return () => {
            cancelled = true;
            controller.abort();
        };
    }, [generation, identity, localImageResource, nearViewport, remoteResource, resourceId, scope, src, storageKey, variant]);

    const handleImgError = (e: React.SyntheticEvent<HTMLImageElement, Event>) => {
        if (refreshingIdentityRef.current === identity || failedIdentityRef.current === identity) return;
        const failedUrl = e.currentTarget?.getAttribute("src") || cachedSrc;
        const expectedSrc = displayedRef.current?.identity === identity ? displayedRef.current.src : cachedSrc;
        if (!failedUrl || (expectedSrc && failedUrl !== expectedSrc)) return;
        const currentContext: ResourceAccessContext = { scope, generation };
        if (!isResourceAccessContextCurrent(currentContext) || identityRef.current !== identity) return;

        if (localImageResource && storageKey && failedUrl.startsWith("blob:")) {
            if (budgetStateRef.current.identity === identity && budgetStateRef.current.used) {
                setDisplayed((current) => (current?.identity === identity ? null : current));
                failedIdentityRef.current = identity;
                setFailedIdentity(identity);
                onErrorRef.current?.(e);
                return;
            }
            budgetStateRef.current = { identity, used: true };
            setDisplayed((current) => (current?.identity === identity ? null : current));
            refreshingIdentityRef.current = identity;
            setRefreshingIdentity(identity);
            recoveryControllerRef.current?.abort();
            const controller = new AbortController();
            recoveryControllerRef.current = controller;
            const isCurrent = () =>
                !controller.signal.aborted &&
                identityRef.current === identity &&
                isResourceAccessContextCurrent(currentContext);
            void resolveImageUrl(storageKey)
                .then(async (url) => {
                    if (!isCurrent()) return;
                    if (!url || url === failedUrl) throw new Error("本地图片恢复地址无效");
                    await prepareCanvasImage(url, controller.signal);
                    if (!isCurrent()) return;
                    setDisplayed({ identity, src: url });
                    refreshingIdentityRef.current = "";
                    setRefreshingIdentity((current) => (current === identity ? "" : current));
                    setFailedIdentity((current) => (current === identity ? "" : current));
                })
                .catch(() => {
                    if (!isCurrent()) return;
                    refreshingIdentityRef.current = "";
                    failedIdentityRef.current = identity;
                    setRefreshingIdentity((current) => (current === identity ? "" : current));
                    setFailedIdentity(identity);
                    onErrorRef.current?.(e);
                });
            return;
        }

        if (remoteResource && storageKey) {
            if (failedUrl) {
                forgetReadyResourceImage(storageKey, variant, failedUrl, currentContext);
            }
            setDisplayed((current) => (current?.identity === identity ? null : current));
            if (budgetStateRef.current.identity === identity && budgetStateRef.current.used) {
                setRefreshingIdentity((current) => (current === identity ? "" : current));
                failedIdentityRef.current = identity;
                setFailedIdentity(identity);
                onErrorRef.current?.(e);
                return;
            }
            budgetStateRef.current = { identity, used: true };
            const peerHit = peekReadyResourceImage(storageKey, variant, currentContext);
            if (peerHit && peerHit.url !== failedUrl) {
                touchReadyResourceImage(storageKey, variant, peerHit.url, currentContext);
                displayedRef.current = { identity, src: peerHit.url };
                setDisplayed({ identity, src: peerHit.url });
                setFailedIdentity((current) => (current === identity ? "" : current));
                return;
            }
            refreshingIdentityRef.current = identity;
            setRefreshingIdentity(identity);
            recoveryControllerRef.current?.abort();
            const controller = new AbortController();
            recoveryControllerRef.current = controller;
            const isCurrent = () =>
                !controller.signal.aborted &&
                identityRef.current === identity &&
                isResourceAccessContextCurrent(currentContext);
            void runRemoteFetchAndPrepare(storageKey, true, controller.signal)
                .then((res) => {
                    if (!isCurrent()) return;
                    if (res.activeAccess && res.candidate === resolveResourceAccessURL(res.activeAccess.url)) {
                        rememberReadyResourceImage(storageKey, variant, res.activeAccess, res.requestedAccess, currentContext);
                    }
                    setDisplayed({ identity, src: res.candidate });
                    refreshingIdentityRef.current = "";
                    setRefreshingIdentity((current) => (current === identity ? "" : current));
                    setFailedIdentity((current) => (current === identity ? "" : current));
                })
                .catch(() => {
                    if (!isCurrent()) return;
                    refreshingIdentityRef.current = "";
                    failedIdentityRef.current = identity;
                    setRefreshingIdentity((current) => (current === identity ? "" : current));
                    setFailedIdentity(identity);
                    onErrorRef.current?.(e);
                });
            return;
        }

        setDisplayed((current) => (current?.identity === identity ? null : current));
        failedIdentityRef.current = identity;
        setFailedIdentity(identity);
        onErrorRef.current?.(e);
    };

    if (!remoteResource) {
        if (failed) return <>{fallback}</>;
        return <img {...props} loading={loading} src={cachedSrc || undefined} onError={handleImgError} />;
    }
    return (
        <span ref={targetRef} className="cached-resource-image-shell">
            {cachedSrc ? <img {...props} loading={remoteLoading} src={cachedSrc} onError={handleImgError} /> : failed ? fallback : loadingFallback}
        </span>
    );
}
