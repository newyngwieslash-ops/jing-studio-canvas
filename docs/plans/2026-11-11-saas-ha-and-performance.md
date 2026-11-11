# 影策 SaaS 多副本高可用与高 ROI 性能优化实施计划

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** 在不引入任何新增第三方依赖、不破坏现有接口与账务/权限边界的前提下，打通 `backend` 与 `yingce-agent` 水平扩容（≥2 副本）的 3 个高可用（HA）阻塞点，并按 ROI 落地 6 项核心性能优化（代理连接池复用、高频鉴权与批量资源消 N+1、Redis Pub/Sub 多路复用与 SSE 事件唤醒、云端缩略图直签、Nginx 长连接池、任务并发阈值与部分索引）。

**Architecture:** 全部复用仓库既有基础设施原语——分布式互斥与续租复用 `internal/platform.Coordinator.AcquireLease`，进程内高并发短缓存复用 `internal/platform.BoundedReadCache`，跨实例取消与流式唤醒复用既有 `go-redis/v9` 客户端，缩略图能力下沉至对象存储签名层（`internal/storage` + `internal/assets`），不在 Go 进程内增加图片解码或状态机负担。

**Tech Stack:** Go 1.25 (Gin, GORM, PostgreSQL / SQLite, `go-redis/v9`), Node.js 22 (`yingce-agent` 原生 ESM + `node:test`), Nginx 1.27, Docker Compose.

---

## 总体任务清单与推荐推进顺序

> [!IMPORTANT]
> 工作区当前存在未提交的其他业务改动（如 `drama_*` 等），执行任意任务时严禁使用 `git checkout --`、`git reset --hard` 或宽范围 `git add .`；每个 Task 只 `git add` 本任务明确列出的文件。

| 阶段 | 任务编号 | 对应问题 | 核心改动文件 | 预期收益 |
| --- | --- | --- | --- | --- |
| **第一阶段：多副本 HA 前置必改（≥2 副本门槛）** | **Task 1 (HA-1)** | 云端 Agent（小影）回调错投与恢复重入 | `runtime.go`、`cloud_agent_pi_coordinator.go`、`service.go`、`docker-compose.deploy.yml` | 消除多副本 `/model` `/tool` `/event` 回调 502 与同 `runID` 双跑 |
| | **Task 2 (HA-2)** | 媒体本地流穿透与 `ffmpeg` 争抢 API CPU | `video_transcode.go`、`service.go`、`main.go`、`docker-compose.deploy.yml` | 生产强制校验 OSS/COS + CDN，本地转码有界信号量限流（1~2 并发） |
| | **Task 3 (HA-3)** | `yingce-agent` 单点与每次对话冷启动子进程 | `yingce-agent/server.mjs`、`yingce-agent/server.test.mjs`、`docker-compose.deploy.yml` | 消除每次 `/v1/runs` 数百毫秒 SDK 冷启动抖动，支持 ≥2 无状态副本 |
| **第二阶段：P0 零风险高收益热路径优化** | **Task 4 (P0-1)** | 渠道代理每次新建 `Transport` + 双次 DNS 解析 | `outbound.go`、`outbound_test.go` | 恢复代理渠道 TLS/HTTP2 连接复用（省 150~400ms/跳），消除连接滞留与单 IP 拨号脆弱性 |
| | **Task 5 (P0-2)** | `CurrentUser` 高频查库 + 批量资源/资产 N+1 | `repository_storage_resources.go`、`resource_access.go`、`asset_library.go`、`auth_bridge.go`、`admin_users.go` | 削减 50%+ 高频鉴权 DB 读负载；百节点画布资源签发从 100+ 次 SQL 降至 2 次 |
| **第三阶段：P1 实时流放大治理与云端缩略图** | **Task 6 (P1-1)** | WebSocket 独占 Redis 连接 + SSE 高频空转扫库 + PG 池硬编码 | `realtime.go`、`wake_hub.go`、`task_text_stream.go`、`routes.go`、`agent.go`、`database.go` | 千人在线 Redis 连接从 `O(标签页)` 降至 `O(活跃房间)`；SSE 空转查库降 80%+ 且首包 <5ms |
| | **Task 7 (P1-2)** | 画布 `VariantThumbnail` 未生效（全量拉 4K 原图） | `storage/objects.go`、`storage/delivery.go`、`assets/access.go` | 零 Go CPU 开销直签云端 640px WebP 缩略图，画布首屏图片带宽与显存降 80%+ |
| **第四阶段：P2 网关长连接与大表调度基线** | **Task 8 (P2-1)** | Nginx 到后端缺少 `upstream keepalive` | `nginx.conf` | 消除 `/api/` 每次请求新建/挥手内网 TCP 连接开销 |
| | **Task 9 (P2-2)** | 默认 Worker 并发过低 & `tasks` 百万行领取变慢 | `migrations.go` (v94)、`docker-compose.deploy.yml`、`.env.example`、`backend-database.mdx` | 提升 I/O 型任务吞吐；部分索引锁定活跃任务，百万历史行下 `ClaimNextTask` 保持亚毫秒 |

---

## 第一阶段：水平扩容（≥2 副本）前必改的 3 个 HA 卡点

### Task 1 (HA-1): 云端 Agent（小影）多副本回调寻址、分布式租约与取消广播

**Files:**
- Modify: `backend/internal/agent/runtime/runtime.go`
- Test: `backend/internal/agent/runtime/runtime_test.go`
- Modify: `backend/internal/app/cloud_agent_pi_coordinator.go`
- Modify: `backend/internal/app/service.go`
- Modify: `docker-compose.deploy.yml`
- Test: `backend/internal/app/cloud_agent_pi_ha_test.go`

**Step 1: Write the failing tests**

在 `backend/internal/agent/runtime/runtime_test.go` 增加自动探测本机非回环 IP 的单测（保留显式 `BridgeHost` 优先级，支持 `"auto"` 或多网卡容器环境）：

```go
func TestResolveBridgeAdvertiseHostPrefersExplicitAndAutoDetectsNonLoopback(t *testing.T) {
	if got := resolveBridgeAdvertiseHost("10.244.1.18"); got != "10.244.1.18" {
		t.Fatalf("explicit host = %q, want 10.244.1.18", got)
	}
	autoHost := resolveBridgeAdvertiseHost("auto")
	if autoHost == "" || autoHost == "backend" || autoHost == "0.0.0.0" {
		t.Fatalf("auto host = %q, want concrete routable or loopback address", autoHost)
	}
}
```

新建 `backend/internal/app/cloud_agent_pi_ha_test.go`，验证两个 `Service` 实例共享同一个 `Coordinator` 时，同一 `runID` 只有一个副本能获得 `agent-run:<runID>` 执行租约，且跨实例取消信号能触发目标实例注册的 `cancel()`：

