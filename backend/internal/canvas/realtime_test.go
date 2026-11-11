package canvas

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func waitPresenceEvent(t *testing.T, session *RealtimeSession, match func(RealtimeEvent) bool) RealtimeEvent {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-session.Events:
			if match(event) {
				return event
			}
		case <-timer.C:
			t.Fatal("realtime event not delivered")
			return RealtimeEvent{}
		}
	}
}

func TestCanvasRealtimePresenceAndScopeIsolation(t *testing.T) {
	hub := NewRealtimeHub(nil)
	a, _, err := hub.Open(context.Background(), "user", "canvas")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, peers, err := hub.Open(context.Background(), "user", "canvas")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	foreign, _, _ := hub.Open(context.Background(), "other", "canvas")
	defer foreign.Close()
	other, _, _ := hub.Open(context.Background(), "user", "other")
	defer other.Close()
	if len(peers) != 2 || a.ID() == b.ID() {
		t.Fatalf("unexpected roster: %+v", peers)
	}
	if err := a.Update(&PresencePoint{X: 12, Y: -3}, []PresenceNode{{ID: "node", PresencePoint: PresencePoint{X: 40, Y: 60}, Width: 240, Height: 160}}); err != nil {
		t.Fatal(err)
	}
	event := waitPresenceEvent(t, b, func(e RealtimeEvent) bool { return e.Peer != nil && e.Peer.ID == a.ID() && e.Peer.Cursor != nil })
	if event.Peer.Cursor.X != 12 || len(event.Peer.Drag) != 1 {
		t.Fatalf("lost presence: %+v", event)
	}
	if err := a.Update(&PresencePoint{X: math.NaN()}, nil); err == nil {
		t.Fatal("accepted non-finite position")
	}
	hub.Changed("user", "canvas")
	waitPresenceEvent(t, b, func(e RealtimeEvent) bool { return e.Type == "invalidate" })
	for _, isolated := range []*RealtimeSession{foreign, other} {
		for len(isolated.Events) > 0 {
			if event := <-isolated.Events; event.Type == "invalidate" || (event.Peer != nil && event.Peer.ID == a.ID()) {
				t.Fatal("cross-scope presence leak")
			}
		}
	}
	a.Close()
	waitPresenceEvent(t, b, func(e RealtimeEvent) bool { return e.Type == "leave" && e.PeerID == a.ID() })
}

