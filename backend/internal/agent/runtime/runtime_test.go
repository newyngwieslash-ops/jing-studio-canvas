package runtime

import (
	"context"
	"encoding/json"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRuntime 用一个假的 agent-runtime.mjs 替换真实运行时，只验证 Go 侧的进程与输出处理。
func fakeRuntime(t *testing.T, script string) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		if _, bunErr := exec.LookPath("bun"); bunErr != nil {
			t.Skip("neither node nor bun is installed")
		}
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent-runtime.mjs"), []byte(script), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CANVAS_PI_RUNTIME_DIR", dir)
}

func TestSetProcessLimitDoesNotStopActiveWork(t *testing.T) {
	t.Cleanup(func() { SetProcessLimit(defaultAgentProcessLimit) })
	release, err := acquireAgentProcess(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	active, limit := SetProcessLimit(1)
	if active != 1 || limit != 1 {
		t.Fatalf("active=%d limit=%d, want active 1 and limit 1", active, limit)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := acquireAgentProcess(ctx); err == nil {
		t.Fatal("lowered limit still admitted another process")
	}
	release()
	SetProcessLimit(defaultAgentProcessLimit)
}

func noopBridge() Bridge {
	handler := func(context.Context, map[string]json.RawMessage) (any, error) { return map[string]any{"ok": true}, nil }
	return Bridge{Model: handler, Tool: handler, Event: handler}
}

// 回归：Wait 早于读完 stdout 时，大量输出后正常退出会被判成
// "read |0: file already closed"。
func TestRunDrainsStdoutBeforeWait(t *testing.T) {
	fakeRuntime(t, `
const line = JSON.stringify({ event: "progress", pad: "x".repeat(2048) }) + "\n";
for (let i = 0; i < 2000; i++) process.stdout.write(line);
process.stdout.write(JSON.stringify({ event: "settled" }) + "\n");
`)
	for i := 0; i < 5; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		err := Run(ctx, ProcessRequest{Model: map[string]any{"id": "m"}}, noopBridge())
		cancel()
		if err != nil {
			t.Fatalf("attempt %d: normal exit reported as failure: %v", i, err)
		}
	}
}

// runtime_error 之后仍要读到 EOF，并把该错误作为运行结果返回。
func TestRunReportsRuntimeErrorAfterDrainingOutput(t *testing.T) {
	fakeRuntime(t, `
process.stdout.write(JSON.stringify({ event: "runtime_error", message: "boom" }) + "\n");
const line = JSON.stringify({ event: "progress", pad: "x".repeat(2048) }) + "\n";
for (let i = 0; i < 500; i++) process.stdout.write(line);
process.exitCode = 1;
`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := Run(ctx, ProcessRequest{Model: map[string]any{"id": "m"}}, noopBridge())
	if err == nil || err.Error() != "boom" {
		t.Fatalf("want runtime_error message, got %v", err)
	}
}

// 子进程只能拿到白名单环境变量，拿不到数据库连接等密钥。
func TestRunDoesNotLeakBackendEnvironment(t *testing.T) {
	t.Setenv("DATABASE_URL", "postgres://secret")
	fakeRuntime(t, `
if (process.env.DATABASE_URL) {
  process.stdout.write(JSON.stringify({ event: "runtime_error", message: "leaked" }) + "\n");
  process.exitCode = 1;
}
`)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := Run(ctx, ProcessRequest{Model: map[string]any{"id": "m"}}, noopBridge()); err != nil {
		if strings.Contains(err.Error(), "leaked") {
			t.Fatal("backend environment leaked into the Agent runtime")
		}
		t.Fatal(err)
	}
}

func TestResolveBridgeAdvertiseHostPrefersExplicitAndAutoDetects(t *testing.T) {
	if got := ResolveBridgeAdvertiseHost(""); got != "127.0.0.1" {
		t.Fatalf("empty host = %q, want 127.0.0.1", got)
	}
	if got := ResolveBridgeAdvertiseHost("   "); got != "127.0.0.1" {
		t.Fatalf("whitespace host = %q, want 127.0.0.1", got)
	}
	if got := ResolveBridgeAdvertiseHost("10.24.36.48"); got != "10.24.36.48" {
		t.Fatalf("explicit IPv4 = %q, want 10.24.36.48", got)
	}
	if got := ResolveBridgeAdvertiseHost("replica-1.internal"); got != "replica-1.internal" {
		t.Fatalf("explicit hostname = %q, want replica-1.internal", got)
	}

	autoHost := ResolveBridgeAdvertiseHost("auto")
	if autoHost == "" || strings.EqualFold(autoHost, "auto") || strings.EqualFold(autoHost, "backend") || autoHost == "0.0.0.0" || autoHost == "::" {
		t.Fatalf("auto host resolved to unusable value %q", autoHost)
	}
	if got := ResolveBridgeAdvertiseHost("AUTO"); got != autoHost {
		t.Fatalf("case-insensitive AUTO = %q, want %q", got, autoHost)
	}

	server, err := StartBridge("127.0.0.1:0", "auto", noopBridge())
	if err != nil {
		t.Fatalf("StartBridge(auto) failed: %v", err)
	}
	defer server.Close()
	parsed, err := url.Parse(server.URL)
	if err != nil {
		t.Fatalf("parse bridge URL %q: %v", server.URL, err)
	}
	if parsed.Scheme != "http" || parsed.Hostname() == "" || strings.EqualFold(parsed.Hostname(), "auto") || parsed.Port() == "" {
		t.Fatalf("unexpected auto bridge URL: %q", server.URL)
	}

	v6Server, err := StartBridge("127.0.0.1:0", "2001:db8::1", noopBridge())
	if err != nil {
		t.Fatalf("StartBridge(IPv6) failed: %v", err)
	}
	defer v6Server.Close()
	if !strings.HasPrefix(v6Server.URL, "http://[2001:db8::1]:") {
		t.Fatalf("IPv6 bridge URL = %q, want bracketed host:port", v6Server.URL)
	}

	for _, invalid := range []string{
		"",
		"0.0.0.0",
		"::",
		"224.0.0.1",
		"fe80::1%eth0",
		"http://bad:8080",
		"bad:8080",
		"bad/path",
		"bad host",
		"bad?query",
		"bad#frag",
		"user@host",
		"[::1]",
		"bad\thost",
		".leading-dot",
		"trailing-dot.",
		"-leading-dash",
		"trailing-dash-",
	} {
		if badServer, err := StartBridge("127.0.0.1:0", invalid, noopBridge()); err == nil {
			badServer.Close()
			t.Fatalf("StartBridge(%q) succeeded unexpectedly", invalid)
		}
	}
}
