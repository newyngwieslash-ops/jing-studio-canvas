package canvas

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"yingce/backend/internal/kernel"
)

type PresencePoint struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

type PresenceNode struct {
	ID string `json:"id"`
	PresencePoint
	Width  float64 `json:"width"`
	Height float64 `json:"height"`
}

type CanvasPresence struct {
	ID        string         `json:"id"`
	Number    int            `json:"number"`
	Cursor    *PresencePoint `json:"cursor"`
	Drag      []PresenceNode `json:"drag"`
	UpdatedAt int64          `json:"updatedAt"`
}

type RealtimeEvent struct {
	Type   string          `json:"type"`
	Peer   *CanvasPresence `json:"peer,omitempty"`
	PeerID string          `json:"peerId,omitempty"`
}

type realtimeEnvelope struct {
	RealtimeEvent
	Source string `json:"source"`
}

// Presence is ephemeral; cursor and drag messages never enter the database.
type RealtimeHub struct {
	mu     sync.Mutex
	rooms  map[string]map[string]*RealtimeSession
	relays map[string]*roomRelay
	closed bool
	redis  *redis.Client
	id     string
}

type roomRelay struct {
	hub     *RealtimeHub
	room    string
	refs    int
	ready   chan struct{}
	initErr error
	ctx     context.Context
	cancel  context.CancelFunc
	pubsub  *redis.PubSub
}

type RealtimeSession struct {
	hub    *RealtimeHub
	room   string
	peer   CanvasPresence
	mu     sync.Mutex
	Events chan RealtimeEvent
	ctx    context.Context
	cancel context.CancelFunc
	relay  *roomRelay
	once   sync.Once
}

func NewRealtimeHub(client *redis.Client) *RealtimeHub {
	return &RealtimeHub{
		rooms:  make(map[string]map[string]*RealtimeSession),
		relays: make(map[string]*roomRelay),
		redis:  client,
		id:     kernel.NewID(),
	}
}

func realtimeRoom(userID, canvasID string) string {
	return "canvas:realtime:v1:" + base64.RawURLEncoding.EncodeToString([]byte(userID)) + ":" + base64.RawURLEncoding.EncodeToString([]byte(canvasID))
}

func (h *RealtimeHub) acquireRoomRelay(ctx context.Context, room string) (*roomRelay, error) {
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return nil, errors.New("实时通知服务已关闭")
	}
	relay := h.relays[room]
	first := false
	if relay == nil {
		relayCtx, relayCancel := context.WithCancel(context.Background())
		relay = &roomRelay{
			hub:    h,
			room:   room,
			refs:   1,
			ready:  make(chan struct{}),
			ctx:    relayCtx,
			cancel: relayCancel,
		}
		h.relays[room] = relay
		first = true
	} else {
		relay.refs++
	}
	h.mu.Unlock()

	if first {
		pubsub := h.redis.Subscribe(relay.ctx, room)
		readyCtx, stop := context.WithTimeout(relay.ctx, 3*time.Second)
		_, subErr := pubsub.Receive(readyCtx)
		stop()

		h.mu.Lock()
		if subErr != nil {
			relay.initErr = subErr
			if h.relays[room] == relay {
				delete(h.relays, room)
			}
			relay.cancel()
			close(relay.ready)
			h.mu.Unlock()
			_ = pubsub.Close()
			h.releaseRoomRelay(relay)
			return nil, subErr
		}
		if h.closed || relay.refs <= 0 || relay.ctx.Err() != nil {
			initErr := relay.ctx.Err()
			if initErr == nil {
				initErr = errors.New("实时通知服务已关闭")
			}
			relay.initErr = initErr
			if h.relays[room] == relay {
				delete(h.relays, room)
			}
			relay.cancel()
			close(relay.ready)
			h.mu.Unlock()
			_ = pubsub.Close()
			h.releaseRoomRelay(relay)
			return nil, initErr
		}
		relay.pubsub = pubsub
		close(relay.ready)
		h.mu.Unlock()

		go relay.receive(pubsub)
		if err := ctx.Err(); err != nil {
			h.releaseRoomRelay(relay)
			return nil, err
		}
		return relay, nil
	}

	select {
	case <-relay.ready:
		if relay.initErr != nil {
			h.releaseRoomRelay(relay)
			return nil, relay.initErr
		}
		return relay, nil
	case <-ctx.Done():
		h.releaseRoomRelay(relay)
		return nil, ctx.Err()
	}
}

