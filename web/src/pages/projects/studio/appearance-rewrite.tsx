import { useState } from "react";
import { useMutation, useQuery } from "@tanstack/react-query";
import { App, Button, Checkbox } from "antd";
import { Wand2 } from "lucide-react";
import { AppModal } from "@/components/ui/product/app-modal";
import { getDramaAppearanceProposals, startDramaAppearanceRewrite, updateDramaCast } from "@/services/api/drama";

/** 重写角色外貌：模型按面部写法给出建议，用户逐个确认后才写入设定；不会自动重新生成参考图。传 cast 时只重写这一个角色。 */
export function AppearanceRewriteButton({ projectId, cast, className, onChanged }: { projectId: string; cast?: { id: string; name: string }; className?: string; onChanged: () => void }) {
    const { message } = App.useApp();
    const [session, setSession] = useState<{ taskId: string; skillName: string } | null>(null);
    const start = useMutation({
        mutationFn: () => startDramaAppearanceRewrite(projectId, cast?.id),
        onSuccess: setSession,
        onError: (error) => message.error(error instanceof Error ? error.message : "启动失败"),
    });
    return (
        <>
            <Button className={className} icon={<Wand2 className="size-4" />} loading={start.isPending} onClick={() => start.mutate()}>
                {cast ? "重写此角色外貌" : "重写角色外貌"}
            </Button>
            <AppModal open={Boolean(session)} title={cast ? `重写「${cast.name}」的外貌` : "重写角色外貌"} footer={null} width={860} centered onCancel={() => setSession(null)}>
                {session ? <Proposals projectId={projectId} {...session} onClose={() => setSession(null)} onChanged={onChanged} /> : null}
            </AppModal>
        </>
    );
}

function Proposals({ projectId, taskId, skillName, onClose, onChanged }: { projectId: string; taskId: string; skillName: string; onClose: () => void; onChanged: () => void }) {
    const { message } = App.useApp();
    const [unchecked, setUnchecked] = useState<Set<string>>(new Set());
    const query = useQuery({
        queryKey: ["drama", projectId, "appearance-rewrite", taskId],
        queryFn: ({ signal }) => getDramaAppearanceProposals(projectId, taskId, signal),
        refetchInterval: (state) => (state.state.data?.status === "running" || !state.state.data ? 2000 : false),
    });
    const data = query.data;
    const selected = (data?.proposals ?? []).filter((item) => !unchecked.has(item.castId));
    const apply = useMutation({
        mutationFn: async () => {
            const failed: string[] = [];
            for (const item of selected) {
                try {
                    await updateDramaCast(projectId, item.castId, { appearance: item.after });
                } catch {
                    failed.push(item.name);
                }
            }
            return failed;
        },
        onSuccess: (failed) => {
            onChanged();
            if (failed.length) {
                message.error(`这些角色没有写入：${failed.join("、")}`);
                return;
            }
            message.success(`已更新 ${selected.length} 个角色的外貌；需要新脸请到角色详情里重新生成参考图`);
            onClose();
        },
    });
    return (
        <div className="flex max-h-[70vh] flex-col gap-3">
            <p className="m-0 text-xs opacity-60">
                按「{skillName || "内置规则"}」改写，已锁定的角色会被跳过；群体角色按一名代表成员写脸。应用后不会自动重新生成参考图。
            </p>
            {query.isError ? <p className="m-0 text-sm text-[var(--status-error)]">{query.error instanceof Error ? query.error.message : "读取失败"}</p> : null}
            {!data && !query.isError ? <p className="m-0 text-sm">正在生成建议…</p> : null}
            {data?.status === "running" ? <p className="m-0 text-sm">模型正在改写角色外貌，通常需要几十秒…</p> : null}
            {data?.status === "failed" ? <p className="m-0 text-sm text-[var(--status-error)]">{data.error || "改写失败"}</p> : null}
            {data?.status === "succeeded" && !data.proposals.length ? <p className="m-0 text-sm">没有需要修改的角色。</p> : null}
            {data?.status === "succeeded" && data.proposals.length ? (
                <>
                    <div className="thin-scrollbar flex flex-col gap-3 overflow-y-auto">
                        {data.proposals.map((item) => (
                            <label key={item.castId} className="flex cursor-pointer gap-3 rounded-lg border border-[var(--workspace-border)] p-3">
                                <Checkbox
                                    checked={!unchecked.has(item.castId)}
                                    onChange={(event) => setUnchecked((prev) => { const next = new Set(prev); if (event.target.checked) next.delete(item.castId); else next.add(item.castId); return next; })}
                                />
                                <div className="grid min-w-0 flex-1 grid-cols-2 gap-3 text-xs leading-5">
                                    <div className="col-span-2 text-sm font-medium">{item.name}</div>
                                    <div><div className="opacity-60">原外貌</div>{item.before || "—"}</div>
                                    <div><div className="opacity-60">新外貌</div>{item.after}</div>
                                </div>
                            </label>
                        ))}
                    </div>
                    <div className="flex justify-end gap-2">
                        <Button onClick={onClose}>取消</Button>
                        <Button type="primary" disabled={!selected.length} loading={apply.isPending} onClick={() => apply.mutate()}>
                            应用 {selected.length} 个
                        </Button>
                    </div>
                </>
            ) : null}
        </div>
    );
}
