package runtime

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const requestLimit = 32 << 20
const lineLimit = 4 << 20

type Bridge struct {
	Model func(context.Context, map[string]json.RawMessage) (any, error)
	Tool  func(context.Context, map[string]json.RawMessage) (any, error)
	Event func(context.Context, map[string]json.RawMessage) (any, error)
}

type ProcessRequest struct {
	BridgeURL     string           `json:"bridgeURL"`
	BridgeToken   string           `json:"bridgeToken"`
	SessionJSONL  string           `json:"sessionJSONL,omitempty"`
	SessionID     string           `json:"sessionId,omitempty"`
	UserID        string           `json:"userId,omitempty"`
	CanvasID      string           `json:"canvasId,omitempty"`
	RunID         string           `json:"runId,omitempty"`
	Prompt        string           `json:"prompt"`
	SystemPrompt  string           `json:"systemPrompt"`
	EnabledSkills []map[string]any `json:"enabledSkills,omitempty"`
	Profile       map[string]any   `json:"profile,omitempty"`
	Features      map[string]any   `json:"features,omitempty"`
	Tools         []map[string]any `json:"tools"`
	Compaction    map[string]any   `json:"compaction,omitempty"`
	Permissions   map[string]any   `json:"permissions,omitempty"`
	Model         map[string]any   `json:"model"`
}

// BridgeServer is the per-run callback endpoint. The Node runtime calls it for
// model, tool, and event steps. Authorization is the bearer token; binding can
// be loopback for the embedded process or the container network for yingce-agent.
type BridgeServer struct {
	URL   string
	Token string
	Close func()
}

func Run(ctx context.Context, request ProcessRequest, bridge Bridge) error {
	release, err := acquireAgentProcess(ctx)
	if err != nil {
		return err
	}
	defer release()
	if bridge.Model == nil || bridge.Tool == nil || bridge.Event == nil {
		return errors.New("agent bridge handlers are incomplete")
	}
	runtimeDir, err := RuntimeDir()
	if err != nil {
		return err
	}
	server, err := StartBridge("127.0.0.1:0", "127.0.0.1", bridge)
	if err != nil {
		return err
	}
	defer server.Close()
	request.BridgeURL = server.URL
	request.BridgeToken = server.Token
	payload, err := json.Marshal(request)
	if err != nil {
		return fmt.Errorf("encode Agent runtime request: %w", err)
	}
	cmd, err := agentRuntimeCommand(ctx, runtimeDir)
	if err != nil {
		return err
	}
	cmd.Dir = runtimeDir
	cmd.Stdin = strings.NewReader(string(payload))
	// 环境变量白名单：只传 Node 运行所需的最少变量。HOME 与会话、工作目录
	// 由运行时自己建的临时隔离目录提供，不使用服务端数据目录。
	cmd.Env = agentRuntimeEnv()
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("open Agent runtime output: %w", err)
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start Agent runtime: %w", err)
	}
	// StdoutPipe 的读取必须在 Wait 之前结束。Wait 会关闭管道，提前调用会把
	// 正常退出误报为 "file already closed"。
	lineErr := ReadOutput(stdout)
	waitErr := cmd.Wait()
	if lineErr != nil {
		return lineErr
	}
	if waitErr != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("agent runtime exited: %w", waitErr)
	}
	return nil
}

// ResolveBridgeAdvertiseHost normalizes the callback host advertised to the
// Agent runtime. Empty defaults to loopback for embedded processes; "auto"
// selects a concrete non-loopback interface IP or hostname so multi-replica
// deployments do not advertise a round-robin service DNS alias.
func ResolveBridgeAdvertiseHost(bridgeHost string) string {
	bridgeHost = strings.TrimSpace(bridgeHost)
	if bridgeHost == "" {
		return "127.0.0.1"
	}
	if strings.EqualFold(bridgeHost, "auto") {
		return detectBridgeAdvertiseHost()
	}
	return bridgeHost
}