```go
package app

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
	"yingce/backend/internal/platform"
)

func TestCloudAgentDistributedLeaseBlocksDuplicateReplicaStart(t *testing.T) {
	mr := miniredis.RunT(t)
	rdb := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdb.Close() })

	coordA := platform.NewCoordinator(rdb, "replica-a")
	coordB := platform.NewCoordinator(rdb, "replica-b")

	leaseA, ok, err := acquireCloudAgentRunLease(context.Background(), coordA, "run-1")
	if err != nil || !ok {
		t.Fatalf("replica A acquire = (%v, %v), want true", ok, err)
	}
	defer leaseA.Release()

	if _, okB, errB := acquireCloudAgentRunLease(context.Background(), coordB, "run-1"); errB != nil || okB {
		t.Fatalf("replica B acquire while A holds lease = (%v, %v), want false", okB, errB)
	}
}

func TestCloudAgentCancelPubSubStopsRemoteReplicaRunner(t *testing.T) {
	mr := miniredis.RunT(t)
	rdbA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdbB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdbA.Close(); _ = rdbB.Close() })

	svcA := &Service{redis: rdbA}
	svcB := &Service{redis: rdbB}
	stopSub := svcA.startCloudAgentCancelSubscriber()
	defer stopSub()

	var cancelled atomic.Bool
	svcA.piMu.Lock()
	svcA.piRunners = map[string]context.CancelFunc{
		"run-remote-1": func() { cancelled.Store(true) },
	}
	svcA.piMu.Unlock()

	// 等待订阅就绪后从副本 B 发起取消
	time.Sleep(30 * time.Millisecond)
	svcB.stopCloudAgentPi("run-remote-1")

	deadline := time.Now().Add(time.Second)
	for !cancelled.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !cancelled.Load() {
		t.Fatal("expected remote replica runner to be cancelled via Redis Pub/Sub")
	}
}
```

**Step 2: Run test to verify it fails**

Run:
```powershell
cd backend; go test ./internal/agent/runtime -run TestResolveBridgeAdvertiseHost -v; go test ./internal/app -run "TestCloudAgentDistributedLease|TestCloudAgentCancelPubSub" -v
```
Expected: 编译失败（`resolveBridgeAdvertiseHost`、`acquireCloudAgentRunLease`、`startCloudAgentCancelSubscriber` 未定义）。

**Step 3: Write minimal implementation**

1. 在 `backend/internal/agent/runtime/runtime.go` 中增加 `resolveBridgeAdvertiseHost(bridgeHost string) string`：
   - 当 `bridgeHost == ""` 时：单机内嵌模式默认返回 `"127.0.0.1"`；
   - 当 `bridgeHost == "auto"` 时（供容器远程 `yingce-agent` 调用默认传入）：遍历 `net.InterfaceAddrs()` 返回首个非回环、非链路本地 IPv4 地址（兜底首个全局单播 IPv6，再兜底 `os.Hostname()`、`127.0.0.1`）；
   - 其他显式值原样返回。
   - 在 `backend/internal/app/cloud_agent_pi_runtime.go` 初始化 `yingceagent` 时，若配置了远程 `YINGCE_AGENT_URL` 且未显式指定 `YINGCE_AGENT_BRIDGE_HOST`（或值为 `"backend"`），默认传 `"auto"`，使每个副本向 `yingce-agent` 通告自身容器 IP，监听 `0.0.0.0:0` 时回调直达本副本。
2. 在 `docker-compose.deploy.yml` 移除 `YINGCE_AGENT_BRIDGE_HOST: backend`（或改为 `${YINGCE_AGENT_BRIDGE_HOST:-auto}`）。
3. 在 `backend/internal/app/cloud_agent_pi_coordinator.go` 中：
   - 新增 `const cloudAgentRunLeaseTTL = 45 * time.Second`、`const cloudAgentCancelChannel = "canvas:agent:cancel"`；
   - 新增 `acquireCloudAgentRunLease(ctx, coord, runID)` 调用 `coord.AcquireLease(ctx, "agent-run:"+runID, 1, cloudAgentRunLeaseTTL)`（`coord == nil` 时返回空操作 lease 保持单测兼容）；
   - 在 `startCloudAgentPi` 的 goroutine 内先抢占 `acquireCloudAgentRunLease`，未抢到直接退出（说明另一副本已在运行/恢复同一 `runID`）；抢到后启动 `20s` 周期的 `lease.Renew(runCtx)` 子协程，并在 `defer` 中执行 `lease.Release()`；
   - 在 `stopCloudAgentPi(runID)` 除取消本进程 `piRunners[runID]` 外，若 `s.redis != nil`，异步/短超时执行 `s.redis.Publish(ctx, cloudAgentCancelChannel, runID)`；
   - 新增 `(s *Service) startCloudAgentCancelSubscriber() func()` 订阅 `cloudAgentCancelChannel`，收到 `runID` 时仅触发本地 `stopLocalCloudAgentPi(runID)`（避免回环重复 Publish），并在 `Service` 启动与 `Close()` 生命周期内挂载。

**Step 4: Run test to verify it passes**

Run:
```powershell
cd backend; go test ./internal/agent/... ./internal/app -run "TestResolveBridgeAdvertiseHost|TestCloudAgentDistributedLease|TestCloudAgentCancelPubSub|TestCloudAgent|TestAgentCancel" -v
```
Expected: PASS。

**Step 5: Commit**

```powershell
git add backend/internal/agent/runtime/runtime.go backend/internal/agent/runtime/runtime_test.go backend/internal/app/cloud_agent_pi_runtime.go backend/internal/app/cloud_agent_pi_coordinator.go backend/internal/app/cloud_agent_pi_ha_test.go backend/internal/app/service.go docker-compose.deploy.yml
git commit -m "fix(agent): 云端Agent - 支持多副本容器IP回调寻址与分布式租约互斥取消"
```

---

### Task 2 (HA-2): SaaS 对象存储生产就绪门禁与本地 `ffmpeg` 转码有界并发隔离

**Files:**
- Modify: `backend/internal/app/video_transcode.go`
- Modify: `backend/internal/app/service.go`
- Modify: `backend/cmd/server/main.go`
- Modify: `docker-compose.deploy.yml`
- Modify: `.env.example`
- Test: `backend/internal/app/video_transcode_limit_test.go`

**Step 1: Write the failing test**

新建 `backend/internal/app/video_transcode_limit_test.go`，测试：
1. 本地转码信号量 `transcodeSem` 容量为 `1` 时，突发触发 5 个转码任务时任意时刻最多只有 `1` 个并发进入转码执行体；
2. 开启 `CANVAS_REQUIRE_OBJECT_STORAGE=true` 且系统未启用公网对象存储（仍为 `local`）时，`ValidateSaaSStorageReadiness()` 返回明确阻断错误，防止多副本线上误用本地磁盘存储导致跨机 404。

```go
package app

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"
)

func TestAcquireTranscodeSlotBoundsConcurrency(t *testing.T) {
	svc := &Service{transcodeSem: make(chan struct{}, 1)}
	var active, maxActive atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, ok := svc.acquireTranscodeSlot(context.Background())
			if !ok {
				t.Error("expected slot acquisition")
				return
			}
			defer release()
			cur := active.Add(1)
			for {
				prev := maxActive.Load()
				if cur <= prev || maxActive.CompareAndSwap(prev, cur) {
					break
				}
			}
			time.Sleep(15 * time.Millisecond)
			active.Add(-1)
		}()
	}
	wg.Wait()
	if got := maxActive.Load(); got != 1 {
		t.Fatalf("max concurrent transcodes = %d, want 1", got)
	}
}

func TestValidateSaaSStorageReadinessRejectsLocalStorageWhenRequired(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.SystemSetting{}); err != nil {
		t.Fatal(err)
	}
	svc := &Service{repo: repository.New(db)}
	if err := svc.ValidateSaaSStorageReadiness(true); err == nil {
		t.Fatal("expected error when object storage is required but not enabled")
	}
	if err := svc.ValidateSaaSStorageReadiness(false); err != nil {
		t.Fatalf("unexpected error when guard disabled: %v", err)
	}
}
```