func (h *RealtimeHub) releaseRoomRelay(relay *roomRelay) {
	if relay == nil {
		return
	}
	var closePubSub *redis.PubSub
	h.mu.Lock()
	relay.refs--
	if relay.refs <= 0 {
		if h.relays[relay.room] == relay {
			delete(h.relays, relay.room)
		}
		relay.cancel()
		closePubSub = relay.pubsub
		relay.pubsub = nil
	}
	h.mu.Unlock()

	if closePubSub != nil {
		_ = closePubSub.Close()
	}
}

func (h *RealtimeHub) Open(ctx context.Context, userID, canvasID string) (*RealtimeSession, []CanvasPresence, error) {
	ctx, cancel := context.WithCancel(ctx)
	s := &RealtimeSession{hub: h, room: realtimeRoom(userID, canvasID), peer: CanvasPresence{ID: kernel.NewID(), UpdatedAt: time.Now().UnixMilli()}, Events: make(chan RealtimeEvent, 128), ctx: ctx, cancel: cancel}
	if h.redis != nil {
		relay, err := h.acquireRoomRelay(ctx, s.room)
		if err != nil {
			cancel()
			return nil, nil, err
		}
		s.relay = relay
		if err := s.claimDeviceNumber(); err != nil {
			cancel()
			h.releaseRoomRelay(relay)
			return nil, nil, err
		}
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		cancel()
		if s.relay != nil {
			h.releaseRoomRelay(s.relay)
			s.releaseDeviceNumber()
		}
		return nil, nil, errors.New("实时通知服务已关闭")
	}
	if s.relay != nil && (s.relay.ctx.Err() != nil || h.relays[s.room] != s.relay) {
		h.mu.Unlock()
		cancel()
		h.releaseRoomRelay(s.relay)
		s.releaseDeviceNumber()
		return nil, nil, errors.New("实时通知连接已断开，请重试")
	}
	if len(h.rooms[s.room]) >= 64 {
		h.mu.Unlock()
		cancel()
		if s.relay != nil {
			h.releaseRoomRelay(s.relay)
			s.releaseDeviceNumber()
		}
		return nil, nil, errors.New("此画布打开的窗口过多")
	}
	if h.rooms[s.room] == nil {
		h.rooms[s.room] = make(map[string]*RealtimeSession)
	}
	peers := make([]CanvasPresence, 0, len(h.rooms[s.room])+1)
	used := make(map[int]bool)
	for _, existing := range h.rooms[s.room] {
		peer := existing.Snapshot()
		peers = append(peers, peer)
		used[peer.Number] = true
	}
	if h.redis == nil {
		s.peer.Number = 1
		for used[s.peer.Number] {
			s.peer.Number++
		}
	}
	h.rooms[s.room][s.peer.ID] = s
	peers = append(peers, s.Snapshot())
	h.mu.Unlock()
	peer := s.Snapshot()
	h.publish(s.room, RealtimeEvent{Type: "peer", Peer: &peer})
	// Other instances answer with their participants, including idle cursors.
	h.publish(s.room, RealtimeEvent{Type: "hello", PeerID: peer.ID})
	return s, peers, nil
}

func (h *RealtimeHub) Changed(userID, canvasID string) {
	h.publish(realtimeRoom(userID, canvasID), RealtimeEvent{Type: "invalidate"})
}

func (h *RealtimeHub) roomSnapshots(room string) []CanvasPresence {
	h.mu.Lock()
	sessions := make([]*RealtimeSession, 0, len(h.rooms[room]))
	for _, session := range h.rooms[room] {
		sessions = append(sessions, session)
	}
	h.mu.Unlock()

	peers := make([]CanvasPresence, 0, len(sessions))
	for _, session := range sessions {
		peers = append(peers, session.Snapshot())
	}
	return peers
}

func (h *RealtimeHub) deliver(room string, event RealtimeEvent) bool {
	if event.Type == "hello" {
		h.mu.Lock()
		closed := h.closed
		h.mu.Unlock()
		return !closed
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return false
	}
	for _, session := range h.rooms[room] {
		select {
		case session.Events <- event:
		default:
			session.cancel() // Slow consumers reconnect and read a fresh snapshot.
		}
	}
	return true
}

func (h *RealtimeHub) publish(room string, event RealtimeEvent) {
	if !h.deliver(room, event) {
		return
	}
	if h.redis == nil {
		return
	}
	data, err := json.Marshal(realtimeEnvelope{RealtimeEvent: event, Source: h.id})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := h.redis.Publish(ctx, room, data).Err(); err != nil {
		slog.Warn("画布实时通知发送失败，客户端将通过状态校验恢复", "error", err)
	}
}

