import test from "node:test";
import assert from "node:assert/strict";
import { spawn } from "node:child_process";
import { existsSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import {
  createAgentServer,
  createWarmProcessPool,
  nonNegativeInt,
  positiveInt,
} from "./server.mjs";

const serverFilePath = fileURLToPath(new URL("./server.mjs", import.meta.url));

function createTestContext(t, prefix) {
  const dir = mkdtempSync(join(tmpdir(), prefix));
  const closers = [];
  const acquiredChildren = new Set();

  t.after(async () => {
    const childWaiters = [];
    for (const child of acquiredChildren) {
      child.on("error", () => {});
      child.stdin?.destroy();
      if (child.exitCode === null && child.signalCode === null) {
        try {
          child.kill("SIGKILL");
        } catch {
          // Ignore ESRCH if already exited.
        }
        childWaiters.push(
          new Promise((resolve) => {
            child.once("close", resolve);
            child.once("error", resolve);
          }),
        );
      }
      child.stdout?.destroy();
      child.stderr?.destroy();
    }
    await Promise.all(childWaiters);

    while (closers.length > 0) {
      const closeFn = closers.pop();
      await closeFn();
    }

    rmSync(dir, { recursive: true, force: true });
  });

  return {
    dir,
    trackPool(pool) {
      closers.push(() => pool.close());
      return pool;
    },
    trackServer(srv) {
      closers.push(() => srv.close());
      return srv;
    },
    acquire(pool) {
      const child = pool.acquire();
      acquiredChildren.add(child);
      return child;
    },
  };
}

function runEchoChild(child, id) {
  return new Promise((resolve, reject) => {
    let out = "";
    child.stdout.on("data", (chunk) => {
      out += chunk.toString("utf8");
    });
    child.on("error", reject);
    child.on("close", (code) => {
      if (code !== 0) {
        reject(new Error(`child exited with code ${code}: ${out}`));
        return;
      }
      try {
        resolve(JSON.parse(out.trim()));
      } catch (err) {
        reject(err);
      }
    });
    child.stdin.on("error", () => {});
    child.stdin.end(JSON.stringify({ id }));
  });
}

async function waitFor(predicate, timeoutMs = 2000, intervalMs = 15) {
  const deadline = Date.now() + timeoutMs;
  while (Date.now() < deadline) {
    if (await predicate()) return;
    await new Promise((resolve) => setTimeout(resolve, intervalMs));
  }
  assert.ok(await predicate(), `condition not met within ${timeoutMs}ms`);
}

test("positiveInt and nonNegativeInt parse integers, reject booleans, and clamp to max", () => {
  assert.equal(positiveInt(undefined, 30, 64), 30);
  assert.equal(positiveInt(null, 30, 64), 30);
  assert.equal(positiveInt("", 30, 64), 30);
  assert.equal(positiveInt(true, 30, 64), 30);
  assert.equal(positiveInt(false, 30, 64), 30);
  assert.equal(positiveInt(0, 30, 64), 30);
  assert.equal(positiveInt("16", 30, 64), 16);
  assert.equal(positiveInt("100", 30, 64), 64);

  assert.equal(nonNegativeInt(undefined, 2, 8), 2);
  assert.equal(nonNegativeInt(null, 2, 8), 2);
  assert.equal(nonNegativeInt("", 2, 8), 2);
  assert.equal(nonNegativeInt("   ", 2, 8), 2);
  assert.equal(nonNegativeInt(true, 2, 8), 2);
  assert.equal(nonNegativeInt(false, 2, 8), 2);
  assert.equal(nonNegativeInt("0", 2, 8), 0);
  assert.equal(nonNegativeInt(0, 2, 8), 0);
  assert.equal(nonNegativeInt("4", 2, 8), 4);
  assert.equal(nonNegativeInt("99", 2, 8), 8);
  assert.equal(nonNegativeInt("-1", 2, 8), 2);
  assert.equal(nonNegativeInt("1.5", 2, 8), 2);
  assert.equal(nonNegativeInt("abc", 2, 8), 2);
});

test("warm process pool pre-spawns distinct OS processes and replenishes without reuse", async (t) => {
  const ctx = createTestContext(t, "warm-pool-basic-");
  const fixture = join(ctx.dir, "echo-worker.mjs");
  writeFileSync(
    fixture,
    `let raw = "";
for await (const chunk of process.stdin) raw += chunk;
const payload = JSON.parse(raw);
process.stdout.write(JSON.stringify({ pid: process.pid, echo: payload.id }) + "\\n");
`,
  );

  const pool = ctx.trackPool(
    createWarmProcessPool({
      size: 2,
      runtimePath: fixture,
      memoryMB: 128,
    }),
  );

  await waitFor(() => pool.idleCount() === 2);
  assert.equal(pool.idleCount(), 2);

  const first = ctx.acquire(pool);
  const second = ctx.acquire(pool);
  assert.ok(first.pid && second.pid, "acquired children must have valid PIDs");
  assert.notEqual(first.pid, second.pid, "each acquire() must get a distinct OS process");

  const [res1, res2] = await Promise.all([
    runEchoChild(first, "run-1"),
    runEchoChild(second, "run-2"),
  ]);
  assert.equal(res1.pid, first.pid);
  assert.equal(res2.pid, second.pid);
  assert.equal(res1.echo, "run-1");
  assert.equal(res2.echo, "run-2");

  await waitFor(() => pool.idleCount() === 2);
  assert.equal(pool.idleCount(), 2, "pool must replenish back to target size");

  const third = ctx.acquire(pool);
  assert.notEqual(third.pid, first.pid, "replenished child must never reuse first PID");
  assert.notEqual(third.pid, second.pid, "replenished child must never reuse second PID");
  const res3 = await runEchoChild(third, "run-3");
  assert.equal(res3.echo, "run-3");
});

test("warm process pool supports size 0 disabled mode and bursts beyond pool size", async (t) => {
  const ctx = createTestContext(t, "warm-pool-burst-");
  const fixture = join(ctx.dir, "echo-worker.mjs");
  writeFileSync(
    fixture,
    `let raw = "";
for await (const chunk of process.stdin) raw += chunk;
const payload = JSON.parse(raw);
process.stdout.write(JSON.stringify({ pid: process.pid, echo: payload.id }) + "\\n");
`,
  );

  let disabledSpawns = 0;
  const disabledPool = ctx.trackPool(
    createWarmProcessPool({
      runtimePath: fixture,
      memoryMB: 128,
      env: { ...process.env, YINGCE_AGENT_WARM_POOL_SIZE: "0" },
      spawnFn: (...args) => {
        disabledSpawns += 1;
        return spawn(...args);
      },
    }),
  );

  await new Promise((resolve) => setTimeout(resolve, 60));
  assert.equal(disabledPool.idleCount(), 0);
  assert.equal(disabledSpawns, 0, "size 0 must not pre-spawn any child");

  const onDemand = ctx.acquire(disabledPool);
  assert.equal(disabledSpawns, 1, "size 0 must spawn synchronously on demand");
  const result = await runEchoChild(onDemand, "on-demand-1");
  assert.equal(result.echo, "on-demand-1");
  await new Promise((resolve) => setTimeout(resolve, 60));
  assert.equal(disabledPool.idleCount(), 0);
  await disabledPool.close();

  const burstPool = ctx.trackPool(
    createWarmProcessPool({
      size: 2,
      runtimePath: fixture,
      memoryMB: 128,
    }),
  );

  await waitFor(() => burstPool.idleCount() === 2);
  const c1 = ctx.acquire(burstPool);
  const c2 = ctx.acquire(burstPool);
  const c3 = ctx.acquire(burstPool);
  assert.equal(
    new Set([c1.pid, c2.pid, c3.pid]).size,
    3,
    "2 warm + 1 fallback burst must yield 3 distinct PIDs",
  );

  const results = await Promise.all([
    runEchoChild(c1, "b1"),
    runEchoChild(c2, "b2"),
    runEchoChild(c3, "b3"),
  ]);
  assert.deepEqual(
    results.map((item) => item.echo),
    ["b1", "b2", "b3"],
  );
  await waitFor(() => burstPool.idleCount() === 2);
});

test("warm process pool backs off on crashes, caches closePromise, and escalates to SIGKILL", async (t) => {
  const ctx = createTestContext(t, "warm-pool-crash-");
  const crashFixture = join(ctx.dir, "crash-worker.mjs");
  writeFileSync(crashFixture, `process.exit(1);\n`);

  const crashChildren = [];
  const crashPool = ctx.trackPool(
    createWarmProcessPool({
      size: 2,
      runtimePath: crashFixture,
      memoryMB: 128,
      errorBackoffMs: 180,
      onStderr: () => {},
      spawnFn: (...args) => {
        const child = spawn(...args);
        crashChildren.push(child);
        return child;
      },
    }),
  );

  await new Promise((resolve) => setTimeout(resolve, 260));
  assert.ok(crashChildren.length >= 2, "initial pool fill should attempt spawning");
  assert.ok(
    crashChildren.length <= 6,
    `expected backoff to prevent tight spawn loop, got ${crashChildren.length} spawns`,
  );
  await crashPool.close();

  const countAtClose = crashChildren.length;
  await new Promise((resolve) => setTimeout(resolve, 220));
  assert.equal(crashChildren.length, countAtClose, "no further spawns may occur after close()");

  const missingPath = join(ctx.dir, "missing-subdir", "nonexistent-runtime.mjs");
  const missingSpawns = [];
  const missingPool = ctx.trackPool(
    createWarmProcessPool({
      size: 2,
      runtimePath: missingPath,
      memoryMB: 128,
      errorBackoffMs: 180,
      onStderr: () => {},
      spawnFn: (...args) => {
        const child = spawn(...args);
        missingSpawns.push(child);
        return child;
      },
    }),
  );
  await new Promise((resolve) => setTimeout(resolve, 240));
  assert.ok(missingSpawns.length >= 1 && missingSpawns.length <= 4);
  await missingPool.close();

  // Stubborn worker traps SIGTERM and holds an active interval; killTimeoutMs must escalate to SIGKILL.
  const stubbornFixture = join(ctx.dir, "stubborn-worker.mjs");
  writeFileSync(
    stubbornFixture,
    `process.on("SIGTERM", () => {});
setInterval(() => {}, 10_000);
process.stdout.write("ready\\n");
`,
  );

  const stubbornChildren = [];
  const stubbornPool = ctx.trackPool(
    createWarmProcessPool({
      size: 2,
      runtimePath: stubbornFixture,
      memoryMB: 128,
      killTimeoutMs: 100,
      spawnFn: (...args) => {
        const child = spawn(...args);
        stubbornChildren.push(child);
        return child;
      },
    }),
  );

  await waitFor(() => stubbornPool.idleCount() === 2);
  // Wait until both workers have installed their SIGTERM trap
  await Promise.all(
    stubbornChildren.map(
      (child) =>
        new Promise((resolve) => {
          if (child.stdout.readableLength > 0) {
            resolve();
            return;
          }
          child.stdout.once("readable", resolve);
        }),
    ),
  );

  const p1 = stubbornPool.close();
  const p2 = stubbornPool.close();
  assert.strictEqual(p1, p2, "close() must return the exact cached closePromise");
  await p1;
  assert.equal(stubbornPool.idleCount(), 0);
  for (const child of stubbornChildren) {
    assert.ok(
      child.exitCode !== null || child.signalCode !== null,
      "close() with killTimeoutMs must terminate SIGTERM-ignoring idle children",
    );
  }
});

test("HTTP server streams /v1/runs, rejects boolean /v1/limit, and handles signal/abort safely", async (t) => {
  const ctx = createTestContext(t, "warm-pool-http-");
  const runtimeFixture = join(ctx.dir, "ndjson-worker.mjs");
  writeFileSync(
    runtimeFixture,
    `let raw = "";
for await (const chunk of process.stdin) raw += chunk;
const req = JSON.parse(raw);
process.stdout.write(JSON.stringify({ event: "started", pid: process.pid, session: req.session }) + "\\n");
process.stdout.write(JSON.stringify({ event: "settled", pid: process.pid, session: req.session }) + "\\n");
`,
  );

  const token = "t".repeat(32);
  const agentSrv = ctx.trackServer(
    createAgentServer({
      token,
      runtimePath: runtimeFixture,
      memoryMB: 128,
      maxSessions: 4,
      warmPoolSize: 2,
    }),
  );
  const { server, warmPool } = agentSrv;

  await new Promise((resolve, reject) => {
    server.once("error", reject);
    server.listen(0, "127.0.0.1", resolve);
  });
  const baseURL = `http://127.0.0.1:${server.address().port}`;

  await waitFor(() => warmPool.idleCount() === 2);

  const healthRes = await fetch(`${baseURL}/healthz`);
  assert.equal(healthRes.status, 200);
  assert.deepEqual(await healthRes.json(), {
    ok: true,
    active: 0,
    limit: 4,
    queued: 0,
    warmPoolIdle: 2,
  });

  // Boolean maxSessions must be rejected with 400
  const badLimitRes = await fetch(`${baseURL}/v1/limit`, {
    method: "PUT",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ maxSessions: true }),
  });
  assert.equal(badLimitRes.status, 400);

  const invokeRun = async (session) => {
    const response = await fetch(`${baseURL}/v1/runs`, {
      method: "POST",
      headers: {
        authorization: `Bearer ${token}`,
        "content-type": "application/json",
      },
      body: JSON.stringify({ session }),
    });
    assert.equal(response.status, 200);
    assert.match(response.headers.get("content-type") || "", /application\/x-ndjson/);
    const text = await response.text();
    return text
      .trim()
      .split("\n")
      .map((line) => JSON.parse(line));
  };

  const events1 = await invokeRun("session-a");
  const events2 = await invokeRun("session-b");

  assert.equal(events1.length, 2);
  assert.equal(events2.length, 2);
  assert.equal(events1[0].session, "session-a");
  assert.equal(events1[1].event, "settled");
  assert.equal(events2[0].session, "session-b");
  assert.equal(events2[1].event, "settled");
  assert.notEqual(
    events1[0].pid,
    events2[0].pid,
    "sequential /v1/runs must never reuse a child process across sessions",
  );

  await waitFor(() => warmPool.idleCount() === 2);
  const afterHealth = await (await fetch(`${baseURL}/healthz`)).json();
  assert.equal(afterHealth.active, 0);
  assert.equal(afterHealth.warmPoolIdle, 2);
  await agentSrv.close();

  // Early non-zero exit with large stdin payload (no EPIPE)
  const earlyExitFixture = join(ctx.dir, "early-exit-worker.mjs");
  writeFileSync(earlyExitFixture, `process.exit(2);\n`);
  const earlyServer = ctx.trackServer(
    createAgentServer({
      token,
      runtimePath: earlyExitFixture,
      memoryMB: 128,
      warmPoolSize: 0,
      onStderr: () => {},
    }),
  );
  await new Promise((resolve, reject) => {
    earlyServer.server.once("error", reject);
    earlyServer.server.listen(0, "127.0.0.1", resolve);
  });
  const earlyURL = `http://127.0.0.1:${earlyServer.server.address().port}`;
  const earlyRes = await fetch(`${earlyURL}/v1/runs`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ padding: "x".repeat(128 * 1024) }),
  });
  assert.equal(earlyRes.status, 200);
  const errEvents = (await earlyRes.text())
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.equal(errEvents[0].event, "runtime_error");
  assert.match(errEvents[0].message, /code 2/);
  await earlyServer.close();

  // Signal exit (code === null, signal === "SIGKILL") emits runtime_error with signal
  const hangAfterStartFixture = join(ctx.dir, "signal-worker.mjs");
  writeFileSync(
    hangAfterStartFixture,
    `let raw = "";
for await (const chunk of process.stdin) raw += chunk;
process.stdout.write(JSON.stringify({ event: "started" }) + "\\n");
setInterval(() => {}, 10_000);
`,
  );
  const signalServer = ctx.trackServer(
    createAgentServer({
      token,
      runtimePath: hangAfterStartFixture,
      memoryMB: 128,
      warmPoolSize: 0,
      onStderr: () => {},
      spawnFn: (...args) => {
        const child = spawn(...args);
        child.stdout.once("data", () => {
          setTimeout(() => {
            try {
              child.kill("SIGKILL");
            } catch {}
          }, 20);
        });
        return child;
      },
    }),
  );
  await new Promise((resolve, reject) => {
    signalServer.server.once("error", reject);
    signalServer.server.listen(0, "127.0.0.1", resolve);
  });
  const sigURL = `http://127.0.0.1:${signalServer.server.address().port}`;
  const sigRes = await fetch(`${sigURL}/v1/runs`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ session: "sig-1" }),
  });
  assert.equal(sigRes.status, 200);
  const sigEvents = (await sigRes.text())
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line));
  assert.equal(sigEvents.length, 2);
  assert.equal(sigEvents[0].event, "started");
  assert.equal(sigEvents[1].event, "runtime_error");
  assert.match(sigEvents[1].message, /signal SIGKILL/);
  const sigHealth = await (await fetch(`${sigURL}/healthz`)).json();
  assert.equal(sigHealth.active, 0);
  await signalServer.close();

  // Client abort mid-stream while worker emits late stdout + exits non-zero must not throw ERR_STREAM_WRITE_AFTER_END
  const lateWriteFixture = join(ctx.dir, "late-write-worker.mjs");
  writeFileSync(
    lateWriteFixture,
    `process.on("SIGTERM", () => {
  process.stdout.write(JSON.stringify({ event: "late_chunk" }) + "\\n");
  setTimeout(() => process.exit(3), 20);
});
let raw = "";
for await (const chunk of process.stdin) raw += chunk;
process.stdout.write(JSON.stringify({ event: "started" }) + "\\n");
setInterval(() => {}, 10_000);
`,
  );
  const abortServer = ctx.trackServer(
    createAgentServer({
      token,
      runtimePath: lateWriteFixture,
      memoryMB: 128,
      warmPoolSize: 0,
      onStderr: () => {},
    }),
  );
  await new Promise((resolve, reject) => {
    abortServer.server.once("error", reject);
    abortServer.server.listen(0, "127.0.0.1", resolve);
  });
  const abortURL = `http://127.0.0.1:${abortServer.server.address().port}`;
  const controller = new AbortController();
  const abortRes = await fetch(`${abortURL}/v1/runs`, {
    method: "POST",
    headers: {
      authorization: `Bearer ${token}`,
      "content-type": "application/json",
    },
    body: JSON.stringify({ session: "abort-1" }),
    signal: controller.signal,
  });
  const reader = abortRes.body.getReader();
  const firstChunk = await reader.read();
  assert.ok(!firstChunk.done);
  controller.abort();
  await waitFor(async () => {
    const h = await (await fetch(`${abortURL}/healthz`)).json();
    return h.active === 0;
  });
});

test("CLI main mode validates PORT before spawning warm pool", async (t) => {
  const ctx = createTestContext(t, "warm-pool-cli-port-");
  const markerFile = join(ctx.dir, "spawned.marker");
  const markerWorker = join(ctx.dir, "marker-worker.mjs");
  writeFileSync(
    markerWorker,
    `import { writeFileSync } from "node:fs";
writeFileSync(${JSON.stringify(markerFile)}, "spawned");
`,
  );

  const exitCode = await new Promise((resolve, reject) => {
    const child = spawn(process.execPath, [serverFilePath], {
      cwd: dirname(serverFilePath),
      env: {
        ...process.env,
        YINGCE_AGENT_TOKEN: "x".repeat(32),
        YINGCE_AGENT_RUNTIME: markerWorker,
        YINGCE_AGENT_WARM_POOL_SIZE: "2",
        PORT: "99999",
      },
      stdio: ["ignore", "pipe", "pipe"],
    });
    child.on("error", reject);
    child.on("close", (code) => resolve(code));
  });
  assert.equal(exitCode, 1);
  assert.equal(existsSync(markerFile), false, "invalid PORT must exit before spawning warm pool");
});