**Step 2: Run test to verify it fails**

Run:
```powershell
cd backend; go test ./internal/app -run "TestAcquireTranscodeSlotBoundsConcurrency|TestValidateSaaSStorageReadiness" -v
```
Expected: 编译失败（`transcodeSem`、`acquireTranscodeSlot`、`ValidateSaaSStorageReadiness` 未定义）。

**Step 3: Write minimal implementation**

1. 在 `backend/internal/app/service.go`：
   - 给 `Service` 结构体增加 `transcodeSem chan struct{}`，在 `NewService` 中读取环境变量 `CANVAS_TRANSCODE_CONCURRENCY`（默认 `1`，允许范围 `[1, 4]`）初始化 `make(chan struct{}, limit)`；
   - 实现 `(s *Service) acquireTranscodeSlot(ctx context.Context) (func(), bool)`：当 `s.transcodeSem == nil` 时直接返回空 release（兼容直接构造 `&Service{}` 的旧测试）；否则 `select` 等待 `s.transcodeSem <- struct{}{}` 或 `ctx.Done()` / `s.stopCh`；
   - 实现 `(s *Service) ValidateSaaSStorageReadiness(requireObjectStorage bool) error`：当 `requireObjectStorage` 为 `true` 时读取系统对象存储配置（`s.systemOSSSetting()`），若 `!setting.Enabled` 则返回明确中文错误：`"已启用 CANVAS_REQUIRE_OBJECT_STORAGE，SaaS 多副本模式必须在后台配置并启用对象存储（OSS/COS/S3/TOS/Kodo）"`；若未配置 `CDNBaseURL` 且非公网直连源站则输出启动告警日志提醒开启 CDN 以避免回退 API 代理流量。
2. 在 `backend/internal/app/video_transcode.go`：
   - 在 `runPlaybackTranscode(resourceID string)` 执行 `ffmpeg` 前先调用 `release, ok := s.acquireTranscodeSlot(s.workerCtx())`；若进程正在关闭（`!ok`），将已抢占的 `processing` 状态重置回 `""` 以便下次恢复，不再无上限并发拉起 `ffmpeg` 进程。
3. 在 `backend/cmd/server/main.go`、`docker-compose.deploy.yml` 与 `.env.example`：
   - 增加 `CANVAS_TRANSCODE_CONCURRENCY`（默认 `1`）与 `CANVAS_REQUIRE_OBJECT_STORAGE`（默认 `false`，SaaS 多副本生产文档与 `.env.example` 推荐设为 `true`）接线。

**Step 4: Run test to verify it passes**

Run:
```powershell
cd backend; go test ./internal/app -run "TestAcquireTranscodeSlotBoundsConcurrency|TestValidateSaaSStorageReadiness|TestPlayback" -v
```
Expected: PASS。

**Step 5: Commit**

```powershell
git add backend/internal/app/video_transcode.go backend/internal/app/service.go backend/internal/app/video_transcode_limit_test.go backend/cmd/server/main.go docker-compose.deploy.yml .env.example
git commit -m "perf(media): 媒体存储与转码 - 增加本地ffmpeg并发信号量与SaaS对象存储门禁"
```

---

### Task 3 (HA-3): `yingce-agent` 预热待命子进程池与无状态多副本就绪

**Files:**
- Modify: `yingce-agent/server.mjs`
- Create: `yingce-agent/server.test.mjs`
- Modify: `docker-compose.deploy.yml`
- Modify: `.env.example`

**Step 1: Write the failing test**

> 原理确认：`backend/agent-runtime/pi/agent-runtime.mjs` 顶层 ESM `import` 会先完成 `@earendil-works/pi-coding-agent`、`@earendil-works/pi-ai`、`undici` 的全部加载与初始化，随后停在 `for await (const chunk of process.stdin)` 等待输入，且 `mkdtemp` 隔离目录在收到请求后才创建。因此预启动待命子进程天然做到 **100% 预热 SDK + 0 会话状态污染 + 用完即销毁**。

新建 `yingce-agent/server.test.mjs`（使用 Node 内置 `node:test` 与 `node:assert/strict`）：

```javascript
import test from "node:test";
import assert from "node:assert/strict";
import { createWarmProcessPool } from "./server.mjs";
import { fileURLToPath } from "node:url";
import { dirname, join } from "node:path";
import { writeFileSync, mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";

test("warm process pool hands out distinct pre-spawned children and replenishes", async () => {
  const dir = mkdtempSync(join(tmpdir(), "warm-pool-test-"));
  const fixture = join(dir, "echo-worker.mjs");
  writeFileSync(
    fixture,
    `let raw = "";
for await (const c of process.stdin) raw += c;
process.stdout.write(JSON.stringify({ pid: process.pid, echo: JSON.parse(raw).id }) + "\\n");
`
  );
  const pool = createWarmProcessPool({
    size: 2,
    scriptPath: fixture,
    nodeOptions: "--max-old-space-size=128",
  });
  try {
    await new Promise((r) => setTimeout(r, 80));
    assert.equal(pool.idleCount(), 2);

    const first = pool.acquire();
    const second = pool.acquire();
    assert.notEqual(first.pid, second.pid, "each run must get an isolated OS process");

    const readLine = (child, id) =>
      new Promise((resolve, reject) => {
        let out = "";
        child.stdout.on("data", (d) => (out += d.toString()));
        child.on("error", reject);
        child.on("close", (code) =>
          code === 0 ? resolve(JSON.parse(out.trim())) : reject(new Error(`exit ${code}`))
        );
        child.stdin.end(JSON.stringify({ id }));
      });

    const [res1, res2] = await Promise.all([readLine(first, "run-1"), readLine(second, "run-2")]);
    assert.equal(res1.echo, "run-1");
    assert.equal(res2.echo, "run-2");

    await new Promise((r) => setTimeout(r, 120));
    assert.equal(pool.idleCount(), 2, "pool must replenish consumed processes");
  } finally {
    pool.close();
    rmSync(dir, { recursive: true, force: true });
  }
});
```

**Step 2: Run test to verify it fails**

Run:
```powershell
node --test yingce-agent/server.test.mjs
```
Expected: FAIL（`createWarmProcessPool` 未导出，且 `server.mjs` 直接顶层 `server.listen` 阻塞退出）。

**Step 3: Write minimal implementation**