func TestCanvasRealtimeAcrossRedisInstances(t *testing.T) {
	binary, err := exec.LookPath("redis-server")
	if err != nil {
		t.Skip("redis-server unavailable")
	}
	dir, err := os.MkdirTemp("", "canvas-presence-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "r.sock")
	cmd := exec.Command(binary, "--port", "0", "--unixsocket", socket, "--unixsocketperm", "700", "--save", "", "--appendonly", "no", "--dir", dir)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	client := redis.NewClient(&redis.Options{Network: "unix", Addr: socket, MaxRetries: -1, ContextTimeoutEnabled: true})
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for client.Ping(ctx).Err() != nil {
		if ctx.Err() != nil {
			t.Fatal("temporary Redis did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	hubA, hubB := NewRealtimeHub(client), NewRealtimeHub(client)
	a, _, err := hubA.Open(ctx, "user", "canvas")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()
	b, _, err := hubB.Open(ctx, "user", "canvas")
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if a.Snapshot().Number != 1 || b.Snapshot().Number != 2 {
		t.Fatalf("instances assigned inconsistent device numbers: %d / %d", a.Snapshot().Number, b.Snapshot().Number)
	}
	waitPresenceEvent(t, b, func(e RealtimeEvent) bool { return e.Peer != nil && e.Peer.ID == a.ID() })
	if err := a.Update(&PresencePoint{X: 88, Y: 99}, nil); err != nil {
		t.Fatal(err)
	}
	waitPresenceEvent(t, b, func(e RealtimeEvent) bool { return e.Peer != nil && e.Peer.Cursor != nil && e.Peer.Cursor.X == 88 })
	hubA.Changed("user", "canvas")
	waitPresenceEvent(t, b, func(e RealtimeEvent) bool { return e.Type == "invalidate" })
	a.Close()
	waitPresenceEvent(t, b, func(e RealtimeEvent) bool { return e.Type == "leave" && e.PeerID == a.ID() })
	c, _, err := hubA.Open(ctx, "user", "canvas")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if c.Snapshot().Number != 1 || b.Snapshot().Number != 2 {
		t.Fatal("released number was not reused without renumbering another device")
	}
	if err := client.ZAdd(ctx, b.room+":devices", redis.Z{Score: 0, Member: b.deviceMember()}).Err(); err != nil {
		t.Fatal(err)
	}
	b.Heartbeat()
	select {
	case <-b.Done():
	default:
		t.Fatal("expired device number retained its connection")
	}
}

func TestCanvasRealtimeDeviceNumbersStayConsistentAndReuseDepartedSlots(t *testing.T) {
	hub := NewRealtimeHub(nil)
	open := func() (*RealtimeSession, []CanvasPresence) {
		t.Helper()
		s, peers, err := hub.Open(context.Background(), "user", "canvas")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(s.Close)
		return s, peers
	}
	a, _ := open()
	b, roster := open()
	c, _ := open()
	if a.Snapshot().Number != 1 || b.Snapshot().Number != 2 || c.Snapshot().Number != 3 {
		t.Fatal("device numbers are not unique")
	}
	for _, peer := range roster {
		if peer.ID == a.ID() && peer.Number != 1 || peer.ID == b.ID() && peer.Number != 2 {
			t.Fatalf("view-dependent numbering: %+v", roster)
		}
	}
	b.Close()
	d, _ := open()
	if d.Snapshot().Number != 2 || c.Snapshot().Number != 3 {
		t.Fatal("joining device changed existing numbers")
	}
	if err := c.Update(&PresencePoint{X: 30, Y: 40}, nil); err != nil {
		t.Fatal(err)
	}
	event := waitPresenceEvent(t, a, func(e RealtimeEvent) bool { return e.Peer != nil && e.Peer.ID == c.ID() && e.Peer.Cursor != nil })
	if event.Peer.Number != 3 {
		t.Fatal("broadcast changed device identity")
	}
}

func TestCanvasRealtimeSharesSingleRedisSubscriptionPerRoom(t *testing.T) {
	srv := startRealtimeRESP2Server(t)
	clientA := redis.NewClient(&redis.Options{Addr: srv.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	clientB := redis.NewClient(&redis.Options{Addr: srv.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = clientB.Close()
	})

	hubA := NewRealtimeHub(clientA)
	defer hubA.Close()
	hubB := NewRealtimeHub(clientB)
	defer hubB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	const tabCount = 10
	sessionsA := make([]*RealtimeSession, tabCount)
	errs := make(chan error, tabCount)
	var wg sync.WaitGroup
	for i := range tabCount {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			s, _, err := hubA.Open(ctx, "user-mux", "canvas-mux")
			if err != nil {
				errs <- err
				return
			}
			sessionsA[idx] = s
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Open on hubA failed: %v", err)
	}
	defer func() {
		for _, s := range sessionsA {
			if s != nil {
				s.Close()
			}
		}
	}()

	room := realtimeRoom("user-mux", "canvas-mux")
	if got := srv.ActiveRoomSubscribers(room); got != 1 {
		t.Fatalf("hubA active SUBSCRIBE count for 10 tabs = %d, want 1", got)
	}
	if total := srv.TotalRoomSubscribes(room); total != 1 {
		t.Fatalf("hubA total SUBSCRIBE commands for 10 tabs = %d, want 1", total)
	}

	seenNumbers := map[int]bool{}
	for _, s := range sessionsA {
		num := s.Snapshot().Number
		if num < 1 || num > tabCount || seenNumbers[num] {
			t.Fatalf("unexpected or duplicate device number %d (seen=%v)", num, seenNumbers)
		}
		seenNumbers[num] = true
	}

	// Open a session on hubB in the same room and verify cross-instance hello roster exchange.
	sessionB, _, err := hubB.Open(ctx, "user-mux", "canvas-mux")
	if err != nil {
		t.Fatalf("hubB Open failed: %v", err)
	}
	defer sessionB.Close()

	if got := srv.ActiveRoomSubscribers(room); got != 2 {
		t.Fatalf("active SUBSCRIBE count across hubA + hubB = %d, want 2", got)
	}

	// hubB should receive peer announcements for all 10 sessions on hubA in response to hello.
	seenPeersOnB := map[string]bool{}
	timer := time.NewTimer(3 * time.Second)
	for len(seenPeersOnB) < tabCount {
		select {
		case ev := <-sessionB.Events:
			if ev.Type == "peer" && ev.Peer != nil && ev.Peer.ID != sessionB.ID() {
				seenPeersOnB[ev.Peer.ID] = true
			}
		case <-timer.C:
			timer.Stop()
			t.Fatalf("hubB only received %d/%d peers from hubA hello roster", len(seenPeersOnB), tabCount)
		}
	}
	timer.Stop()

	// Verify Update cursor broadcast from hubA -> hubB and hubB -> all hubA sessions.
	if err := sessionsA[0].Update(&PresencePoint{X: 123, Y: 456}, nil); err != nil {
		t.Fatal(err)
	}
	waitPresenceEvent(t, sessionB, func(e RealtimeEvent) bool {
		return e.Type == "peer" && e.Peer != nil && e.Peer.ID == sessionsA[0].ID() && e.Peer.Cursor != nil && e.Peer.Cursor.X == 123
	})

	if err := sessionB.Update(&PresencePoint{X: 77, Y: 88}, nil); err != nil {
		t.Fatal(err)
	}
	for _, s := range sessionsA {
		waitPresenceEvent(t, s, func(e RealtimeEvent) bool {
			return e.Type == "peer" && e.Peer != nil && e.Peer.ID == sessionB.ID() && e.Peer.Cursor != nil && e.Peer.Cursor.X == 77
		})
	}

	// Verify Changed invalidate broadcast from hubB -> hubA.
	hubB.Changed("user-mux", "canvas-mux")
	waitPresenceEvent(t, sessionsA[3], func(e RealtimeEvent) bool { return e.Type == "invalidate" })

	// Verify Close leave delivery and subscription count staying at 1 while 9 tabs remain on hubA.
	departedID := sessionsA[0].ID()
	sessionsA[0].Close()
	waitPresenceEvent(t, sessionB, func(e RealtimeEvent) bool { return e.Type == "leave" && e.PeerID == departedID })
	if got := srv.ActiveRoomSubscribers(room); got != 2 {
		t.Fatalf("active SUBSCRIBE count after closing 1 of 10 hubA tabs = %d, want 2", got)
	}

	// Close remaining sessions on hubA and hubB; verify active subscriptions drop to 0.
	for _, s := range sessionsA[1:] {
		s.Close()
	}
	sessionB.Close()

	deadline := time.Now().Add(time.Second)
	for srv.ActiveRoomSubscribers(room) > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := srv.ActiveRoomSubscribers(room); got != 0 {
		t.Fatalf("active SUBSCRIBE count after all sessions closed = %d, want 0", got)
	}
}

func TestCanvasRealtimeStaleRelayDisconnectDoesNotCancelNewerGeneration(t *testing.T) {
	srv := startRealtimeRESP2Server(t)
	clientA := redis.NewClient(&redis.Options{Addr: srv.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	clientB := redis.NewClient(&redis.Options{Addr: srv.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() {
		_ = clientA.Close()
		_ = clientB.Close()
	})

	hubA := NewRealtimeHub(clientA)
	defer hubA.Close()
	hubB := NewRealtimeHub(clientB)
	defer hubB.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	s1, _, err := hubA.Open(ctx, "user-gen", "canvas-gen")
	if err != nil {
		t.Fatalf("s1 Open failed: %v", err)
	}
	staleRelay := s1.relay
	s1.Close()

	s2, _, err := hubA.Open(ctx, "user-gen", "canvas-gen")
	if err != nil {
		t.Fatalf("s2 Open failed: %v", err)
	}
	defer s2.Close()
	if s2.relay == staleRelay {
		t.Fatal("expected s2 to bind to a fresh roomRelay generation")
	}

	// Simulate late disconnect callback from staleRelay arriving after s2 opened on the new relay.
	hubA.handleRelayDisconnect(staleRelay, nil)

	select {
	case <-s2.Done():
		t.Fatal("stale relay disconnect must not cancel sessions bound to a newer roomRelay")
	default:
	}

	// Verify s2 still receives cross-instance events over its active relay.
	peerB, _, err := hubB.Open(ctx, "user-gen", "canvas-gen")
	if err != nil {
		t.Fatalf("hubB Open failed: %v", err)
	}
	defer peerB.Close()
	if err := peerB.Update(&PresencePoint{X: 42, Y: 24}, nil); err != nil {
		t.Fatal(err)
	}
	waitPresenceEvent(t, s2, func(e RealtimeEvent) bool {
		return e.Type == "peer" && e.Peer != nil && e.Peer.ID == peerB.ID() && e.Peer.Cursor != nil && e.Peer.Cursor.X == 42
	})
}

func TestCanvasRealtimeDisconnectDuringClaimDoesNotOrphanSession(t *testing.T) {
	srv := startRealtimeRESP2Server(t)
	client := redis.NewClient(&redis.Options{Addr: srv.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() { _ = client.Close() })

	hub := NewRealtimeHub(client)
	defer hub.Close()

	room := realtimeRoom("user-orphan", "canvas-orphan")
	srv.SetBeforeEvalHook(func() {
		srv.DropRoomSubscribers(room)
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			hub.mu.Lock()
			gone := hub.relays[room] == nil
			hub.mu.Unlock()
			if gone {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
	})

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s, _, err := hub.Open(ctx, "user-orphan", "canvas-orphan")
	srv.SetBeforeEvalHook(nil)
	if err == nil {
		s.Close()
		t.Fatal("Open succeeded despite roomRelay disconnecting during claimDeviceNumber; session would be orphaned")
	}

	hub.mu.Lock()
	roomLen := len(hub.rooms[room])
	relayStillPresent := hub.relays[room] != nil
	hub.mu.Unlock()
	if roomLen != 0 || relayStillPresent {
		t.Fatalf("expected clean rollback after mid-open relay disconnect, got roomLen=%d relayPresent=%v", roomLen, relayStillPresent)
	}
	if slots := srv.DeviceSlotCount(room); slots != 0 {
		t.Fatalf("device slots remaining after failed Open = %d, want 0", slots)
	}
}

type realtimeRESP2Client struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *realtimeRESP2Client) writeRaw(payload string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := io.WriteString(c.conn, payload)
	return err
}

type realtimeRESP2Server struct {
	addr            string
	mu              sync.Mutex
	subs            map[string]map[*realtimeRESP2Client]struct{}
	totalSubscribes map[string]int
	zsets           map[string]map[string]float64
	beforeEval      func()
}

func (s *realtimeRESP2Server) Addr() string { return s.addr }

func (s *realtimeRESP2Server) SetBeforeEvalHook(fn func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.beforeEval = fn
}

func (s *realtimeRESP2Server) DropRoomSubscribers(room string) {
	s.mu.Lock()
	clients := make([]*realtimeRESP2Client, 0, len(s.subs[room]))
	for client := range s.subs[room] {
		clients = append(clients, client)
	}
	delete(s.subs, room)
	s.mu.Unlock()
	for _, client := range clients {
		_ = client.conn.Close()
	}
}

func (s *realtimeRESP2Server) DeviceSlotCount(room string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.zsets[room+":devices"])
}

func (s *realtimeRESP2Server) ActiveRoomSubscribers(room string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.subs[room])
}

func (s *realtimeRESP2Server) TotalRoomSubscribes(room string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalSubscribes[room]
}

func startRealtimeRESP2Server(t *testing.T) *realtimeRESP2Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen realtime RESP2 server: %v", err)
	}
	srv := &realtimeRESP2Server{
		addr:            ln.Addr().String(),
		subs:            map[string]map[*realtimeRESP2Client]struct{}{},
		totalSubscribes: map[string]int{},
		zsets:           map[string]map[string]float64{},
	}
	var wg sync.WaitGroup
	conns := map[net.Conn]struct{}{}
	closed := false

	t.Cleanup(func() {
		_ = ln.Close()
		srv.mu.Lock()
		closed = true
		for c := range conns {
			_ = c.Close()
		}
		srv.mu.Unlock()
		wg.Wait()
	})

	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			srv.mu.Lock()
			if closed {
				srv.mu.Unlock()
				_ = conn.Close()
				return
			}
			conns[conn] = struct{}{}
			wg.Add(1)
			srv.mu.Unlock()

			go func(c net.Conn) {
				defer wg.Done()
				client := &realtimeRESP2Client{conn: c}
				defer func() {
					_ = c.Close()
					srv.mu.Lock()
					delete(conns, c)
					for ch, set := range srv.subs {
						delete(set, client)
						if len(set) == 0 {
							delete(srv.subs, ch)
						}
					}
					srv.mu.Unlock()
				}()

				reader := bufio.NewReader(c)
				subCount := 0
				for {
					args, readErr := readRealtimeRESP2Command(reader)
					if readErr != nil || len(args) == 0 {
						return
					}
					switch strings.ToUpper(args[0]) {
					case "HELLO":
						_ = client.writeRaw("-ERR unknown command\r\n")
					case "PING":
						_ = client.writeRaw("+PONG\r\n")
					case "CLIENT", "SELECT", "QUIT":
						_ = client.writeRaw("+OK\r\n")
					case "SUBSCRIBE":
						for _, ch := range args[1:] {
							srv.mu.Lock()
							if srv.subs[ch] == nil {
								srv.subs[ch] = map[*realtimeRESP2Client]struct{}{}
							}
							srv.subs[ch][client] = struct{}{}
							srv.totalSubscribes[ch]++
							srv.mu.Unlock()
							subCount++
							_ = client.writeRaw(fmt.Sprintf("*3\r\n$9\r\nsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(ch), ch, subCount))
						}
					case "UNSUBSCRIBE":
						channels := args[1:]
						srv.mu.Lock()
						if len(channels) == 0 {
							for ch, set := range srv.subs {
								if _, ok := set[client]; ok {
									channels = append(channels, ch)
								}
							}
						}
						for _, ch := range channels {
							if set := srv.subs[ch]; set != nil {
								delete(set, client)
								if len(set) == 0 {
									delete(srv.subs, ch)
								}
							}
							if subCount > 0 {
								subCount--
							}
						}
						srv.mu.Unlock()
						for _, ch := range channels {
							_ = client.writeRaw(fmt.Sprintf("*3\r\n$11\r\nunsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(ch), ch, subCount))
						}
					case "PUBLISH":
						if len(args) < 3 {
							_ = client.writeRaw("-ERR wrong number of arguments\r\n")
							continue
						}
						ch, msg := args[1], args[2]
						srv.mu.Lock()
						targets := make([]*realtimeRESP2Client, 0, len(srv.subs[ch]))
						for sub := range srv.subs[ch] {
							targets = append(targets, sub)
						}
						srv.mu.Unlock()
						frame := fmt.Sprintf("*3\r\n$7\r\nmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n", len(ch), ch, len(msg), msg)
						delivered := 0
						for _, sub := range targets {
							if sub.writeRaw(frame) == nil {
								delivered++
							}
						}
						_ = client.writeRaw(fmt.Sprintf(":%d\r\n", delivered))
					case "EVAL", "EVALSHA":
						// Emulate claimRealtimeDevice and renewRealtimeDevice Lua scripts.
						srv.mu.Lock()
						hook := srv.beforeEval
						srv.mu.Unlock()
						if hook != nil {
							hook()
						}
						if len(args) < 5 {
							_ = client.writeRaw(":0\r\n")
							continue
						}
						scriptOrHash := args[1]
						key := args[3]
						arg1 := args[4]
						nowMs := float64(time.Now().UnixMilli())
						srv.mu.Lock()
						zset := srv.zsets[key]
						if zset == nil {
							zset = map[string]float64{}
							srv.zsets[key] = zset
						}
						if scriptOrHash == renewRealtimeDevice.Hash() || strings.Contains(scriptOrHash, "ZSCORE") {
							exp, ok := zset[arg1]
							if !ok || exp <= nowMs {
								srv.mu.Unlock()
								_ = client.writeRaw(":0\r\n")
								continue
							}
							zset[arg1] = nowMs + 60000
							srv.mu.Unlock()
							_ = client.writeRaw(":1\r\n")
							continue
						}
						for member, exp := range zset {
							if exp <= nowMs {
								delete(zset, member)
							}
						}
						used := map[int]bool{}
						for member := range zset {
							if prefix, _, ok := strings.Cut(member, ":"); ok {
								if num, err := strconv.Atoi(prefix); err == nil {
									used[num] = true
								}
							}
						}
						assigned := 0
						for num := 1; num <= 64; num++ {
							if !used[num] {
								assigned = num
								zset[strconv.Itoa(num)+":"+arg1] = nowMs + 60000
								break
							}
						}
						srv.mu.Unlock()
						_ = client.writeRaw(fmt.Sprintf(":%d\r\n", assigned))
					case "ZREM":
						if len(args) < 3 {
							_ = client.writeRaw(":0\r\n")
							continue
						}
						key := args[1]
						removed := 0
						srv.mu.Lock()
						if zset := srv.zsets[key]; zset != nil {
							for _, m := range args[2:] {
								if _, ok := zset[m]; ok {
									delete(zset, m)
									removed++
								}
							}
							if len(zset) == 0 {
								delete(srv.zsets, key)
							}
						}
						srv.mu.Unlock()
						_ = client.writeRaw(fmt.Sprintf(":%d\r\n", removed))
					case "ZADD":
						if len(args) >= 4 {
							score, _ := strconv.ParseFloat(args[2], 64)
							srv.mu.Lock()
							if srv.zsets[args[1]] == nil {
								srv.zsets[args[1]] = map[string]float64{}
							}
							srv.zsets[args[1]][args[3]] = score
							srv.mu.Unlock()
						}
						_ = client.writeRaw(":1\r\n")
					default:
						_ = client.writeRaw("+OK\r\n")
					}
				}
			}(conn)
		}
	}()

	return srv
}

func readRealtimeRESP2Command(r *bufio.Reader) ([]string, error) {
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
	for range count {
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
