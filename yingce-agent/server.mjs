import { spawn } from "node:child_process";
import { timingSafeEqual } from "node:crypto";
import { realpathSync } from "node:fs";
import { createServer } from "node:http";
import { dirname, resolve } from "node:path";
import { fileURLToPath, pathToFileURL } from "node:url";

const noop = () => {};

export function positiveInt(raw, fallback, max) {
  if (typeof raw === "boolean") return fallback;
  const parsed = Number(raw);
  if (!Number.isInteger(parsed) || parsed < 1) return fallback;
  return Math.min(parsed, max);
}

export function nonNegativeInt(raw, fallback, max) {
  if (raw === undefined || raw === null || typeof raw === "boolean" || String(raw).trim() === "") {
    return fallback;
  }
  const parsed = Number(raw);
  if (!Number.isInteger(parsed) || parsed < 0) return fallback;
  return Math.min(parsed, max);
}

export function createWarmProcessPool({
  size,
  runtimePath,
  memoryMB,
  env = process.env,
  errorBackoffMs = 500,
  killTimeoutMs = 2000,
  onStderr = (chunk) => process.stderr.write(chunk),
  spawnFn = spawn,
} = {}) {
  const defaultSize = nonNegativeInt(env.YINGCE_AGENT_WARM_POOL_SIZE, 2, 8);
  const targetSize = nonNegativeInt(size, defaultSize, 8);
  const resolvedMemoryMB = positiveInt(
    memoryMB,
    positiveInt(env.NODE_MAX_OLD_SPACE_SIZE, 512, 8192),
    8192,
  );
  const resolvedRuntimePath =
    runtimePath || env.YINGCE_AGENT_RUNTIME || "/app/agent-runtime/agent-runtime.mjs";
  const resolvedBackoffMs = nonNegativeInt(errorBackoffMs, 500, 60_000);
  const resolvedKillTimeoutMs = positiveInt(killTimeoutMs, 2000, 30_000);

  const idle = [];
  let closed = false;
  let closePromise = null;
  let immediateHandle = null;
  let backoffTimer = null;

  function spawnRuntimeChild() {
    return spawnFn(process.execPath, [`--max-old-space-size=${resolvedMemoryMB}`, resolvedRuntimePath], {
      cwd: dirname(resolvedRuntimePath),
      env: {
        PATH: env.PATH || "",
        NODE_ENV: "production",
        PI_OFFLINE: "1",
      },
      stdio: ["pipe", "pipe", "pipe"],
    });
  }

  function isEntryLive(entry) {
    const { child, unusable } = entry;
    return (
      !unusable &&
      child.pid !== undefined &&
      child.exitCode === null &&
      child.signalCode === null &&
      !child.killed &&
      Boolean(child.stdin) &&
      !child.stdin.destroyed
    );
  }

  function discardEntry(entry) {
    entry.unusable = true;
    entry.detach();
    entry.child.on("error", noop);
    entry.child.stdin?.destroy();
    if (entry.child.exitCode === null && entry.child.signalCode === null) {
      try {
        entry.child.kill("SIGTERM");
      } catch {
        // Ignore ESRCH when child exited concurrently.
      }
    }
  }

  function scheduleBackoffReplenish() {
    if (closed || targetSize === 0 || backoffTimer !== null) return;
    if (immediateHandle !== null) {
      clearImmediate(immediateHandle);
      immediateHandle = null;
    }
    backoffTimer = setTimeout(() => {
      backoffTimer = null;
      fill();
    }, resolvedBackoffMs);
    backoffTimer.unref?.();
  }

  function scheduleImmediateReplenish() {
    if (closed || targetSize === 0 || immediateHandle !== null || backoffTimer !== null) return;
    immediateHandle = setImmediate(() => {
      immediateHandle = null;
      fill();
    });
  }

  function fill() {
    if (closed || targetSize === 0) return;
    while (!closed && idle.length < targetSize) {
      const child = spawnRuntimeChild();
      const onIdleStderr = (chunk) => {
        if (onStderr) onStderr(chunk);
      };
      const onIdleTermination = () => {
        if (entry.unusable) return;
        entry.unusable = true;
        entry.detach();
        child.on("error", noop);
        const index = idle.indexOf(entry);
        if (index >= 0) idle.splice(index, 1);
        scheduleBackoffReplenish();
      };
      const entry = {
        child,
        unusable: false,
        detach() {
          child.off("error", onIdleTermination);
          child.off("exit", onIdleTermination);
          child.off("close", onIdleTermination);
          child.stderr?.off("data", onIdleStderr);
        },
      };
      child.on("error", onIdleTermination);
      child.on("exit", onIdleTermination);
      child.on("close", onIdleTermination);
      child.stderr?.on("data", onIdleStderr);

      if (!isEntryLive(entry)) {
        discardEntry(entry);
        scheduleBackoffReplenish();
        break;
      }
      idle.push(entry);
    }
  }

  function acquire() {
    while (idle.length > 0) {
      const entry = idle.shift();
      entry.detach();
      if (isEntryLive(entry)) {
        scheduleImmediateReplenish();
        return entry.child;
      }
      discardEntry(entry);
    }
    if (!closed && targetSize > 0) {
      scheduleImmediateReplenish();
    }
    return spawnRuntimeChild();
  }

  function idleCount() {
    let count = 0;
    for (const entry of idle) {
      if (isEntryLive(entry)) count += 1;
    }
    return count;
  }

  function close() {
    if (closePromise !== null) return closePromise;
    closed = true;
    if (immediateHandle !== null) {
      clearImmediate(immediateHandle);
      immediateHandle = null;
    }
    if (backoffTimer !== null) {
      clearTimeout(backoffTimer);
      backoffTimer = null;
    }
    const drained = idle.splice(0, idle.length);
    const waitPromises = drained.map((entry) => {
      const { child } = entry;
      entry.unusable = true;
      entry.detach();
      child.on("error", noop);
      child.stdin?.destroy();
      const alreadyExited = child.exitCode !== null || child.signalCode !== null;
      if (!alreadyExited) {
        try {
          child.kill("SIGTERM");
        } catch {
          // Ignore ESRCH when child exited concurrently.
        }
      }
      if (alreadyExited) {
        return Promise.resolve();
      }
      return new Promise((resolveExit) => {
        let killTimer = null;
        const done = () => {
          if (killTimer !== null) {
            clearTimeout(killTimer);
            killTimer = null;
          }
          child.off("close", done);
          child.off("error", done);
          resolveExit();
        };
        child.once("close", done);
        child.once("error", done);
        killTimer = setTimeout(() => {
          killTimer = null;
          if (child.exitCode === null && child.signalCode === null) {
            try {
              child.kill("SIGKILL");
            } catch {
              // Ignore ESRCH when child exited concurrently.
            }
          }
          child.stdout?.destroy();
          child.stderr?.destroy();
        }, resolvedKillTimeoutMs);
        killTimer.unref?.();
      });
    });
    closePromise = Promise.all(waitPromises).then(() => undefined);
    return closePromise;
  }

  fill();

  return {
    acquire,
    idleCount,
    close,
  };
}

