package platform

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net"
	"path"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestWakeHubLocalSubscribeCoalesceUnsubAndClose(t *testing.T) {
	hub := NewWakeHub(nil)
	defer hub.Close()

	ch, unsub := hub.Subscribe("task-text:local-1")
	otherCh, otherUnsub := hub.Subscribe("task-text:local-2")
	defer otherUnsub()

	// Multiple notifications before drain must coalesce without blocking.
	for range 10 {
		hub.Notify("task-text:local-1")
	}

	select {
	case <-ch:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("expected local wake notification")
	}

	select {
	case <-ch:
		t.Fatal("expected coalesced buffer of 1 to be empty after single receive")
	default:
	}

	select {
	case <-otherCh:
		t.Fatal("unexpected cross-topic wake")
	default:
	}

	// Idempotent unsub stops further notifications without closing ch.
	unsub()
	unsub()
	hub.Notify("task-text:local-1")
	select {
	case <-ch:
		t.Fatal("unsubscribed channel should not receive notifications or be closed")
	default:
	}

	// Idempotent Close is safe even with active subscribers and subsequent Notify/Subscribe calls.
	hub.Close()
	hub.Close()
	hub.Notify("task-text:local-2")
	select {
	case <-otherCh:
		t.Fatal("closed hub should not notify or close subscriber channels")
	default:
	}
	postCloseCh, postCloseUnsub := hub.Subscribe("task-text:after-close")
	postCloseUnsub()
	select {
	case <-postCloseCh:
		t.Fatal("post-close channel must not be closed")
	default:
	}
}