func (h *RealtimeHub) handleRelayDisconnect(relay *roomRelay, pubsub *redis.PubSub) {
	var closePubSub *redis.PubSub
	var sessions []*RealtimeSession
	h.mu.Lock()
	if h.relays[relay.room] == relay {
		delete(h.relays, relay.room)
	}
	relay.cancel()
	if relay.pubsub != nil {
		closePubSub = relay.pubsub
		relay.pubsub = nil
	} else {
		closePubSub = pubsub
	}
	for _, session := range h.rooms[relay.room] {
		if session.relay == relay {
			sessions = append(sessions, session)
		}
	}
	h.mu.Unlock()

	if closePubSub != nil {
		_ = closePubSub.Close()
	}
	for _, session := range sessions {
		session.cancel()
	}
}

func (r *roomRelay) receive(pubsub *redis.PubSub) {
	for {
		message, err := pubsub.ReceiveMessage(r.ctx)
		if err != nil {
			if r.ctx.Err() == nil {
				r.hub.handleRelayDisconnect(r, pubsub)
			}
			return
		}
		var event realtimeEnvelope
		if json.Unmarshal([]byte(message.Payload), &event) != nil || event.Source == r.hub.id {
			continue
		}
		if event.Type == "hello" {
			peers := r.hub.roomSnapshots(r.room)
			for i := range peers {
				peer := peers[i]
				r.hub.publish(r.room, RealtimeEvent{Type: "peer", Peer: &peer})
			}
			continue
		}
		r.hub.deliver(r.room, event.RealtimeEvent)
	}
}

func (h *RealtimeHub) Close() {
	if h == nil {
		return
	}
	var pubsubs []*redis.PubSub
	var sessions []*RealtimeSession
	h.mu.Lock()
	h.closed = true
	for room, relay := range h.relays {
		delete(h.relays, room)
		relay.cancel()
		if relay.pubsub != nil {
			pubsubs = append(pubsubs, relay.pubsub)
			relay.pubsub = nil
		}
	}
	for _, roomSessions := range h.rooms {
		for _, session := range roomSessions {
			sessions = append(sessions, session)
		}
	}
	h.mu.Unlock()

	for _, pubsub := range pubsubs {
		_ = pubsub.Close()
	}
	for _, session := range sessions {
		session.cancel()
	}
}

func (s *RealtimeSession) ID() string               { return s.peer.ID }
func (s *RealtimeSession) Done() <-chan struct{}    { return s.ctx.Done() }
func (s *RealtimeSession) Snapshot() CanvasPresence { s.mu.Lock(); defer s.mu.Unlock(); return s.peer }

func validPresenceNumber(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0) && math.Abs(value) <= 1e8
}

func (s *RealtimeSession) Update(cursor *PresencePoint, drag []PresenceNode) error {
	if cursor != nil && (!validPresenceNumber(cursor.X) || !validPresenceNumber(cursor.Y)) {
		return errors.New("光标坐标无效")
	}
	if len(drag) > 100 {
		return errors.New("拖拽预览节点过多")
	}
	seen := make(map[string]bool, len(drag))
	for _, node := range drag {
		if node.ID == "" || len(node.ID) > 160 || seen[node.ID] || !validPresenceNumber(node.X) || !validPresenceNumber(node.Y) || !validPresenceNumber(node.Width) || !validPresenceNumber(node.Height) || node.Width <= 0 || node.Height <= 0 {
			return errors.New("拖拽预览无效")
		}
		seen[node.ID] = true
	}
	s.mu.Lock()
	s.peer.Cursor, s.peer.Drag, s.peer.UpdatedAt = cursor, drag, time.Now().UnixMilli()
	peer := s.peer
	s.mu.Unlock()
	s.hub.publish(s.room, RealtimeEvent{Type: "peer", Peer: &peer})
	return nil
}

func (s *RealtimeSession) Heartbeat() {
	if !s.renewDeviceNumber() {
		s.cancel()
		return
	}
	s.mu.Lock()
	s.peer.UpdatedAt = time.Now().UnixMilli()
	peer := s.peer
	s.mu.Unlock()
	s.hub.publish(s.room, RealtimeEvent{Type: "peer", Peer: &peer})
}

func (s *RealtimeSession) Close() {
	s.once.Do(func() {
		s.cancel()
		s.hub.mu.Lock()
		delete(s.hub.rooms[s.room], s.peer.ID)
		if len(s.hub.rooms[s.room]) == 0 {
			delete(s.hub.rooms, s.room)
		}
		s.hub.mu.Unlock()
		if s.relay != nil {
			s.hub.releaseRoomRelay(s.relay)
			s.releaseDeviceNumber()
		}
		s.hub.publish(s.room, RealtimeEvent{Type: "leave", PeerID: s.peer.ID})
	})
}