export function createAgentServer(options = {}) {
  const env = options.env ?? process.env;
  const token = options.token ?? env.YINGCE_AGENT_TOKEN ?? "";
  let maxSessions = positiveInt(options.maxSessions ?? env.MAX_CONCURRENT_SESSIONS, 30, 64);
  const memoryMB = positiveInt(options.memoryMB ?? env.NODE_MAX_OLD_SPACE_SIZE, 512, 8192);
  const runtimePath =
    options.runtimePath ?? env.YINGCE_AGENT_RUNTIME ?? "/app/agent-runtime/agent-runtime.mjs";
  const warmPoolSize = nonNegativeInt(
    options.warmPoolSize ?? env.YINGCE_AGENT_WARM_POOL_SIZE,
    2,
    8,
  );
  const bodyLimit = positiveInt(options.bodyLimit, 32 * 1024 * 1024, 512 * 1024 * 1024);
  const onStderr = options.onStderr ?? ((chunk) => process.stderr.write(chunk));

  const warmPool =
    options.warmPool ??
    createWarmProcessPool({
      size: warmPoolSize,
      runtimePath,
      memoryMB,
      env,
      errorBackoffMs: options.errorBackoffMs,
      killTimeoutMs: options.killTimeoutMs,
      onStderr,
      spawnFn: options.spawnFn,
    });

  let active = 0;
  const waiters = [];

  function authorized(request) {
    if (!token) return false;
    const actual = Buffer.from(request.headers.authorization || "");
    const expected = Buffer.from(`Bearer ${token}`);
    return actual.length === expected.length && timingSafeEqual(actual, expected);
  }

  function acquireSlot(response) {
    if (response.destroyed) return Promise.reject(new Error("Agent request cancelled while queued"));
    if (active < maxSessions) {
      active += 1;
      return Promise.resolve();
    }
    return new Promise((resolveSlot, rejectSlot) => {
      const waiter = {
        resolve: () => {
          response.off("close", abort);
          resolveSlot();
        },
        reject: rejectSlot,
      };
      waiters.push(waiter);
      const abort = () => {
        const index = waiters.indexOf(waiter);
        if (index >= 0) waiters.splice(index, 1);
        rejectSlot(new Error("Agent request cancelled while queued"));
      };
      // 请求体正常读完也会 destroyed；排队取消跟随尚未结束的响应连接。
      response.once("close", abort);
    });
  }

  function admitWaiters() {
    while (waiters.length > 0 && active < maxSessions) {
      active += 1;
      waiters.shift().resolve();
    }
  }

  function releaseSlot() {
    active = Math.max(0, active - 1);
    admitWaiters();
  }

  function readBody(request) {
    return new Promise((resolveBody, rejectBody) => {
      const chunks = [];
      let size = 0;
      request.on("data", (chunk) => {
        size += chunk.length;
        if (size > bodyLimit) {
          rejectBody(new Error("request body too large"));
          request.destroy();
          return;
        }
        chunks.push(chunk);
      });
      request.on("end", () => resolveBody(Buffer.concat(chunks)));
      request.on("error", rejectBody);
    });
  }

  function writeRuntimeError(response, message) {
    if (response.writableEnded || response.destroyed) return;
    if (!response.headersSent) {
      response.writeHead(200, { "content-type": "application/x-ndjson" });
    }
    response.end(`${JSON.stringify({ event: "runtime_error", message })}\n`);
  }

  const server = createServer(async (request, response) => {
    if (request.method === "GET" && request.url === "/healthz") {
      response.writeHead(200, { "content-type": "application/json" });
      response.end(
        JSON.stringify({
          ok: true,
          active,
          limit: maxSessions,
          queued: waiters.length,
          warmPoolIdle: warmPool.idleCount(),
        }),
      );
      return;
    }
    if (request.method === "PUT" && request.url === "/v1/limit") {
      if (!authorized(request)) {
        response.writeHead(401, { "content-type": "application/json" });
        response.end(JSON.stringify({ error: "unauthorized" }));
        return;
      }
      try {
        const body = JSON.parse((await readBody(request)).toString("utf8"));
        const next = positiveInt(body.maxSessions, 0, 64);
        if (!next) throw new Error("maxSessions must be an integer from 1 to 64");
        maxSessions = next;
        admitWaiters();
        response.writeHead(200, { "content-type": "application/json" });
        response.end(JSON.stringify({ ok: true, active, limit: maxSessions, queued: waiters.length }));
      } catch (error) {
        response.writeHead(400, { "content-type": "application/json" });
        response.end(JSON.stringify({ error: error instanceof Error ? error.message : "invalid limit" }));
      }
      return;
    }
    if (request.method !== "POST" || request.url !== "/v1/runs") {
      response.writeHead(404, { "content-type": "application/json" });
      response.end(JSON.stringify({ error: "not found" }));
      return;
    }
    response.on("error", noop);
    if (!authorized(request)) {
      response.writeHead(401, { "content-type": "application/json" });
      response.end(JSON.stringify({ error: "unauthorized" }));
      return;
    }
    let body;
    try {
      body = await readBody(request);
      JSON.parse(body.toString("utf8"));
    } catch (error) {
      response.writeHead(400, { "content-type": "application/json" });
      response.end(JSON.stringify({ error: error instanceof Error ? error.message : "invalid request" }));
      return;
    }
    try {
      await acquireSlot(response);
    } catch (error) {
      if (response.writableEnded || response.destroyed) return;
      response.writeHead(499, { "content-type": "application/json" });
      response.end(JSON.stringify({ error: error instanceof Error ? error.message : "cancelled" }));
      return;
    }
    if (response.writableEnded || response.destroyed) {
      releaseSlot();
      return;
    }

    const child = warmPool.acquire();
    let released = false;
    const stop = () => {
      if (child.exitCode === null && child.signalCode === null) {
        try {
          child.kill("SIGTERM");
        } catch {
          // Ignore ESRCH when child exited concurrently.
        }
      }
    };
    const finish = () => {
      if (released) return;
      released = true;
      response.off("close", stop);
      releaseSlot();
    };
    let sawRuntimeError = false;
    response.once("close", stop);
    response.writeHead(200, { "content-type": "application/x-ndjson" });
    child.stdout?.on("data", (chunk) => {
      const text = chunk.toString("utf8");
      if (text.includes('"runtime_error"') || text.includes('"bridge_error"')) sawRuntimeError = true;
      if (response.writableEnded || response.destroyed) return;
      response.write(chunk);
    });
    if (onStderr) {
      child.stderr?.on("data", onStderr);
    }
    child.on("error", (error) => {
      writeRuntimeError(response, error.message);
      finish();
    });
    child.on("close", (code, signal) => {
      if (!response.writableEnded && !response.destroyed) {
        if (!sawRuntimeError && (code || signal)) {
          const message =
            code !== null
              ? `Agent runtime exited with code ${code}`
              : `Agent runtime exited with signal ${signal}`;
          response.write(`${JSON.stringify({ event: "runtime_error", message })}\n`);
        }
        response.end();
      }
      finish();
    });
    if (child.stdin) {
      child.stdin.on("error", noop);
      child.stdin.end(body);
    }
  });

  server.requestTimeout = 0;
  server.headersTimeout = 0;
  server.timeout = 0;
  server.once("close", () => {
    void warmPool.close();
  });

  let serverClosePromise = null;
  function close() {
    if (serverClosePromise !== null) return serverClosePromise;
    const serverClosed = server.listening
      ? new Promise((resolveClose) => server.close(() => resolveClose()))
      : Promise.resolve();
    serverClosePromise = Promise.all([warmPool.close(), serverClosed]).then(() => undefined);
    return serverClosePromise;
  }

  return { server, warmPool, close };
}