func TestWakeHubCrossInstanceSinglePSubscribeAndReconnect(t *testing.T) {
	srv := startWakeRESP2Server(t)
	rdbA := redis.NewClient(&redis.Options{Addr: srv.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	rdbB := redis.NewClient(&redis.Options{Addr: srv.Addr(), MaxRetries: -1, ContextTimeoutEnabled: true})
	t.Cleanup(func() {
		_ = rdbA.Close()
		_ = rdbB.Close()
	})

	hubA := NewWakeHub(rdbA)
	defer hubA.Close()
	hubB := NewWakeHub(rdbB)
	defer hubB.Close()

	var targetCh <-chan struct{}
	for i := range 20 {
		topic := fmt.Sprintf("task-text:%d", i)
		ch, unsub := hubB.Subscribe(topic)
		t.Cleanup(unsub)
		if i == 7 {
			targetCh = ch
		}
	}

	// Wait briefly for PSubscribe handshake and warm rdbA command pool so timing measures pure Pub/Sub wake delivery.
	deadline := time.Now().Add(time.Second)
	for srv.ActivePSubConnsForPattern("canvas:wake:*") < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if err := rdbA.Ping(context.Background()).Err(); err != nil {
		t.Fatalf("rdbA ping: %v", err)
	}
	// hubA (1) + hubB (1) => hubB itself only opened 1 PSUBSCRIBE connection across 20 topics!
	if got := srv.ActivePSubConnsForPattern("canvas:wake:*"); got != 2 {
		t.Fatalf("active PSUBSCRIBE connections across hubA+hubB = %d, want 2 (1 per WakeHub)", got)
	}
	if total := srv.TotalPSubCommands(); total != 2 {
		t.Fatalf("total PSUBSCRIBE commands across 20 topics = %d, want 2 (1 per WakeHub)", total)
	}

	start := time.Now()
	hubA.Notify("task-text:7")
	select {
	case <-targetCh:
		if elapsed := time.Since(start); elapsed >= 50*time.Millisecond {
			t.Fatalf("cross-instance wake took %s, want < 50ms", elapsed)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("timed out waiting for cross-instance wake in < 50ms")
	}

	// Drop all current PSUBSCRIBE connections to verify bounded-backoff reconnect without leaking PubSub.
	srv.DropAllPSubConns()
	reconDeadline := time.Now().Add(2 * time.Second)
	for srv.ActivePSubConnsForPattern("canvas:wake:*") < 2 && time.Now().Before(reconDeadline) {
		time.Sleep(15 * time.Millisecond)
	}
	if got := srv.ActivePSubConnsForPattern("canvas:wake:*"); got != 2 {
		t.Fatalf("active PSUBSCRIBE connections after reconnect = %d, want 2", got)
	}

	startAfterDrop := time.Now()
	hubA.Notify("task-text:7")
	select {
	case <-targetCh:
		if elapsed := time.Since(startAfterDrop); elapsed >= 50*time.Millisecond {
			t.Fatalf("post-reconnect wake took %s, want < 50ms", elapsed)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("timed out waiting for wake after PSUBSCRIBE reconnect in < 50ms")
	}

	hubA.Close()
	hubB.Close()
	closeDeadline := time.Now().Add(time.Second)
	for srv.ActivePSubConnsForPattern("canvas:wake:*") > 0 && time.Now().Before(closeDeadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := srv.ActivePSubConnsForPattern("canvas:wake:*"); got != 0 {
		t.Fatalf("active PSUBSCRIBE connections after Close() = %d, want 0", got)
	}
}

type wakeRESP2Client struct {
	conn net.Conn
	wmu  sync.Mutex
}

func (c *wakeRESP2Client) writeRaw(payload string) error {
	c.wmu.Lock()
	defer c.wmu.Unlock()
	_, err := io.WriteString(c.conn, payload)
	return err
}

type wakeRESP2Server struct {
	addr         string
	mu           sync.Mutex
	psubs        map[string]map[*wakeRESP2Client]struct{}
	totalPSubCmd int
}

func (s *wakeRESP2Server) Addr() string { return s.addr }

func (s *wakeRESP2Server) ActivePSubConnsForPattern(pattern string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.psubs[pattern])
}

func (s *wakeRESP2Server) TotalPSubCommands() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.totalPSubCmd
}

func (s *wakeRESP2Server) DropAllPSubConns() {
	s.mu.Lock()
	var conns []net.Conn
	for _, set := range s.psubs {
		for cl := range set {
			conns = append(conns, cl.conn)
		}
	}
	s.mu.Unlock()
	for _, c := range conns {
		_ = c.Close()
	}
}

func startWakeRESP2Server(t *testing.T) *wakeRESP2Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen wake RESP2 server: %v", err)
	}
	srv := &wakeRESP2Server{
		addr:  ln.Addr().String(),
		psubs: map[string]map[*wakeRESP2Client]struct{}{},
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
				client := &wakeRESP2Client{conn: c}
				defer func() {
					_ = c.Close()
					srv.mu.Lock()
					delete(conns, c)
					for pat, set := range srv.psubs {
						delete(set, client)
						if len(set) == 0 {
							delete(srv.psubs, pat)
						}
					}
					srv.mu.Unlock()
				}()

				reader := bufio.NewReader(c)
				subCount := 0
				for {
					args, readErr := readWakeRESP2Command(reader)
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
					case "PSUBSCRIBE":
						for _, pat := range args[1:] {
							srv.mu.Lock()
							if srv.psubs[pat] == nil {
								srv.psubs[pat] = map[*wakeRESP2Client]struct{}{}
							}
							srv.psubs[pat][client] = struct{}{}
							srv.totalPSubCmd++
							srv.mu.Unlock()
							subCount++
							_ = client.writeRaw(fmt.Sprintf("*3\r\n$10\r\npsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(pat), pat, subCount))
						}
					case "PUNSUBSCRIBE":
						pats := args[1:]
						srv.mu.Lock()
						if len(pats) == 0 {
							for pat, set := range srv.psubs {
								if _, ok := set[client]; ok {
									pats = append(pats, pat)
								}
							}
						}
						for _, pat := range pats {
							if set := srv.psubs[pat]; set != nil {
								delete(set, client)
								if len(set) == 0 {
									delete(srv.psubs, pat)
								}
							}
							if subCount > 0 {
								subCount--
							}
						}
						srv.mu.Unlock()
						for _, pat := range pats {
							_ = client.writeRaw(fmt.Sprintf("*3\r\n$12\r\npunsubscribe\r\n$%d\r\n%s\r\n:%d\r\n", len(pat), pat, subCount))
						}
					case "PUBLISH":
						if len(args) < 3 {
							_ = client.writeRaw("-ERR wrong number of arguments\r\n")
							continue
						}
						ch, msg := args[1], args[2]
						type pTarget struct {
							pattern string
							client  *wakeRESP2Client
						}
						var targets []pTarget
						srv.mu.Lock()
						for pat, set := range srv.psubs {
							matched, _ := path.Match(pat, ch)
							if !matched && strings.HasSuffix(pat, "*") && strings.HasPrefix(ch, strings.TrimSuffix(pat, "*")) {
								matched = true
							}
							if matched {
								for cl := range set {
									targets = append(targets, pTarget{pattern: pat, client: cl})
								}
							}
						}
						srv.mu.Unlock()
						delivered := 0
						for _, target := range targets {
							frame := fmt.Sprintf("*4\r\n$8\r\npmessage\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n$%d\r\n%s\r\n",
								len(target.pattern), target.pattern, len(ch), ch, len(msg), msg)
							if target.client.writeRaw(frame) == nil {
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
	return srv
}

func readWakeRESP2Command(r *bufio.Reader) ([]string, error) {
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
