package app

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	agentruntime "yingce/backend/internal/agent/runtime"
	"yingce/backend/internal/database"
	"yingce/backend/internal/model"
	"yingce/backend/internal/platform"
	"yingce/backend/internal/repository"
)

func TestCloudAgentRemoteBridgeHostAutoReplacesServiceAlias(t *testing.T) {
	var seenBridgeURL atomic.Value
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req agentruntime.ProcessRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		seenBridgeURL.Store(req.BridgeURL)
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"event":"settled"}`+"\n")
	}))
	defer agentServer.Close()

	nop := func(context.Context, map[string]json.RawMessage) (any, error) {
		return map[string]any{"ok": true}, nil
	}
	bridge := cloudAgentPiBridge{Model: nop, Tool: nop, Event: nop}
	token := strings.Repeat("k", 32)
	t.Setenv("YINGCE_AGENT_URL", agentServer.URL)
	t.Setenv("YINGCE_AGENT_TOKEN", token)

	for _, envVal := range []string{"", "backend", "BACKEND"} {
		t.Setenv("YINGCE_AGENT_BRIDGE_HOST", envVal)
		seenBridgeURL.Store("")
		err := runCloudAgentPi(context.Background(), cloudAgentPiProcessRequest{
			Model: map[string]any{"id": "text-test"},
		}, bridge)
		if err != nil {
			t.Fatalf("env=%q runCloudAgentPi failed: %v", envVal, err)
		}
		rawURL, _ := seenBridgeURL.Load().(string)
		parsed, err := url.Parse(rawURL)
		if err != nil {
			t.Fatalf("env=%q invalid bridgeURL %q: %v", envVal, rawURL, err)
		}
		host := parsed.Hostname()
		if host == "" || strings.EqualFold(host, "backend") || strings.EqualFold(host, "auto") || host == "0.0.0.0" {
			t.Fatalf("env=%q bridgeURL host = %q (url=%q), want concrete routable host", envVal, host, rawURL)
		}
	}

	t.Setenv("YINGCE_AGENT_BRIDGE_HOST", "10.88.77.66")
	if err := runCloudAgentPi(context.Background(), cloudAgentPiProcessRequest{
		Model: map[string]any{"id": "text-test"},
	}, bridge); err != nil {
		t.Fatalf("explicit bridge host failed: %v", err)
	}
	rawURL, _ := seenBridgeURL.Load().(string)
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Hostname() != "10.88.77.66" {
		t.Fatalf("explicit bridgeURL = %q, want host 10.88.77.66", rawURL)
	}
}

func TestCloudAgentDistributedLeaseBlocksDuplicateReplicaAndReleasesOnExit(t *testing.T) {
	rawDB, err := database.Open(database.Config{Driver: "sqlite", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	svcA, db, _, _ := creationTestServiceOnDatabase(t, rawDB)
	svcA.legacyCloudAgentRootTask = false
	if err := db.Create(&model.CanvasProject{ID: "agent-canvas", UserID: "user", PayloadJSON: `{"nodes":[]}`}).Error; err != nil {
		t.Fatal(err)
	}

	runView, err := svcA.CreateCloudAgentRun("user", agentTestRequest(), "")
	if err != nil {
		t.Fatalf("create cloud agent run: %v", err)
	}
	runID := runView.ID

	coord := platform.NewCoordinatorWithRedis(nil, "shared-ha-test")
	svcA.coordinator = coord
	svcA.disablePiRuntime = false

	svcB := &Service{
		repo:             repository.New(db),
		dataDir:          t.TempDir(),
		coordinator:      coord,
		piRunners:        make(map[string]context.CancelFunc),
		disablePiRuntime: false,
	}
	t.Cleanup(func() {
		svcB.closeCloudAgentPiRunners()
	})

	blockFirst := make(chan struct{})
	var unblockOnce sync.Once
	unblockFirst := func() { unblockOnce.Do(func() { close(blockFirst) }) }
	firstEntered := make(chan struct{})
	var remoteRuns atomic.Int32
	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callNum := remoteRuns.Add(1)
		if callNum == 1 {
			close(firstEntered)
			<-blockFirst
		}
		w.Header().Set("Content-Type", "application/x-ndjson")
		_, _ = io.WriteString(w, `{"event":"settled"}`+"\n")
	}))
	defer agentServer.Close()
	defer unblockFirst()

	t.Setenv("YINGCE_AGENT_URL", agentServer.URL)
	t.Setenv("YINGCE_AGENT_TOKEN", strings.Repeat("s", 32))
	t.Setenv("YINGCE_AGENT_BRIDGE_HOST", "127.0.0.1")

	// Mark run as waiting_user on exit so clean runtime exit doesn't fail on 0 assistant responses,
	// allowing us to verify clean lease lifecycle and same-replica restart ordering.
	execRow, err := svcA.repo.CloudAgent("user", runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := svcA.repo.MutateCloudAgent("user", runID, execRow.Revision, func(current *model.CloudAgentExecution, _ *repository.Repository) error {
		current.Status = "waiting_approval"
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// Start run on replica A; wait until replica A holds the lease and enters remote runtime.
	svcA.startCloudAgentPi("user", runID)
	select {
	case <-firstEntered:
	case <-time.After(5 * time.Second):
		t.Fatal("replica A did not enter remote agent runtime")
	}

	// While replica A holds the lease, direct lease acquisition and startCloudAgentPi on replica B must be rejected cleanly.
	if leaseB, acquired, err := svcB.acquireCloudAgentRunLease(context.Background(), runID); err != nil || acquired || leaseB != nil {
		if leaseB != nil {
			leaseB.Release()
		}
		t.Fatalf("replica B unexpectedly acquired active lease: acquired=%v err=%v", acquired, err)
	}

	svcB.startCloudAgentPi("user", runID)
	svcB.piRunnerWg.Wait()
	if got := remoteRuns.Load(); got != 1 {
		t.Fatalf("duplicate replica B executed runtime session: remoteRuns=%d, want 1", got)
	}
	checkRow, err := svcA.repo.CloudAgent("user", runID)
	if err != nil {
		t.Fatal(err)
	}
	if checkRow.Status == "failed" {
		t.Fatalf("replica B marked active run failed on lease contention: %s", checkRow.FailureMessage)
	}

	// Queue an approval-resume restart on replica A BEFORE releasing the first session:
	// defer must release the first lease BEFORE starting the queued restart!
	svcA.resumeCloudAgentPi("user", runID)
	unblockFirst()
	svcA.piRunnerWg.Wait()

	if got := remoteRuns.Load(); got != 2 {
		t.Fatalf("queued resume on replica A did not re-acquire lease after exit: remoteRuns=%d, want 2", got)
	}

	// Once replica A has exited completely, replica B can acquire the lease for runID.
	leaseB, acquired, err := svcB.acquireCloudAgentRunLease(context.Background(), runID)
	if err != nil || !acquired || leaseB == nil {
		t.Fatalf("replica B could not acquire lease after replica A exited: acquired=%v err=%v", acquired, err)
	}
	leaseB.Release()

	// Verify lease acquisition failure (e.g. unreachable Redis) neither marks run failed nor triggers restart loop.
	deadRDB := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", DialTimeout: 50 * time.Millisecond, MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = deadRDB.Close() })
	svcBroken := &Service{
		repo:             repository.New(db),
		dataDir:          t.TempDir(),
		coordinator:      platform.NewCoordinatorWithRedis(deadRDB, "broken-redis"),
		piRunners:        make(map[string]context.CancelFunc),
		piRunnerRestarts: map[string]struct{}{runID: {}},
		disablePiRuntime: false,
	}
	svcBroken.startCloudAgentPi("user", runID)
	svcBroken.piRunnerWg.Wait()
	if got := remoteRuns.Load(); got != 2 {
		t.Fatalf("broken lease replica executed runtime session: remoteRuns=%d, want 2", got)
	}
	afterBroken, err := svcA.repo.CloudAgent("user", runID)
	if err != nil {
		t.Fatal(err)
	}
	if afterBroken.Status == "failed" {
		t.Fatalf("lease error unexpectedly marked run failed: %s", afterBroken.FailureMessage)
	}
}

func TestCloudAgentCancelPubSubStopsRemoteReplicaRunner(t *testing.T) {
	addr := startMiniRESP2PubSubServer(t)

	rdbA := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdbA.Close() })
	rdbB := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdbB.Close() })

	svcA := &Service{
		coordinator:      platform.NewCoordinatorWithRedis(rdbA, "replica-a"),
		piRunners:        make(map[string]context.CancelFunc),
		piRunnerRestarts: map[string]struct{}{"run-remote-1": {}},
	}
	svcB := &Service{
		coordinator: platform.NewCoordinatorWithRedis(rdbB, "replica-b"),
		piRunners:   make(map[string]context.CancelFunc),
	}

	stopSub := svcA.startCloudAgentCancelSubscriber()
	defer stopSub()

	cancelled := make(chan struct{})
	var cancelOnce sync.Once
	svcA.piRunnerMu.Lock()
	svcA.piRunners["run-remote-1"] = func() {
		cancelOnce.Do(func() { close(cancelled) })
	}
	svcA.piRunnerMu.Unlock()

	svcB.stopCloudAgentPi("run-remote-1")

	select {
	case <-cancelled:
	case <-time.After(3 * time.Second):
		t.Fatal("remote replica runner was not cancelled via Redis Pub/Sub")
	}

	svcA.piRunnerMu.Lock()
	_, restartRemaining := svcA.piRunnerRestarts["run-remote-1"]
	svcA.piRunnerMu.Unlock()
	if restartRemaining {
		t.Fatal("cancelled run kept queued restart entry on remote replica")
	}

	// Verify ensureCloudAgentCancelSubscriber + Service.Close() cleans up subscriber cleanly before closing Redis.
	rdbClose := redis.NewClient(&redis.Options{Addr: addr, MaxRetries: -1, ContextTimeoutEnabled: true})
	svcClose := &Service{
		coordinator: platform.NewCoordinatorWithRedis(rdbClose, "replica-close"),
		piRunners:   make(map[string]context.CancelFunc),
	}
	svcClose.ensureCloudAgentCancelSubscriber()
	svcClose.ensureCloudAgentCancelSubscriber() // idempotent
	if err := svcClose.Close(); err != nil {
		t.Fatalf("Service.Close() with active cancel subscriber failed: %v", err)
	}
}

func TestCloudAgentLeaseLossCancelsSessionWithoutMarkingRunFailed(t *testing.T) {
	origTTL := cloudAgentRunLeaseTTL
	origRenew := cloudAgentRunLeaseRenewInterval
	t.Cleanup(func() {
		cloudAgentRunLeaseTTL = origTTL
		cloudAgentRunLeaseRenewInterval = origRenew
	})

	rawDB, err := database.Open(database.Config{Driver: "sqlite", DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open sqlite db: %v", err)
	}
	svc, db, _, _ := creationTestServiceOnDatabase(t, rawDB)
	svc.legacyCloudAgentRootTask = false
	if err := db.Create(&model.CanvasProject{ID: "agent-canvas", UserID: "user", PayloadJSON: `{"nodes":[]}`}).Error; err != nil {
		t.Fatal(err)
	}

	runView, err := svc.CreateCloudAgentRun("user", agentTestRequest(), "")
	if err != nil {
		t.Fatalf("create cloud agent run: %v", err)
	}
	runID := runView.ID

	// Subcase 1: Lease expires before renew tick (ErrSlotLeaseExpired) -> cancels active session,
	// suppresses queued restart, and does NOT mark recoverable "running" execution as "failed"
	// even if the runtime stream emits a non-context.Canceled error while shutting down.
	cloudAgentRunLeaseTTL = 40 * time.Millisecond
	cloudAgentRunLeaseRenewInterval = 65 * time.Millisecond
	svc.coordinator = platform.NewCoordinatorWithRedis(nil, "lease-expiry-test")
	svc.disablePiRuntime = false

	entered := make(chan struct{})
	var enteredOnce sync.Once
	ctxCancelled := make(chan struct{})
	var cancelledOnce sync.Once
	stopServer1 := make(chan struct{})
	var stopServer1Once sync.Once
	unblockServer1 := func() { stopServer1Once.Do(func() { close(stopServer1) }) }
	var remoteCalls atomic.Int32

	agentServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		remoteCalls.Add(1)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		enteredOnce.Do(func() { close(entered) })
		select {
		case <-r.Context().Done():
			cancelledOnce.Do(func() { close(ctxCancelled) })
		case <-stopServer1:
		}
	}))
	defer agentServer.Close()
	defer unblockServer1()

	t.Setenv("YINGCE_AGENT_URL", agentServer.URL)
	t.Setenv("YINGCE_AGENT_TOKEN", strings.Repeat("m", 32))
	t.Setenv("YINGCE_AGENT_BRIDGE_HOST", "127.0.0.1")

	svc.startCloudAgentPi("user", runID)
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("session did not enter remote agent server")
	}
	// Queue a restart while active; lease loss must suppress restarting on this replica.
	svc.resumeCloudAgentPi("user", runID)

	select {
	case <-ctxCancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("expired lease did not cancel active session context")
	}

	done := make(chan struct{})
	go func() {
		svc.piRunnerWg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runner goroutine did not exit after lease-loss cancel")
	}

	if got := remoteCalls.Load(); got != 1 {
		t.Fatalf("lease-lost runner unexpectedly restarted: remoteCalls=%d, want 1", got)
	}
	execRow, err := svc.repo.CloudAgent("user", runID)
	if err != nil {
		t.Fatal(err)
	}
	if execRow.Status == "failed" {
		t.Fatalf("lease-lost session wrongly marked recoverable run as failed: %s", execRow.FailureMessage)
	}

	// Subcase 2: Consecutive transient renew errors fence the session before TTL expiry.
	addr := startMiniRESP2PubSubServer(t)
	rdb := redis.NewClient(&redis.Options{Addr: addr, DialTimeout: 50 * time.Millisecond, ReadTimeout: 50 * time.Millisecond, WriteTimeout: 50 * time.Millisecond, MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = rdb.Close() })

	cloudAgentRunLeaseTTL = 120 * time.Millisecond
	cloudAgentRunLeaseRenewInterval = 35 * time.Millisecond
	svc.coordinator = platform.NewCoordinatorWithRedis(rdb, "transient-fence-test")

	entered2 := make(chan struct{})
	var entered2Once sync.Once
	cancelled2 := make(chan struct{})
	var cancelled2Once sync.Once
	stopServer2 := make(chan struct{})
	var stopServer2Once sync.Once
	unblockServer2 := func() { stopServer2Once.Do(func() { close(stopServer2) }) }
	agentServer2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/x-ndjson")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		entered2Once.Do(func() { close(entered2) })
		select {
		case <-r.Context().Done():
			cancelled2Once.Do(func() { close(cancelled2) })
		case <-stopServer2:
		}
	}))
	defer agentServer2.Close()
	defer unblockServer2()
	t.Setenv("YINGCE_AGENT_URL", agentServer2.URL)

	svc.startCloudAgentPi("user", runID)
	select {
	case <-entered2:
	case <-time.After(3 * time.Second):
		t.Fatal("subcase 2 session did not enter remote agent server")
	}
	// Simulate Redis network outage during active session by closing the client pool:
	// first renew tick (> renewInterval before deadline) tolerates transient error;
	// subsequent tick (<= renewInterval before deadline) fences and cancels session.
	_ = rdb.Close()

	select {
	case <-cancelled2:
	case <-time.After(2 * time.Second):
		t.Fatal("consecutive renew errors did not fence and cancel active session")
	}
	svc.piRunnerWg.Wait()

	execRow2, err := svc.repo.CloudAgent("user", runID)
	if err != nil {
		t.Fatal(err)
	}
	if execRow2.Status == "failed" {
		t.Fatalf("fenced session wrongly marked recoverable run as failed: %s", execRow2.FailureMessage)
	}
}

type resp2SubClient struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *resp2SubClient) writeRaw(payload string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := io.WriteString(c.conn, payload)
	return err
}

func startMiniRESP2PubSubServer(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen mini RESP2 server: %v", err)
	}

	var mu sync.Mutex
	var wg sync.WaitGroup
	closed := false
	subs := map[string]map[*resp2SubClient]struct{}{}
	conns := map[net.Conn]struct{}{}

	t.Cleanup(func() {
		_ = ln.Close()
		mu.Lock()
		closed = true
		for c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			if closed {
				mu.Unlock()
				_ = conn.Close()
				return
			}
			conns[conn] = struct{}{}
			wg.Add(1)
			mu.Unlock()
			go func(c net.Conn) {
				defer wg.Done()
				client := &resp2SubClient{conn: c}
				defer func() {
					_ = c.Close()
					mu.Lock()
					delete(conns, c)
					for ch, set := range subs {
						delete(set, client)
						if len(set) == 0 {
							delete(subs, ch)
						}
					}
					mu.Unlock()
				}()
				reader := bufio.NewReader(c)
				subCount := 0
				for {
					args, err := readRESP2Command(reader)
					if err != nil || len(args) == 0 {
						return
					}
					switch strings.ToUpper(args[0]) {
					case "HELLO":
						_ = client.writeRaw("-ERR unknown command\r\n")
					case "PING":
						_ = client.writeRaw("+PONG\r\n")
					case "CLIENT", "SELECT", "QUIT":
						_ = client.writeRaw("+OK\r\n")
					case "EVAL", "EVALSHA", "ZREM":
						_ = client.writeRaw(":1\r\n")
					case "SUBSCRIBE":
						for _, ch := range args[1:] {
							mu.Lock()
							if subs[ch] == nil {
								subs[ch] = map[*resp2SubClient]struct{}{}
							}
							subs[ch][client] = struct{}{}
							mu.Unlock()
							subCount++
							_ = client.writeRaw(fmt.Sprintf("*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(ch), ch, subCount))
						}
					case "PUBLISH":
						if len(args) < 3 {
							_ = client.writeRaw("-ERR wrong number of arguments\r\n")
							continue
						}
						ch, msg := args[1], args[2]
						mu.Lock()
						targets := make([]*resp2SubClient, 0, len(subs[ch]))
						for sub := range subs[ch] {
							targets = append(targets, sub)
						}
						mu.Unlock()
						frame := fmt.Sprintf("*3\r\n$7\r\nmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(ch), ch, len(msg), msg)
						delivered := 0
						for _, sub := range targets {
							if sub.writeRaw(frame) == nil {
								delivered++
							}
						}
						_ = client.writeRaw(fmt.Sprintf(":%d\r\n", delivered))
					default:
						_ = client.writeRaw("+OK\r\n")
					}
				}
			}(conn)
		}
	}()

	return ln.Addr().String()
}

func readRESP2Command(r *bufio.Reader) ([]string, error) {
	line, err := r.ReadString('\n')
	if err != nil {
		return nil, err
	}
	line = strings.TrimRight(line, "\r\n")
	if line == "" {
		return nil, nil
	}
	if line[0] != '*' {
		return strings.Fields(line), nil
	}
	count, err := strconv.Atoi(line[1:])
	if err != nil || count < 0 {
		return nil, fmt.Errorf("invalid multibulk header %q", line)
	}
	args := make([]string, 0, count)
	for i := 0; i < count; i++ {
		hdr, err := r.ReadString('\n')
		if err != nil {
			return nil, err
		}
		hdr = strings.TrimRight(hdr, "\r\n")
		if len(hdr) == 0 || hdr[0] != '$' {
			return nil, fmt.Errorf("invalid bulk header %q", hdr)
		}
		n, err := strconv.Atoi(hdr[1:])
		if err != nil || n < 0 {
			return nil, fmt.Errorf("invalid bulk length %q", hdr)
		}
		buf := make([]byte, n+2)
		if _, err := io.ReadFull(r, buf); err != nil {
			return nil, err
		}
		args = append(args, string(buf[:n]))
	}
	return args, nil
}