function isMainModule() {
  if (!process.argv[1]) return false;
  const resolvedArg = resolve(process.argv[1]);
  if (import.meta.url === pathToFileURL(resolvedArg).href) return true;
  try {
    return realpathSync(resolvedArg) === realpathSync(fileURLToPath(import.meta.url));
  } catch {
    return false;
  }
}

if (isMainModule()) {
  const token = process.env.YINGCE_AGENT_TOKEN || "";
  if (token.length < 32) {
    console.error("YINGCE_AGENT_TOKEN must contain at least 32 characters");
    process.exit(1);
  }
  const rawPort = process.env.PORT;
  const port =
    rawPort !== undefined && typeof rawPort !== "boolean" && String(rawPort).trim() !== ""
      ? Number(rawPort)
      : 8081;
  if (!Number.isInteger(port) || port < 0 || port > 65535) {
    console.error("PORT must be an integer from 0 to 65535");
    process.exit(1);
  }
  const { server, warmPool } = createAgentServer({ env: process.env, token });
  let shuttingDown = false;
  const shutdown = () => {
    if (shuttingDown) return;
    shuttingDown = true;
    void warmPool.close();
    server.close();
  };
  process.once("SIGTERM", shutdown);
  process.once("SIGINT", shutdown);
  server.once("error", (err) => {
    console.error(err);
    void warmPool.close().finally(() => process.exit(1));
  });
  server.listen(port, "0.0.0.0", () => {
    const address = server.address();
    console.log(`YINGCE_AGENT_READY ${address.port}`);
  });
}