修改 `yingce-agent/server.mjs`：
1. 导出 `createWarmProcessPool({ size, scriptPath, nodeOptions })`：
   - 维护最多 `size`（读取 `YINGCE_AGENT_WARM_POOL_SIZE`，默认 `2`，夹逼到 `[0, 8]`）个预启动的 `spawn(process.execPath, [scriptPath], { env: cleanRuntimeEnv(nodeOptions), stdio: ["pipe", "pipe", "pipe"] })`；
   - 若待命子进程在未被领取前意外退出或报错，自动从队列剔除并延迟退避补充；
   - `acquire()`：弹出队首存活的预热子进程并 `setImmediate(fill)` 异步补齐池容量；池为空时即时同步 `spawnOne()` 兜底（绝不阻塞请求）；每个子进程严格只服务一次 `/v1/runs`，结束后自然退出，绝不回收复用；
   - `close()`：关闭池并在 `SIGTERM` / `SIGINT` 时 `kill("SIGTERM")` 清理空闲待命子进程。
2. `/healthz` 响应追加 `warmPoolIdle: warmPool.idleCount()` 便于探针观察。
3. 仅当入口模块直接执行（`import.meta.url === pathToFileURL(process.argv[1]).href`）时启动 `server.listen`，以便 `node --test` 直接导入单测。
4. 在 `docker-compose.deploy.yml` 与 `.env.example` 为 `yingce-agent` 增加 `YINGCE_AGENT_WARM_POOL_SIZE: ${YINGCE_AGENT_WARM_POOL_SIZE:-2}` 及 `deploy.replicas: ${YINGCE_AGENT_REPLICAS:-2}` 无状态多副本配置。

**Step 4: Run test to verify it passes**

Run:
```powershell
node --test yingce-agent/server.test.mjs
```
Expected: PASS。

**Step 5: Commit**

```powershell
git add yingce-agent/server.mjs yingce-agent/server.test.mjs docker-compose.deploy.yml .env.example
git commit -m "perf(agent): yingce-agent运行时 - 增加单次隔离预热子进程池并支持无状态多副本"
```

---

## 第二阶段：P0 零风险高收益热路径优化

### Task 4 (P0-1): 渠道代理 `http.Transport` 连接池复用与无代理多 IP 拨号回退

**Files:**
- Modify: `backend/internal/outbound/outbound.go`
- Modify: `backend/internal/outbound/outbound_test.go`

**Step 1: Write the failing tests**

在 `backend/internal/outbound/outbound_test.go` 追加两个测试：
1. 相同代理地址（即使 timeout 不同）调用 `OutboundHTTPClientWithProxy` 必须复用同一个底层 `*http.Transport` 实例（保持 TLS 会话与 HTTP/2 连接池，杜绝每次请求新建 Transport 泄漏 90s 空闲连接）；不同代理地址隔离 Transport。
2. 多 IP 目标（首个 IP 连接拒绝、第二个 IP 正常监听）在拨号阶段能够顺序回退成功，而不是只拨 `addresses[0]` 直接报错。

```go
func TestOutboundHTTPClientWithProxyReusesTransportPerProxy(t *testing.T) {
	t.Setenv("CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS", "127.0.0.1")
	c1 := OutboundHTTPClientWithProxy(10*time.Second, "http://127.0.0.1:8888")
	c2 := OutboundHTTPClientWithProxy(30*time.Second, "http://127.0.0.1:8888")
	c3 := OutboundHTTPClientWithProxy(10*time.Second, "http://127.0.0.1:8889")

	if c1.Transport != c2.Transport {
		t.Fatal("expected identical proxyURL to share pooled *http.Transport")
	}
	if c1.Transport == c3.Transport {
		t.Fatal("expected distinct proxyURLs to use distinct *http.Transport instances")
	}
}

func TestDialValidatedAddressesFallsBackToHealthyIP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	_, port, _ := net.SplitHostPort(ln.Addr().String())

	// 找一个已关闭的本地端口构造首 IP 失败场景
	closedLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedAddr := closedLn.Addr().String()
	_ = closedLn.Close()

	conn, err := dialSequentialTargets(context.Background(), &net.Dialer{Timeout: 500 * time.Millisecond}, []string{
		closedAddr,
		net.JoinHostPort("127.0.0.1", port),
	})
	if err != nil {
		t.Fatalf("dialSequentialTargets failed to fall back: %v", err)
	}
	_ = conn.Close()
}
```

**Step 2: Run test to verify it fails**

Run:
```powershell
cd backend; go test ./internal/outbound -run "TestOutboundHTTPClientWithProxyReusesTransportPerProxy|TestDialValidatedAddressesFallsBackToHealthyIP" -v
```
Expected: FAIL（`c1.Transport != c2.Transport`，且 `dialSequentialTargets` 未定义）。

**Step 3: Write minimal implementation**

修改 `backend/internal/outbound/outbound.go`：
1. 增加代理 Transport 缓存（带上限保护，例如最多缓存 256 个不同代理，超出时对淘汰项调用 `CloseIdleConnections()`，防止动态随机代理打爆内存）：
   - Key 由规范化后的 `proxyParsed.String()` + 当前私网放行配置摘要组成（避免单测动态修改 `CANVAS_ALLOWED_PRIVATE_UPSTREAM_HOSTS` 串缓存）；
   - `OutboundHTTPClientWithProxy(timeout, proxyURL)` 命中缓存时直接返回 `&http.Client{Timeout: timeout, Transport: pooledTransport, CheckRedirect: validateOutboundRedirect}`。
2. 在 `newOutboundTransportWithProxy` 的 `DialContext` 中：
   - 提取 `dialSequentialTargets(ctx, dialer, targets []string) (net.Conn, error)`：按顺序尝试已通过 SSRF 校验的候选 `net.JoinHostPort(ip.String(), port)`，单目标拨号失败且 `ctx.Err() == nil` 时继续尝试下一个已校验 IP，全部失败返回首个错误；
   - 支持从 `ctx` 读取上游刚校验通过的 IP 列表（可选 `WithValidatedHostAddrs`），避免同一次出站请求内 `LookupIPAddr` 执行两次。

**Step 4: Run test to verify it passes**

Run:
```powershell
cd backend; go test ./internal/outbound/... -v
```
Expected: 全部测试 PASS。

**Step 5: Commit**

```powershell
git add backend/internal/outbound/outbound.go backend/internal/outbound/outbound_test.go
git commit -m "perf(outbound): 模型渠道网络层 - 复用代理Transport连接池并支持多IP拨号回退"
```

---

### Task 5 (P0-2): `CurrentUser` 短 TTL 读缓存（含登出/封禁瞬时吊销）与批量资源/资产接口消 N+1

**Files:**
- Modify: `backend/internal/repository/repository_storage_resources.go`
- Modify: `backend/internal/app/resource_access.go`
- Modify: `backend/internal/repository/project_character.go`
- Modify: `backend/internal/app/asset_library.go`
- Modify: `backend/internal/app/auth_bridge.go`
- Modify: `backend/internal/app/admin_users.go`
- Modify: `backend/internal/app/service.go`
- Test: `backend/internal/app/resource_access_batch_test.go`
- Test: `backend/internal/app/admin_user_disable_session_test.go`

**Step 1: Write the failing tests**

