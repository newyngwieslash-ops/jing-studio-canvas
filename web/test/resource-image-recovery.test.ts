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
}, 30_000);
