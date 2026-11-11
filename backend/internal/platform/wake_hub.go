package platform

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

const (
	wakeChannelPrefix    = "canvas:wake:"
	wakeChannelPattern   = "canvas:wake:*"
	wakePublishTimeout   = 200 * time.Millisecond
	wakeSubscribeTimeout = 2 * time.Second
	wakeInitWaitTimeout  = 250 * time.Millisecond
	wakeMinBackoff       = 200 * time.Millisecond
	wakeMaxBackoff       = 2 * time.Second
)

// WakeHub multiplexes cross-instance SSE wakeup notifications over a single
// Redis PSUBSCRIBE connection per process and coalesces local wake signals via
// non-blocking 1-buffered channels. Persistence remains authoritative in DB.
type WakeHub struct {
	mu        sync.Mutex
	rdb       *redis.Client
	subs      map[string]map[chan struct{}]struct{}
	closed    bool
	ctx       context.Context
	cancel    context.CancelFunc
	pubsub    *redis.PubSub
	ready     chan struct{}
	done      chan struct{}
	closeOnce sync.Once
}

func NewWakeHub(rdb *redis.Client) *WakeHub {
	ctx, cancel := context.WithCancel(context.Background())
	h := &WakeHub{
		rdb:    rdb,
		subs:   make(map[string]map[chan struct{}]struct{}),
		ctx:    ctx,
		cancel: cancel,
		ready:  make(chan struct{}),
		done:   make(chan struct{}),
	}
	if rdb == nil {
		close(h.ready)
		close(h.done)
		return h
	}
	go h.psubLoop()
	select {
	case <-h.ready:
	case <-time.After(wakeInitWaitTimeout):
	}
	return h
}

func (h *WakeHub) Subscribe(topic string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	if h == nil || topic == "" {
		return ch, func() {}
	}
	h.mu.Lock()
	if !h.closed {
		if h.subs[topic] == nil {
			h.subs[topic] = make(map[chan struct{}]struct{})
		}
		h.subs[topic][ch] = struct{}{}
	}
	h.mu.Unlock()

	var once sync.Once
	unsub := func() {
		once.Do(func() {
			h.mu.Lock()
			if set := h.subs[topic]; set != nil {
				delete(set, ch)
				if len(set) == 0 {
					delete(h.subs, topic)
				}
			}
			h.mu.Unlock()
		})
	}
	return ch, unsub
}

func (h *WakeHub) notifyLocal(topic string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	for ch := range h.subs[topic] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *WakeHub) Notify(topic string) {
	if h == nil || topic == "" {
		return
	}
	h.mu.Lock()
	if h.closed {
		h.mu.Unlock()
		return
	}
	for ch := range h.subs[topic] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
	rdb := h.rdb
	parentCtx := h.ctx
	h.mu.Unlock()

	if rdb != nil {
		pubCtx, cancel := context.WithTimeout(parentCtx, wakePublishTimeout)
		_ = rdb.Publish(pubCtx, wakeChannelPrefix+topic, "1").Err()
		cancel()
	}
}

func (h *WakeHub) psubLoop() {
	defer close(h.done)
	var readyOnce sync.Once
	markReady := func() {
		readyOnce.Do(func() { close(h.ready) })
	}
	defer markReady()

	backoff := wakeMinBackoff
	for h.ctx.Err() == nil {
		ps := h.rdb.PSubscribe(h.ctx, wakeChannelPattern)
		subCtx, subCancel := context.WithTimeout(h.ctx, wakeSubscribeTimeout)
		_, err := ps.Receive(subCtx)
		subCancel()
		if err != nil {
			_ = ps.Close()
			markReady()
			if !h.sleepBackoff(backoff) {
				return
			}
			backoff = min(backoff*2, wakeMaxBackoff)
			continue
		}

		h.mu.Lock()
		if h.closed {
			h.mu.Unlock()
			_ = ps.Close()
			return
		}
		h.pubsub = ps
		h.mu.Unlock()

		markReady()
		backoff = wakeMinBackoff

		for {
			msg, recvErr := ps.ReceiveMessage(h.ctx)
			if recvErr != nil {
				h.mu.Lock()
				if h.pubsub == ps {
					h.pubsub = nil
				}
				closed := h.closed
				h.mu.Unlock()
				_ = ps.Close()
				if closed || h.ctx.Err() != nil {
					return
				}
				if !h.sleepBackoff(backoff) {
					return
				}
				backoff = min(backoff*2, wakeMaxBackoff)
				break
			}
			if topic, ok := strings.CutPrefix(msg.Channel, wakeChannelPrefix); ok && topic != "" {
				h.notifyLocal(topic)
			}
		}
	}
}

func (h *WakeHub) sleepBackoff(d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-h.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func (h *WakeHub) Close() {
	if h == nil {
		return
	}
	h.closeOnce.Do(func() {
		h.mu.Lock()
		h.closed = true
		h.subs = make(map[string]map[chan struct{}]struct{})
		ps := h.pubsub
		h.pubsub = nil
		h.cancel()
		h.mu.Unlock()

		if ps != nil {
			_ = ps.Close()
		}
		<-h.done
	})
}