新建 `backend/internal/app/resource_access_batch_test.go`，利用 GORM callback 统计 SQL 查询次数，验证：
1. `ResourceAccessBatch` 查询 20 个同用户资源时，SQL 查询次数为常数级（≤3 次，而不是 ≥20 次 N+1），且仍严格保持缺失/越权资源静默忽略与输入去重保序；
2. `CurrentUser` 连续调用同一有效 Cookie 在 3s 窗口内命中内存缓存（第 2 次 0 SQL），而调用 `Logout`、`UpdateUser(..., Disabled)`、`DeleteUser` 或 `BulkDisableUsers` 后**立即**（0ms 延迟，跨实例通过 Redis 吊销键）失效，绝不放行已封禁/已登出会话。

```go
package app

import (
	"testing"
	"time"

	"gorm.io/driver/sqlite"
	"gorm.io/gorm"
	"yingce/backend/internal/assets"
	"yingce/backend/internal/model"
	"yingce/backend/internal/repository"
)

func TestResourceAccessBatchAvoidsNPlusOneQueries(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&model.Resource{}, &model.SystemSetting{}, &model.StorageLocation{}, &model.UserOSSSetting{}); err != nil {
		t.Fatal(err)
	}
	ids := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		id := newID()
		ids = append(ids, id)
		if err := db.Create(&model.Resource{
			ID: id, UserID: "u1", Kind: "image", Status: model.ResourceStatusReady,
			Provider: "local", ObjectKey: "u1/" + id + ".png", MimeType: "image/png",
		}).Error; err != nil {
			t.Fatal(err)
		}
	}

	var queries int
	_ = db.Callback().Query().After("gorm:query").Register("count_queries", func(*gorm.DB) { queries++ })

	svc := &Service{repo: repository.New(db)}
	got, err := svc.ResourceAccessBatch("u1", ids, assets.AccessRequest{Intent: assets.IntentDisplay, Variant: assets.VariantOriginal})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 20 {
		t.Fatalf("got %d descriptors, want 20", len(got))
	}
	if queries > 4 {
		t.Fatalf("ResourceAccessBatch executed %d SQL queries for 20 resources, want <= 4", queries)
	}
}
```

并在 `backend/internal/app/admin_user_disable_session_test.go` 增加缓存命中与封禁瞬时吊销断言（连续两次 `CurrentUser` 后禁用账号，下一次 `CurrentUser` 立即返回错误）。

**Step 2: Run test to verify it fails**

Run:
```powershell
cd backend; go test ./internal/app -run "TestResourceAccessBatchAvoidsNPlusOneQueries|TestUpdateUserDisableRevokesSessions" -v
```
Expected: `TestResourceAccessBatchAvoidsNPlusOneQueries` 失败（实际执行 20+ 次 SQL）。

**Step 3: Write minimal implementation**

1. **批量资源消 N+1**：
   - 在 `backend/internal/repository/repository_storage_resources.go` 增加 `ResourcesForUserByIDs(userID string, ids []string) ([]model.Resource, error)`（`WHERE user_id = ? AND id IN ?`）；
   - 在 `backend/internal/app/resource_access.go` 的 `ResourceAccessBatch` 中：先做输入去重与最大 100 条校验，一次调用 `s.repo.ResourcesForUserByIDs(userID, uniqueIDs)` 构建 `byID` 字典，并用请求级 `settingMemo := map[string]*storage.Settings{}` 缓存 `ossSettingForResource` 结果（同一 `userID + provider + storageSettingID` 只查一次），按请求顺序组装返回。
2. **素材分页角色卡消 N+1**：
   - 在 `backend/internal/repository/project_character.go` 增加按 `(userID, libraryAssetIDs)` 批量查询角色卡方法，在 `backend/internal/app/asset_library.go` 的 `UserAssetsPage` 中收集当前页 `AssetCategoryCharacter` 的 `ID` 列表一次性查回。
3. **`CurrentUser` 3s 有界读缓存 + 瞬时吊销**：
   - 在 `Service` 上挂载 `authSessionCache *platform.BoundedReadCache[string, model.User]`（16 分片、4MB 上限、`3 * time.Second` TTL，返回浅拷贝 `u := cached; return &u, nil` 防止调用方原地修改污染缓存）；
   - Key 为 `sessionID + ":" + hashToken(secret)`（不含明文 token）；
   - 读取前若 `s.redis != nil`，用单次 Pipeline 检查是否存在短期吊销标记 `canvas:auth:revoked-session:<sessionID>` 或 `canvas:auth:revoked-user:<userID>`（也可在命中本地缓存后校验 `revokedUsers` 本地同步 map + Redis 标记）；
   - 在 `Logout`（吊销 `sessionID`）以及 `UpdateUser`（当状态变更为 `Disabled` 或角色变更时）、`DeleteUser`、`BulkDisableUsers` 中调用 `s.invalidateUserAuthCache(userIDs...)`：立即清理本机缓存键并向 Redis 写入 `SET canvas:auth:revoked-user:<uid> 1 EX 10`（10s > 3s 本地缓存 TTL），确保多副本下封禁/改角色/登出 **0ms 全局生效**。

**Step 4: Run test to verify it passes**

Run:
```powershell
cd backend; go test ./internal/app ./internal/auth -run "TestResourceAccessBatch|TestUpdateUserDisableRevokesSessions|TestBulkDisable|TestUserAssets|TestAuth" -v
```
Expected: PASS。

**Step 5: Commit**

```powershell
git add backend/internal/repository/repository_storage_resources.go backend/internal/app/resource_access.go backend/internal/repository/project_character.go backend/internal/app/asset_library.go backend/internal/app/auth_bridge.go backend/internal/app/admin_users.go backend/internal/app/service.go backend/internal/app/resource_access_batch_test.go backend/internal/app/admin_user_disable_session_test.go
git commit -m "perf(auth,assets): 鉴权与资源访问 - 增加CurrentUser短缓存吊销并消除批量资源N+1查询"
```

---

## 第三阶段：P1 实时流连接模型与画布云端缩略图

### Task 6 (P1-1): `RealtimeHub` Redis 房间多路复用、SSE 事件唤醒与 PG 连接池可配置化

**Files:**
- Modify: `backend/internal/canvas/realtime.go`
- Test: `backend/internal/canvas/realtime_test.go`
- Create: `backend/internal/platform/wake_hub.go`
- Test: `backend/internal/platform/wake_hub_test.go`
- Modify: `backend/internal/app/service.go`
- Modify: `backend/internal/app/task_text_stream.go`
- Modify: `backend/internal/app/cloud_agent_runtime_media.go`
- Modify: `backend/internal/handler/routes.go`
- Modify: `backend/internal/handler/agent.go`
- Modify: `backend/internal/database/database.go`
- Test: `backend/internal/database/pool_test.go`
- Modify: `.env.example`

**Step 1: Write the failing tests**