func detectBridgeAdvertiseHost() string {
	var fallbackIPv6 string
	if ifaces, err := net.Interfaces(); err == nil {
		for _, iface := range ifaces {
			if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
				continue
			}
			addrs, addrErr := iface.Addrs()
			if addrErr != nil {
				continue
			}
			for _, addr := range addrs {
				ip := interfaceAddrIP(addr)
				if !usableAdvertiseIP(ip) {
					continue
				}
				if ip4 := ip.To4(); ip4 != nil {
					return ip4.String()
				}
				if fallbackIPv6 == "" && ip.IsGlobalUnicast() {
					fallbackIPv6 = ip.String()
				}
			}
		}
	}
	if fallbackIPv6 != "" {
		return fallbackIPv6
	}
	if host, err := os.Hostname(); err == nil {
		host = strings.TrimSpace(host)
		if validBridgeAdvertiseHost(host) {
			return host
		}
	}
	return "127.0.0.1"
}

func interfaceAddrIP(addr net.Addr) net.IP {
	switch v := addr.(type) {
	case *net.IPNet:
		return v.IP
	case *net.IPAddr:
		return v.IP
	default:
		return nil
	}
}

func usableAdvertiseIP(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() && !ip.IsUnspecified()
}

func validBridgeAdvertiseHost(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return !ip.IsUnspecified() && !ip.IsMulticast() && !ip.IsLinkLocalMulticast()
	}
	if host[0] == '.' || host[0] == '-' || host[len(host)-1] == '.' || host[len(host)-1] == '-' {
		return false
	}
	for i := 0; i < len(host); i++ {
		if !isBridgeHostnameByte(host[i]) {
			return false
		}
	}
	return true
}

func isBridgeHostnameByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9') || b == '.' || b == '-' || b == '_'
}

func StartBridge(listenAddr, advertiseHost string, bridge Bridge) (*BridgeServer, error) {
	if bridge.Model == nil || bridge.Tool == nil || bridge.Event == nil {
		return nil, errors.New("agent bridge handlers are incomplete")
	}
	advertiseHost = strings.TrimSpace(advertiseHost)
	if strings.EqualFold(advertiseHost, "auto") {
		advertiseHost = ResolveBridgeAdvertiseHost("auto")
	}
	if !validBridgeAdvertiseHost(advertiseHost) {
		return nil, errors.New("agent bridge host must be a hostname or IP without a scheme or port")
	}
	tokenBytes := make([]byte, 32)
	if _, err := rand.Read(tokenBytes); err != nil {
		return nil, fmt.Errorf("create Agent bridge credential: %w", err)
	}
	token := hex.EncodeToString(tokenBytes)
	listener, err := net.Listen("tcp", listenAddr)
	if err != nil {
		return nil, fmt.Errorf("listen for Agent bridge: %w", err)
	}
	tcpAddr, ok := listener.Addr().(*net.TCPAddr)
	if !ok || tcpAddr.Port == 0 {
		_ = listener.Close()
		return nil, errors.New("agent bridge did not receive a TCP port")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /model", func(w http.ResponseWriter, r *http.Request) { BridgeCall(w, r, token, bridge.Model) })
	mux.HandleFunc("POST /tool", func(w http.ResponseWriter, r *http.Request) { BridgeCall(w, r, token, bridge.Tool) })
	mux.HandleFunc("POST /event", func(w http.ResponseWriter, r *http.Request) { BridgeCall(w, r, token, bridge.Event) })
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second, ReadTimeout: 2 * time.Minute}
	go func() { _ = server.Serve(listener) }()
	return &BridgeServer{
		URL:   "http://" + net.JoinHostPort(advertiseHost, strconv.Itoa(tcpAddr.Port)),
		Token: token,
		Close: func() { _ = server.Shutdown(context.Background()) },
	}, nil
}

