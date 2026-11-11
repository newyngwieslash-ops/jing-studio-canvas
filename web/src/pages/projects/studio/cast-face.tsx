import { useRef, useState } from "react";
import { useMutation } from "@tanstack/react-query";
import { App, Button } from "antd";
import { FolderOpen, ScanFace, Upload, X } from "lucide-react";

import { setDramaCastFace, type DramaCast } from "@/services/api/drama";
import { uploadResourceFile } from "@/services/api/resources";

import { mediaBusy, MediaThumb } from "../drama-shared";
import { CastLibraryPicker } from "./cast-library-picker";

/**
 * 角色人脸参考：用户上传已确定的人脸 / 形象，生成参考图时五官以它为准，服装、发型与妆容仍按外貌描述和参考图方案；
 * 造型以角色本体参考图为底图，沿用同一张脸。设置后解锁并重新生成，旧图保留在版本记录。
 */
export function CastFaceField({ projectId, cast, restyle, onChanged }: { projectId: string; cast: DramaCast; restyle: boolean; onChanged: () => void }) {
    const { message } = App.useApp();
    const fileRef = useRef<HTMLInputElement>(null);
    const [libraryOpen, setLibraryOpen] = useState(false);
    const faceId = cast.faceResourceId || "";
    const applied = (result: { affectedSegments: number }) => {
        message.success(result.affectedSegments ? `已设置人脸参考，正在重新生成参考图；${result.affectedSegments} 个剧情段的视频需重新生成` : "已设置人脸参考，正在按设定重新生成参考图");
        onChanged();
    };
    const upload = useMutation({
        mutationFn: async (file: File) => {
            const resource = await uploadResourceFile(file, "image", { fileName: file.name });
            return setDramaCastFace(projectId, cast.id, resource.id, restyle);
        },
        onSuccess: applied,
        onError: (error) => message.error(error instanceof Error ? error.message : "上传失败"),
    });
    const clear = useMutation({
        mutationFn: () => setDramaCastFace(projectId, cast.id, "", false),
        onSuccess: () => {
            message.success("已清除人脸参考，之后按外貌描述生成");
            onChanged();
        },
        onError: (error) => message.error(error instanceof Error ? error.message : "清除失败"),
    });
    // 素材库选图的失败由选择弹窗内联显示，这里只处理成功。
    const adoptLibraryImage = async (resourceId: string) => applied(await setDramaCastFace(projectId, cast.id, resourceId, restyle));
    const generating = mediaBusy(cast.imageStatus);
    const busy = upload.isPending || clear.isPending || generating;
    return (
        <div className="drama-look">
            <span className="drama-look-thumb">{faceId ? <MediaThumb resourceId={faceId} alt={`${cast.name}的人脸参考`} /> : <ScanFace className="size-4 opacity-40" />}</span>
            <span className="drama-look-body">
                <b className="truncate">{faceId ? "已指定人脸参考" : "未指定人脸参考"}</b>
                <small>{faceId ? "五官以此为准，服装妆造按设定生成" : "上传确定的人脸，按设定生成参考图"}</small>
            </span>
            <span className="drama-look-actions">
                <Button
                    size="small"
                    type="text"
                    icon={<Upload className="size-3.5" />}
                    loading={upload.isPending}
                    disabled={busy && !upload.isPending}
                    aria-label={`上传${cast.name}的人脸参考`}
                    title={generating ? "参考图生成中，完成后再更换" : "上传人脸参考"}
                    onClick={() => fileRef.current?.click()}
                />
                <Button size="small" type="text" icon={<FolderOpen className="size-3.5" />} disabled={busy} aria-label={`从素材库选择${cast.name}的人脸参考`} title="从素材库选择人脸参考" onClick={() => setLibraryOpen(true)} />
                {faceId ? <Button size="small" type="text" icon={<X className="size-3.5" />} loading={clear.isPending} disabled={busy && !clear.isPending} aria-label="清除人脸参考" title="清除人脸参考" onClick={() => clear.mutate()} /> : null}
            </span>
            {libraryOpen ? <CastLibraryPicker face open={libraryOpen} cast={cast} onClose={() => setLibraryOpen(false)} onPick={adoptLibraryImage} /> : null}
            <input
                ref={fileRef}
                type="file"
                accept="image/png,image/jpeg,image/webp"
                className="hidden"
                aria-label={`上传${cast.name}的人脸参考`}
                onChange={(event) => {
                    const file = event.target.files?.[0];
                    event.target.value = "";
                    if (!file) return;
                    if (!file.type.startsWith("image/")) {
                        message.warning("请选择 PNG、JPG 或 WebP 图片");
                        return;
                    }
                    if (file.size > 20 * 1024 * 1024) {
                        message.warning("图片不超过 20MB");
                        return;
                    }
                    upload.mutate(file);
                }}
            />
        </div>
    );
}