1. 在 `backend/internal/canvas/realtime_test.go` 新增测试：同一画布房间打开 10 个并发客户端连接时，基于 `miniredis` 验证 Redis 侧活跃 `PUBSUB NUMSUB` 订阅者数量仅为 **1**（按房间共享单条 `redis.PubSub` 进程内分发，最后一个客户端断开后自动退订归零）。
2. 新建 `backend/internal/platform/wake_hub_test.go`：验证两个 `WakeHub` 实例通过 `miniredis` 连接时，实例 A 调用 `Notify("task-text:t1")` 能在 `<50ms` 内唤醒实例 B 上 `Subscribe("task-text:t1")` 的 `<-ch`，且无 Redis 时单机内存通知同样生效。
3. 新建 `backend/internal/database/pool_test.go`：验证 `parsePoolOptionsFromEnv` 支持 `CANVAS_DB_MAX_OPEN_CONNS`、`CANVAS_DB_MAX_IDLE_CONNS`、`CANVAS_DB_CONN_MAX_LIFETIME`，未设置时保留安全默认值（`maxOpen=50`, `maxIdle=25`, `maxLifetime=30m`）。

```go
// backend/internal/platform/wake_hub_test.go
package platform

import (
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/redis/go-redis/v9"
)

func TestWakeHubCrossInstanceNotification(t *testing.T) {
	mr := miniredis.RunT(t)
	rdbA := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	rdbB := redis.NewClient(&redis.Options{Addr: mr.Addr()})
	t.Cleanup(func() { _ = rdbA.Close(); _ = rdbB.Close() })

	hubA := NewWakeHub(rdbA)
	defer hubA.Close()
	hubB := NewWakeHub(rdbB)
	defer hubB.Close()

	ch, unsub := hubB.Subscribe("task-text:123")
	defer unsub()

	time.Sleep(30 * time.Millisecond)
	hubA.Notify("task-text:123")

	select {
	case <-ch:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cross-instance wake signal")
	}
}
```

**Step 2: Run test to verify it fails**

Run:
```powershell
cd backend; go test ./internal/platform -run TestWakeHubCrossInstanceNotification -v; go test ./internal/canvas -v; go test ./internal/database -run TestPool -v
```
Expected: 编译失败（`NewWakeHub` 等未定义）。

**Step 3: Write minimal implementation**

1. **`RealtimeHub` 按房间共享 Redis 订阅（`backend/internal/canvas/realtime.go`）**：
   - 将每个 `Open()` 独立执行 `h.redis.Subscribe(ctx, h.channelName(room))` 改为：在 `RealtimeHub` 内维护 `relays map[string]*roomRelay`（受 `h.mu` 保护）；
   - 房间首个本地客户端加入时创建 1 条 `redis.Subscribe` 后台读取协程，收到远端实例消息后调用既有 `h.deliverLocal(...)` 扇出给本房间所有本地客户端；
   - 房间最后一个本地客户端在 `Close()` 移除时，取消该 `roomRelay` 并关闭对应的 `PubSub` 连接。
2. **轻量跨实例事件唤醒器（`backend/internal/platform/wake_hub.go`）**：
   - `NewWakeHub(rdb *redis.Client)`：全进程仅维护 **1 条** `rdb.PSubscribe(ctx, "canvas:wake:*")` 连接，加本地 `map[string]map[chan struct{}]struct{}`（缓冲长度 `1` 的非阻塞通知 channel）；
   - `Notify(topic string)`：本地非阻塞触发 `select { case ch <- struct{}{}: default: }`；若 `rdb != nil` 则短超时 `rdb.Publish(ctx, "canvas:wake:"+topic, "1")`；
   - 在 `AppendTaskTextDelta` 与任务终态写库后调用 `s.wakeHub.Notify("task-text:" + taskID)`；在 `MutateCloudAgent` / 事件追加后调用 `s.wakeHub.Notify("agent-run:" + runID)`；
   - 在 `streamTaskTextEvents`（`backend/internal/handler/routes.go`）和 `/agent/runs/:id/events`（`backend/internal/handler/agent.go`）中订阅对应 `wakeCh`：收到 `<-wakeCh` 立即查库推送；兜底轮询 `time.NewTicker` 从 `750ms` / `1s` 放宽至 `5 * time.Second`（即使 Redis 瞬断也只是退回 5s 兜底，不断流、不丢游标）。
3. **PostgreSQL 连接池参数化（`backend/internal/database/database.go`）**：
   - 在 `ConfigurePool` 中读取 `CANVAS_DB_MAX_OPEN_CONNS`（默认 `50`）、`CANVAS_DB_MAX_IDLE_CONNS`（默认 `25`）、`CANVAS_DB_CONN_MAX_LIFETIME`（默认 `30m`），设置 `sqlDB.SetConnMaxLifetime` 与 `SetConnMaxIdleTime(10 * time.Minute)`，兼容 PgBouncer 事务/会话池回收。

**Step 4: Run test to verify it passes**

Run:
```powershell
cd backend; go test ./internal/platform ./internal/canvas ./internal/database ./internal/handler -run "TestWakeHub|TestRealtime|TestPool|TestTaskTextEvents|TestAgent" -v
```
Expected: PASS。

**Step 5: Commit**

```powershell
git add backend/internal/canvas/realtime.go backend/internal/canvas/realtime_test.go backend/internal/platform/wake_hub.go backend/internal/platform/wake_hub_test.go backend/internal/app/service.go backend/internal/app/task_text_stream.go backend/internal/app/cloud_agent_runtime_media.go backend/internal/handler/routes.go backend/internal/handler/agent.go backend/internal/database/database.go backend/internal/database/pool_test.go .env.example
git commit -m "perf(realtime,sse): 实时推送与连接池 - 共享房间Redis订阅并改SSE为事件唤醒驱动"
```

---

### Task 7 (P1-2): 画布 `VariantThumbnail` 对象存储云端图片处理直签（OSS / COS / 七牛）

**Files:**
- Modify: `backend/internal/storage/objects.go`
- Modify: `backend/internal/storage/delivery.go`
- Test: `backend/internal/storage/signing_test.go`
- Modify: `backend/internal/assets/access.go`
- Test: `backend/internal/assets/access_test.go`

**Step 1: Write the failing tests**

> 签名契约核对：
> - **阿里云 OSS V1 签名**（`signedAliyunOSSObjectURL`）：`x-oss-process` 属于子资源（sub-resource），必须按字典序拼入 `CanonicalizedResource`（`response-cache-control` < `response-content-disposition` < `x-oss-process`，且签名原文中值不 URL 转义：`x-oss-process=image/resize,w_640,m_lfit/format,webp`），否则 OSS 返回 `SignatureDoesNotMatch`；
> - **腾讯云 COS 签名**（`signedCOSObjectURL`）：写入 `PresignedURLOptions.Query`（无值参数键 `imageMogr2/thumbnail/640x/format/webp` 或 `x-oss-process` 兼容处理）；
> - **七牛 Kodo / 公网 CDN**：在签名前将图片处理参数拼入 URL 查询串，`MakePrivateURLv2` 会连同查询串一并计算私有签名；
> - **不支持云端光栅缩放的类型**（如 `image/svg+xml`、`local` 本地存储、原生 `s3` / 无图片处理的自定义端点）：自动保持原有回退 `ActualVariant = VariantOriginal, FallbackReason = "thumbnail_not_ready"`，绝不生成 400 坏链。

在 `backend/internal/storage/signing_test.go` 与 `backend/internal/assets/access_test.go` 添加测试：