func ReadOutput(stdout io.Reader) error {
	scanner := bufio.NewScanner(stdout)
	scanner.Buffer(make([]byte, 64*1024), lineLimit)
	var lastError error
	for scanner.Scan() {
		var event struct {
			Event   string `json:"event"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			lastError = fmt.Errorf("decode Agent runtime event: %w", err)
			continue
		}
		if event.Event == "runtime_error" || event.Event == "bridge_error" {
			lastError = errors.New(firstNonEmpty(event.Message, "Agent runtime failed"))
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read Agent runtime output: %w", err)
	}
	return lastError
}

func BridgeCall(w http.ResponseWriter, r *http.Request, expectedToken string, handler func(context.Context, map[string]json.RawMessage) (any, error)) {
	if r.Header.Get("Authorization") != "Bearer "+expectedToken {
		http.Error(w, `{"error":"unauthorized bridge"}`, http.StatusUnauthorized)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, requestLimit+1))
	if err != nil || len(body) > requestLimit {
		http.Error(w, `{"error":"bridge payload too large or unreadable"}`, http.StatusRequestEntityTooLarge)
		return
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		http.Error(w, `{"error":"invalid bridge payload"}`, http.StatusBadRequest)
		return
	}
	result, err := handler(r.Context(), payload)
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(result)
}

func RuntimeDir() (string, error) {
	candidates := []string{os.Getenv("CANVAS_PI_RUNTIME_DIR"), "/app/backend/agent-runtime/pi"}
	if _, source, _, ok := runtime.Caller(0); ok {
		candidates = append(candidates, filepath.Clean(filepath.Join(filepath.Dir(source), "../../../agent-runtime/pi")))
	}
	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(filepath.Join(candidate, "agent-runtime.mjs")); err == nil && !info.IsDir() {
			return candidate, nil
		}
	}
	return "", errors.New("agent runtime files are missing; set CANVAS_PI_RUNTIME_DIR")
}

const (
	defaultAgentProcessLimit = 30
	maxAgentProcessLimit     = 64
)

type agentProcessGate struct {
	mu     sync.Mutex
	limit  int
	active int
	wake   chan struct{}
}

var processGate = newAgentProcessGate()

func newAgentProcessGate() *agentProcessGate {
	limit := defaultAgentProcessLimit
	if raw := strings.TrimSpace(os.Getenv("CANVAS_AGENT_MAX_PROCESSES")); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil {
			limit = clampAgentProcessLimit(parsed)
		}
	}
	return &agentProcessGate{limit: limit, wake: make(chan struct{})}
}

func clampAgentProcessLimit(value int) int {
	if value < 1 {
		return defaultAgentProcessLimit
	}
	if value > maxAgentProcessLimit {
		return maxAgentProcessLimit
	}
	return value
}

// SetProcessLimit changes how many embedded Agent processes may run at once.
// Lowering the limit does not stop a process that has already started.
func SetProcessLimit(value int) (active int, limit int) {
	processGate.mu.Lock()
	defer processGate.mu.Unlock()
	processGate.limit = clampAgentProcessLimit(value)
	close(processGate.wake)
	processGate.wake = make(chan struct{})
	return processGate.active, processGate.limit
}

func ProcessUsage() (active int, limit int) {
	processGate.mu.Lock()
	defer processGate.mu.Unlock()
	return processGate.active, processGate.limit
}

func acquireAgentProcess(ctx context.Context) (func(), error) {
	for {
		processGate.mu.Lock()
		if processGate.active < processGate.limit {
			processGate.active++
			processGate.mu.Unlock()
			return releaseAgentProcess, nil
		}
		wake := processGate.wake
		processGate.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-wake:
		}
	}
}

func releaseAgentProcess() {
	processGate.mu.Lock()
	if processGate.active > 0 {
		processGate.active--
	}
	close(processGate.wake)
	processGate.wake = make(chan struct{})
	processGate.mu.Unlock()
}

func nodeMemoryMB() string {
	raw := strings.TrimSpace(os.Getenv("YINGCE_AGENT_NODE_MEMORY_MB"))
	if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 128 && parsed <= 8192 {
		return strconv.Itoa(parsed)
	}
	return "512"
}

func agentRuntimeCommand(ctx context.Context, runtimeDir string) (*exec.Cmd, error) {
	scriptPath := filepath.Join(runtimeDir, "agent-runtime.mjs")
	if nodePath, err := exec.LookPath("node"); err == nil {
		return exec.CommandContext(ctx, nodePath, "--max-old-space-size="+nodeMemoryMB(), scriptPath), nil
	}
	if bunPath, err := exec.LookPath("bun"); err == nil {
		return exec.CommandContext(ctx, bunPath, scriptPath), nil
	}
	return nil, errors.New("start Agent runtime: neither node nor bun executable found in PATH")
}

func agentRuntimeEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"NODE_ENV=production",
		"PI_OFFLINE=1",
		"NO_PROXY=127.0.0.1,localhost,::1",
		"no_proxy=127.0.0.1,localhost,::1",
	}
	if runtime.GOOS == "windows" {
		for _, key := range []string{"SystemRoot", "WINDIR", "ComSpec", "PATHEXT", "TEMP", "TMP"} {
			if val := os.Getenv(key); val != "" {
				env = append(env, key+"="+val)
			}
		}
	}
	return env
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}