```go
func TestResolveAccessSignsCloudThumbnailForAliyunAndCOS(t *testing.T) {
	now := time.Unix(1_760_000_000, 0).UTC()
	ossSetting := storage.NormalizeSettings(storage.Settings{
		Enabled:         true,
		Provider:        "aliyun",
		Region:          "cn-hangzhou",
		Endpoint:        "https://oss-cn-hangzhou.aliyuncs.com",
		Bucket:          "canvas-prod",
		AccessKeyID:     "ak-test",
		AccessKeySecret: "sk-test",
	})
	res := &model.Resource{
		ID:        "res-img-1",
		UserID:    "u1",
		Kind:      "image",
		MimeType:  "image/png",
		Status:    model.ResourceStatusReady,
		Provider:  "aliyun",
		ObjectKey: "open-ai-canvas/u1/demo.png",
	}
	desc, err := ResolveAccess(res, &ossSetting, AccessRequest{
		Intent:  IntentDisplay,
		Variant: VariantThumbnail,
	}, now)
	if err != nil {
		t.Fatal(err)
	}
	if desc.ActualVariant != VariantThumbnail || desc.FallbackReason != "" {
		t.Fatalf("got variant=%q fallback=%q, want thumbnail with empty fallback", desc.ActualVariant, desc.FallbackReason)
	}
	if !strings.Contains(desc.URL, "x-oss-process=") {
		t.Fatalf("expected x-oss-process in signed URL, got %s", desc.URL)
	}
}
```

同时在 `signing_test.go` 中用固定密钥计算并断言包含 `x-oss-process` 与 `response-cache-control` 时的阿里云 V1 HMAC-SHA1 `Signature` 精确值，以及 `image/svg+xml` 自动回退 `VariantOriginal`。

**Step 2: Run test to verify it fails**

Run:
```powershell
cd backend; go test ./internal/assets ./internal/storage -run "TestResolveAccessSignsCloudThumbnail|TestSignedAliyunOSSThumbnail" -v
```
Expected: FAIL（`desc.ActualVariant == "original"`，`FallbackReason == "thumbnail_not_ready"`）。

**Step 3: Write minimal implementation**

1. 在 `backend/internal/storage/objects.go` 与 `delivery.go`：
   - 新增辅助函数 `SupportsCloudThumbnail(setting Settings, mimeType string) bool`：仅对 `image/jpeg`、`image/png`、`image/webp`、`image/bmp`、`image/tiff`（排除 `svg`、`gif` 动图或未知二进制）且 `setting.Provider` 为 `aliyun`、`tencent`、`qiniu` 时返回 `true`；
   - 在 `objectURLSigning` 增加 `Thumbnail bool`：
     - `aliyun`：追加 `x-oss-process=image/resize,w_640,m_lfit/format,webp` 到 `query` 与 `CanonicalizedResource` 的 `overrides`（位列 `response-*` 之后）；
     - `tencent`：在 `signedCOSObjectURL` 的 `query` 中加入数据万象缩略图参数 `imageMogr2/thumbnail/640x/format/webp`；
     - `qiniu` / `SignCDNURL`：新增 `SignCDNThumbnailURL` 与 `SignedOriginObjectThumbnailURL`（与 `SignedOriginObjectDisplayURL` 保持相同的时间窗对齐以保证浏览器 HTTP 强缓存命中！）。
2. 在 `backend/internal/assets/access.go` 的 `ResolveAccess` 中：
   - 当 `req.Variant == VariantThumbnail` 且 `resource.Kind == "image"` 且 `setting != nil && storage.SupportsCloudThumbnail(*setting, resource.MimeType)` 时，保留 `actualVariant = VariantThumbnail`、`fallbackReason = ""`，并在 `DeliveryCDN` / `DeliverySignedObject` 分支调用缩略图签名函数；其余情况维持原回退逻辑不变。

**Step 4: Run test to verify it passes**

Run:
```powershell
cd backend; go test ./internal/storage/... ./internal/assets/... -v
```
Expected: 全部 PASS。

**Step 5: Commit**

```powershell
git add backend/internal/storage/objects.go backend/internal/storage/delivery.go backend/internal/storage/signing_test.go backend/internal/assets/access.go backend/internal/assets/access_test.go
git commit -m "perf(assets): 画布缩略图访问 - 支持OSS/COS/Kodo云端缩略图时间窗对齐直签"
```

---

## 第四阶段：P2 网关长连接池与数据库/队列扩容基线

### Task 8 (P2-1): Nginx 反向代理 `upstream keepalive` 长连接池

**Files:**
- Modify: `nginx.conf`
- Create: `scripts/check-nginx-keepalive.test.mjs` (或前端/脚本轻量断言 + Docker `nginx -t` 校验)

**Step 1: Write the failing check**

先对当前 `nginx.conf` 运行配置校验脚本或检查 `upstream` 块：
1. 当前 `nginx.conf` 直接写 `proxy_pass http://backend:8080;`，无 `upstream` 块，且普通 `/api/` 未设置 `proxy_set_header Connection "";`（Nginx `proxy_http_version 1.1` 默认发 `Connection: close`）。

新建 `web/src/services/api/__tests__/nginx-keepalive-config.test.ts`（或直接用 `bun test` 读取仓库根 `../nginx.conf` 校验关键代理契约，与项目既有静态契约测试保持一致）：

```typescript
import { describe, expect, it } from "bun:test";
import { readFileSync } from "node:fs";
import { resolve } from "node:path";

describe("nginx.conf upstream keepalive contract", () => {
  const conf = readFileSync(resolve(import.meta.dir, "../../../../../nginx.conf"), "utf8");

  it("defines an upstream block with keepalive and clears Connection header on HTTP/1.1 API locations", () => {
    expect(conf).toMatch(/upstream\s+backend_upstream\s*\{[\s\S]*?server\s+backend:8080;[\s\S]*?keepalive\s+64;/);
    expect(conf).toContain("proxy_pass http://backend_upstream;");
    // WebSocket location must still upgrade
    expect(conf).toContain("proxy_set_header Connection $connection_upgrade;");
  });
});
```

**Step 2: Run test to verify it fails**

Run:
```powershell
cd web; bun test src/services/api/__tests__/nginx-keepalive-config.test.ts
```
Expected: FAIL（缺少 `upstream backend_upstream` 与 `keepalive 64;`）。

**Step 3: Write minimal implementation**

修改 `nginx.conf`：
1. 在顶层（`map $http_upgrade $connection_upgrade` 之后）定义：
   ```nginx
   upstream backend_upstream {
       server backend:8080;
       keepalive 64;
   }
   ```
2. 将 4 处 `proxy_pass http://backend:8080;` 统一替换为 `proxy_pass http://backend_upstream;`；
3. 在普通 `location /api/` 块补齐 `proxy_set_header Connection "";`（注意：`location ~ ^/api/tasks/[^/]+/text-events$`、`location ~ ^/api/agent/runs/[^/]+/events$` 和 `system-model-test-stream` 已含 `proxy_set_header Connection "";`；`location /api/canvases/` 必须继续保留 `proxy_set_header Connection $connection_upgrade;` 以支持 `/realtime` WebSocket 升级，同时无 Upgrade 头时 `$connection_upgrade` 映射应为空串 `""` 而不是 `close`，以便普通 `/api/canvases/*` REST 请求也复用 keepalive！将 `map` 的 `default upgrade; '' "";` 修正为标准 Nginx keepalive + WebSocket 写法）。

> [!TIP]
> Nginx 官方标准 WebSocket + Upstream Keepalive 写法正是：
> ```nginx
> map $http_upgrade $connection_upgrade {
>     default upgrade;
>     ''      '';
> }
> ```
> 当无 `Upgrade` 头时 `$connection_upgrade` 为 `''`，既支持 `/realtime` WebSocket 握手，又让同前缀下的普通画布保存/读取 REST 请求自动走 `backend_upstream` 长连接池！

**Step 4: Run test to verify it passes**

Run:
```powershell
cd web; bun test src/services/api/__tests__/nginx-keepalive-config.test.ts
```
（若本机 Docker 运行中，附加执行 `docker run --rm -v "${PWD}/nginx.conf:/etc/nginx/conf.d/default.conf:ro" nginx:1.27-alpine nginx -t` 验证语法，注意宿主机无 `backend` DNS 时可用 `--add-host backend:127.0.0.1`。）
Expected: PASS。

**Step 5: Commit**

```powershell
git add nginx.conf web/src/services/api/__tests__/nginx-keepalive-config.test.ts
git commit -m "perf(deploy): Nginx网关 - 配置backend长连接池keepalive并保持WebSocket升级兼容"
```

---

### Task 9 (P2-2): 生产默认并发阈值调优与 `tasks` 活跃任务领取部分索引迁移（v94）

**Files:**
- Modify: `backend/internal/database/migrations.go`
- Modify: `backend/internal/database/migrations_test.go`
- Modify: `docker-compose.deploy.yml`
- Modify: `.env.example`
- Modify: `docs/content/docs/backend/backend-database.mdx`
- Modify: `docs/content/docs/progress/pending-test.mdx`

**Step 1: Write the failing test**

在 `backend/internal/database/migrations_test.go` 增加测试，断言迁移执行后 `CurrentSchemaVersion == 94`，且 `tasks` 表上已创建活跃任务领取部分索引 `idx_tasks_active_claim`：

```go
func TestMigrateTasksActiveClaimPartialIndex(t *testing.T) {
	db, err := Open(Config{Driver: "sqlite", DSN: "file:migration-tasks-active-claim?mode=memory&cache=shared"})
	if err != nil {
		t.Fatal(err)
	}
	if err := MigrateBaseline(db); err != nil {
		t.Fatal(err)
	}
	if CurrentSchemaVersion < 94 {
		t.Fatalf("CurrentSchemaVersion = %d, want >= 94", CurrentSchemaVersion)
	}
	var count int64
	if err := db.Raw("SELECT COUNT(1) FROM sqlite_master WHERE type = 'index' AND tbl_name = 'tasks' AND name = 'idx_tasks_active_claim'").Scan(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("idx_tasks_active_claim count = %d, want 1", count)
	}
}
```

**Step 2: Run test to verify it fails**

Run:
```powershell
cd backend; go test ./internal/database -run "TestCurrentSchemaVersionMatchesMigrationPlan|TestMigrateTasksActiveClaimPartialIndex" -v
```
Expected: FAIL（`CurrentSchemaVersion = 93, want >= 94`）。

**Step 3: Write minimal implementation**

1. 在 `backend/internal/database/migrations.go`：
   - 将 `const CurrentSchemaVersion int64 = 93` 更新为 `94`；
   - 在 `schemaMigrations` 末尾（版本 `93` 之后）追加版本 `94`：
     ```go
     {version: 94, name: "tasks_active_claim_partial_index", checksum: "sha256:tasks-active-claim-partial-index-v94-20261111", apply: func(tx *gorm.DB) error {
     	if !tx.Migrator().HasTable(&model.Task{}) {
     		return nil
     	}
     	// 仅索引 queued/running 活跃行，避免 tasks 累积百万终态行后 ClaimNextTask 退化。
     	return tx.Exec("CREATE INDEX IF NOT EXISTS idx_tasks_active_claim ON tasks (created_at, next_poll_at) WHERE status IN ('queued', 'running')").Error
     }},
     ```
     （SQLite 与 PostgreSQL 均原生支持该 `WHERE status IN ('queued', 'running')` 部分索引语法，与迁移 `v3`、`v8` 口径一致。）
2. 在 `docker-compose.deploy.yml` 与 `.env.example` 调优默认 I/O 任务并发参数（仍支持环境变量覆盖）：
   - `CANVAS_WORKER_CONCURRENCY: ${CANVAS_WORKER_CONCURRENCY:-16}`（原 `3`）
   - `CANVAS_CHANNEL_CONCURRENCY: ${CANVAS_CHANNEL_CONCURRENCY:-10}`（原 `3`）
   - `CANVAS_USER_TASK_CONCURRENCY: ${CANVAS_USER_TASK_CONCURRENCY:-4}`（原 `2`）
3. 按 `AGENTS.md` §5 与 §9 规范同步文档：
   - 在 `docs/content/docs/backend/backend-database.mdx` 同步记录 `tasks` 表活跃领取部分索引 `idx_tasks_active_claim` 及 PostgreSQL 连接池环境变量（`CANVAS_DB_MAX_OPEN_CONNS` 等）；
   - 在 `docs/content/docs/progress/pending-test.mdx` 追加本次 SaaS 多副本高可用与 6 项性能优化的待线上/预发验收清单。

**Step 4: Run test to verify it passes**

Run:
```powershell
cd backend; go test ./internal/database/... ./internal/repository -run "TestCurrentSchemaVersion|TestMigrate|TestClaim" -v
```
以及格式化新增前端测试文件（若 Task 8 新增了 `.ts` 测试）：
```powershell
cd web; ./node_modules/.bin/prettier --write src/services/api/__tests__/nginx-keepalive-config.test.ts
```
Expected: 全部 PASS。

**Step 5: Commit**

```powershell
git add backend/internal/database/migrations.go backend/internal/database/migrations_test.go docker-compose.deploy.yml .env.example docs/content/docs/backend/backend-database.mdx docs/content/docs/progress/pending-test.mdx
git commit -m "perf(tasks,db): 任务调度与数据库 - 增加活跃任务部分索引v94并调优生产默认并发阈值"
```

---

## 交付与推送前总体验证门禁（`AGENTS.md` §8）

全部 9 个 Task 完成后，在推送分支前执行：

1. **后端改动包与关键回归测试**：
   ```powershell
   cd backend; go test ./internal/outbound/... ./internal/storage/... ./internal/assets/... ./internal/platform/... ./internal/canvas/... ./internal/database/... ./internal/agent/...
   ```
2. **Node Agent 运行时池测试**：
   ```powershell
   node --test yingce-agent/server.test.mjs
   ```
3. **文档站类型检查**：
   ```powershell
   cd docs; bun run types:check
   ```
4. **推送前统一门禁（工作区干净且 rebase 后）**：
   ```powershell
   bun scripts/verify-before-push.mjs
   ```
