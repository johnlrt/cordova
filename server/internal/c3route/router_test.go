package c3route

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"cordova/server/internal/inference"
	requestcoord "cordova/server/internal/request"
)

const (
	nodeA = "11111111-1111-4111-8111-111111111111"
	nodeB = "22222222-2222-4222-8222-222222222222"
	model = "test-model"
)

type fakeEncoder struct{ err error }

func (e fakeEncoder) EncodeInput(_ context.Context, _ string, m []inference.Message) ([]int32, error) {
	if e.err != nil {
		return nil, e.err
	}
	return make([]int32, len(m)+1), nil
}

type script func(s *fakeStream, f StartFrame)

type fakeSession struct {
	gen         uint64
	script      script
	startErr    error
	ackOnCancel bool
	wg          sync.WaitGroup
	mu          sync.Mutex
	streams     []*fakeStream
}

func (s *fakeSession) Generation() uint64 { return s.gen }

func (s *fakeSession) Start(_ context.Context, f StartFrame) (Stream, error) {
	if s.startErr != nil {
		return nil, s.startErr
	}
	st := &fakeStream{ev: make(chan Event, 64), sess: s, frame: f}
	s.mu.Lock()
	s.streams = append(s.streams, st)
	s.mu.Unlock()
	if s.script != nil {
		s.wg.Add(1)
		go func() { defer s.wg.Done(); s.script(st, f) }()
	}
	return st, nil
}

func (s *fakeSession) last() *fakeStream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.streams[len(s.streams)-1]
}

type fakeStream struct {
	ev      chan Event
	sess    *fakeSession
	frame   StartFrame
	mu      sync.Mutex
	cancels []CancelFrame
	closed  bool
}

func (s *fakeStream) Events() <-chan Event { return s.ev }
func (s *fakeStream) Cancel(_ context.Context, f CancelFrame) error {
	s.mu.Lock()
	s.cancels = append(s.cancels, f)
	s.mu.Unlock()
	if s.sess.ackOnCancel {
		s.ev <- Event{Generation: s.sess.gen, Type: EventCancelled, AttemptID: f.AttemptID, LeaseID: f.LeaseID}
	}
	return nil
}
func (s *fakeStream) Close() { s.mu.Lock(); s.closed = true; s.mu.Unlock() }
func (s *fakeStream) cancelReasons() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var r []string
	for _, c := range s.cancels {
		r = append(r, c.Reason)
	}
	return r
}

func ev(s *fakeStream, f StartFrame, typ string) Event {
	return Event{Generation: s.sess.gen, Type: typ, AttemptID: f.AttemptID, LeaseID: f.LeaseID}
}
func delta(s *fakeStream, f StartFrame, i int64, text string) Event {
	e := ev(s, f, EventDelta)
	e.ChunkIndex, e.Text = i, text
	return e
}
func complete(s *fakeStream, f StartFrame, last *int64, reason string) Event {
	e := ev(s, f, EventComplete)
	e.LastChunkIndex, e.FinishReason = last, reason
	return e
}
func i64(v int64) *int64 { return &v }

func newRouter(t *testing.T, cfg Config) *Router {
	t.Helper()
	cfg.Encoder = fakeEncoder{}
	if cfg.AcceptTimeout == 0 {
		cfg.AcceptTimeout = 300 * time.Millisecond
	}
	if cfg.CancelTimeout == 0 {
		cfg.CancelTimeout = 200 * time.Millisecond
	}
	r, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func register(t *testing.T, r *Router, id string, s *fakeSession) {
	t.Helper()
	if err := r.Register(NodeInfo{NodeID: id, Session: s, CatalogVersion: "cv", Models: []string{model}}); err != nil {
		t.Fatal(err)
	}
}

func acquire(t *testing.T, r *Router) requestcoord.Attempt {
	t.Helper()
	a, err := r.Acquire(context.Background(), model, nil, time.Now().Add(5*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	return a
}

var req = inference.Request{Messages: []inference.Message{{Role: "user", Content: "hi"}}, MaxTokens: 16}

func TestRunTable(t *testing.T) {
	tests := []struct {
		name       string
		maxOutput  int
		script     script
		wantErr    error // nil = success
		wantReason string
		wantText   string
		wantCancel string // expected cancel reason, "" none
	}{
		{name: "happy path", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, "he")
			s.ev <- delta(s, f, 1, "llo")
			s.ev <- complete(s, f, i64(1), "stop")
		}, wantReason: "stop", wantText: "hello"},
		{name: "empty output", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- complete(s, f, nil, "length")
		}, wantReason: "length"},
		{name: "stale generation frames ignored", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			old := delta(s, f, 0, "late")
			old.Generation = s.sess.gen - 1
			s.ev <- old
			s.ev <- delta(s, f, 0, "ok")
			s.ev <- complete(s, f, i64(0), "stop")
		}, wantReason: "stop", wantText: "ok"},
		{name: "first chunk must be zero", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 1, "x")
		}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonSuperseded},
		{name: "chunk gap", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, "a")
			s.ev <- delta(s, f, 2, "b")
		}, wantErr: requestcoord.ErrNodeFailed, wantText: "a", wantCancel: ReasonSuperseded},
		{name: "chunk repeat", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, "a")
			s.ev <- delta(s, f, 0, "a")
		}, wantErr: requestcoord.ErrNodeFailed, wantText: "a", wantCancel: ReasonSuperseded},
		{name: "delta before accepted", script: func(s *fakeStream, f StartFrame) {
			s.ev <- delta(s, f, 0, "a")
		}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonSuperseded},
		{name: "duplicate accepted", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- ev(s, f, EventAccepted)
		}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonSuperseded},
		{name: "invalid utf8", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, "\xff")
		}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonSuperseded},
		{name: "oversized delta", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, strings.Repeat("a", maxDeltaBytes+1))
		}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonSuperseded},
		{name: "output over limit", maxOutput: 5, script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, "abc")
			s.ev <- delta(s, f, 1, "def")
		}, wantErr: requestcoord.ErrOutputLimit, wantText: "abc", wantCancel: ReasonOutputLimit},
		{name: "mismatched attempt id", script: func(s *fakeStream, f StartFrame) {
			e := ev(s, f, EventAccepted)
			e.AttemptID = "other"
			s.ev <- e
		}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonSuperseded},
		{name: "complete wrong last index", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, "a")
			s.ev <- complete(s, f, i64(3), "stop")
		}, wantErr: requestcoord.ErrNodeFailed, wantText: "a", wantCancel: ReasonSuperseded},
		{name: "complete missing last index", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, "a")
			s.ev <- complete(s, f, nil, "stop")
		}, wantErr: requestcoord.ErrNodeFailed, wantText: "a", wantCancel: ReasonSuperseded},
		{name: "complete bad finish reason", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- complete(s, f, nil, "other")
		}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonSuperseded},
		{name: "complete before accepted", script: func(s *fakeStream, f StartFrame) {
			s.ev <- complete(s, f, nil, "stop")
		}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonSuperseded},
		{name: "node error frame", script: func(s *fakeStream, f StartFrame) {
			e := ev(s, f, EventError)
			e.Code = "oom"
			s.ev <- e
		}, wantErr: requestcoord.ErrNodeFailed},
		{name: "unsolicited cancelled", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- ev(s, f, EventCancelled)
		}, wantErr: requestcoord.ErrNodeFailed},
		{name: "stream closed", script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			close(s.ev)
		}, wantErr: requestcoord.ErrNodeFailed},
		{name: "accept timeout", script: func(s *fakeStream, f StartFrame) {}, wantErr: requestcoord.ErrNodeFailed, wantCancel: ReasonDeadline},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := newRouter(t, Config{MaxOutputBytes: tc.maxOutput})
			sess := &fakeSession{gen: 5, script: tc.script, ackOnCancel: true}
			register(t, r, nodeA, sess)
			a := acquire(t, r)
			var got strings.Builder
			reason, err := a.(requestcoord.AcceptedAttempt).RunAccepted(context.Background(), req, func() error { return nil },
				func(s string) error { got.WriteString(s); return nil })
			sess.wg.Wait()
			if tc.wantErr == nil && err != nil || tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}
			if reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
			if got.String() != tc.wantText {
				t.Fatalf("text = %q, want %q", got.String(), tc.wantText)
			}
			st := sess.last()
			reasons := st.cancelReasons()
			if tc.wantCancel == "" && len(reasons) != 0 || tc.wantCancel != "" && (len(reasons) != 1 || reasons[0] != tc.wantCancel) {
				t.Fatalf("cancels = %v, want %q", reasons, tc.wantCancel)
			}
			if !st.closed {
				t.Fatal("stream not closed")
			}
			// slot is held until Release, then free on every path.
			if _, err := r.Acquire(context.Background(), model, nil, time.Now().Add(time.Second)); !errors.Is(err, requestcoord.ErrNoCapacity) {
				t.Fatalf("slot should be held before Release, err=%v", err)
			}
			a.Release(context.Background())
			a.Release(context.Background())
			b := acquire(t, r)
			b.Release(context.Background())
		})
	}
}

func TestStartFrameFields(t *testing.T) {
	r := newRouter(t, Config{})
	sess := &fakeSession{gen: 1, script: func(s *fakeStream, f StartFrame) {
		s.ev <- ev(s, f, EventAccepted)
		s.ev <- complete(s, f, nil, "stop")
	}}
	register(t, r, nodeA, sess)
	a := acquire(t, r)
	defer a.Release(context.Background())
	seed := int64(7)
	in := req
	in.Parameters = inference.Parameters{Temperature: 0.5, TopP: 0.9, Stop: []string{"x"}, Seed: &seed}
	if _, err := a.Run(context.Background(), in, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	sess.wg.Wait()
	f := sess.last().frame
	if f.ModelID != model || f.CatalogVersion != "cv" || f.Kind != "user" || f.InputTokens != 2 || len(f.InputTokenIDs) != 2 ||
		f.MaxTokens != 16 || f.Temperature != 0.5 || f.TopP != 0.9 || f.Seed == nil || *f.Seed != 7 ||
		len(f.Stop) != 1 || f.AttemptID == "" || f.LeaseID == "" || f.AttemptID == f.LeaseID || f.DeadlineAt == "" {
		t.Fatalf("unexpected start frame: %+v", f)
	}
	if a.OwnerID() != nodeA {
		t.Fatal("owner")
	}
}

func TestClientCancelSendsCancelAndReleases(t *testing.T) {
	r := newRouter(t, Config{})
	sess := &fakeSession{gen: 1, ackOnCancel: true, script: func(s *fakeStream, f StartFrame) {
		s.ev <- ev(s, f, EventAccepted)
	}}
	register(t, r, nodeA, sess)
	a := acquire(t, r)
	ctx, cancel := context.WithCancel(context.Background())
	accepted := make(chan struct{})
	go func() { <-accepted; cancel() }()
	_, err := a.(requestcoord.AcceptedAttempt).RunAccepted(ctx, req, func() error { close(accepted); return nil }, func(string) error { return nil })
	sess.wg.Wait()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	if got := sess.last().cancelReasons(); len(got) != 1 || got[0] != ReasonClientCancelled {
		t.Fatalf("cancels = %v", got)
	}
	a.Release(context.Background())
	acquire(t, r).Release(context.Background())
}

func TestDeadlineSendsCancel(t *testing.T) {
	r := newRouter(t, Config{AcceptTimeout: 5 * time.Second})
	sess := &fakeSession{gen: 1, ackOnCancel: true, script: func(s *fakeStream, f StartFrame) {
		s.ev <- ev(s, f, EventAccepted)
	}}
	register(t, r, nodeA, sess)
	a, err := r.Acquire(context.Background(), model, nil, time.Now().Add(100*time.Millisecond))
	if err != nil {
		t.Fatal(err)
	}
	defer a.Release(context.Background())
	_, err = a.Run(context.Background(), req, func(string) error { return nil })
	sess.wg.Wait()
	if !errors.Is(err, requestcoord.ErrNodeFailed) {
		t.Fatalf("err = %v", err)
	}
	if got := sess.last().cancelReasons(); len(got) != 1 || got[0] != ReasonDeadline {
		t.Fatalf("cancels = %v", got)
	}
}

func TestEmitErrorCancelsNode(t *testing.T) {
	r := newRouter(t, Config{})
	sess := &fakeSession{gen: 1, ackOnCancel: true, script: func(s *fakeStream, f StartFrame) {
		s.ev <- ev(s, f, EventAccepted)
		s.ev <- delta(s, f, 0, "a")
	}}
	register(t, r, nodeA, sess)
	a := acquire(t, r)
	defer a.Release(context.Background())
	boom := errors.New("client gone")
	_, err := a.Run(context.Background(), req, func(string) error { return boom })
	sess.wg.Wait()
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v", err)
	}
	if got := sess.last().cancelReasons(); len(got) != 1 || got[0] != ReasonClientCancelled {
		t.Fatalf("cancels = %v", got)
	}
}

func TestAcceptedCallbackError(t *testing.T) {
	r := newRouter(t, Config{})
	sess := &fakeSession{gen: 1, ackOnCancel: true, script: func(s *fakeStream, f StartFrame) { s.ev <- ev(s, f, EventAccepted) }}
	register(t, r, nodeA, sess)
	a := acquire(t, r)
	defer a.Release(context.Background())
	boom := errors.New("closed")
	_, err := a.(requestcoord.AcceptedAttempt).RunAccepted(context.Background(), req, func() error { return boom }, func(string) error { return nil })
	sess.wg.Wait()
	if !errors.Is(err, boom) || len(sess.last().cancelReasons()) != 1 {
		t.Fatalf("err=%v cancels=%v", err, sess.last().cancelReasons())
	}
}

func TestStartAndEncoderFailures(t *testing.T) {
	r := newRouter(t, Config{})
	sess := &fakeSession{gen: 1, startErr: errors.New("write failed")}
	register(t, r, nodeA, sess)
	a := acquire(t, r)
	if _, err := a.Run(context.Background(), req, func(string) error { return nil }); !errors.Is(err, requestcoord.ErrNodeFailed) {
		t.Fatalf("err = %v", err)
	}
	a.Release(context.Background())
	acquire(t, r).Release(context.Background())

	r2, _ := New(Config{Encoder: fakeEncoder{err: errors.New("tok")}})
	sess2 := &fakeSession{gen: 1}
	register(t, r2, nodeA, sess2)
	a2 := acquire(t, r2)
	defer a2.Release(context.Background())
	if _, err := a2.Run(context.Background(), req, func(string) error { return nil }); !errors.Is(err, requestcoord.ErrNodeFailed) {
		t.Fatalf("err = %v", err)
	}
	if len(sess2.streams) != 0 {
		t.Fatal("start must not be sent without token IDs")
	}
}

func TestRunTwiceRejected(t *testing.T) {
	r := newRouter(t, Config{})
	sess := &fakeSession{gen: 1, script: func(s *fakeStream, f StartFrame) {
		s.ev <- ev(s, f, EventAccepted)
		s.ev <- complete(s, f, nil, "stop")
	}}
	register(t, r, nodeA, sess)
	a := acquire(t, r)
	defer a.Release(context.Background())
	if _, err := a.Run(context.Background(), req, func(string) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Run(context.Background(), req, func(string) error { return nil }); !errors.Is(err, requestcoord.ErrNodeFailed) {
		t.Fatalf("second run err = %v", err)
	}
}

func TestAcquireRules(t *testing.T) {
	r := newRouter(t, Config{})
	sa, sb := &fakeSession{gen: 1}, &fakeSession{gen: 1}
	register(t, r, nodeA, sa)
	register(t, r, nodeB, sb)
	dl := time.Now().Add(time.Second)

	a1, err := r.Acquire(context.Background(), model, nil, dl)
	if err != nil || a1.OwnerID() != nodeA {
		t.Fatalf("first: %v %v", a1, err)
	}
	// One slot per node: second request goes to B, third has no capacity.
	a2, err := r.Acquire(context.Background(), model, nil, dl)
	if err != nil || a2.OwnerID() != nodeB {
		t.Fatalf("second: %v", err)
	}
	if _, err := r.Acquire(context.Background(), model, nil, dl); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("third: %v", err)
	}
	a1.Release(context.Background())
	a2.Release(context.Background())

	// Retry exclusion.
	a3, err := r.Acquire(context.Background(), model, map[string]struct{}{nodeA: {}}, dl)
	if err != nil || a3.OwnerID() != nodeB {
		t.Fatalf("exclusion: %v", err)
	}
	a3.Release(context.Background())
	if _, err := r.Acquire(context.Background(), model, map[string]struct{}{nodeA: {}, nodeB: {}}, dl); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("all excluded: %v", err)
	}
	// Unknown model, expired deadline, cancelled ctx.
	if _, err := r.Acquire(context.Background(), "other", nil, dl); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("model: %v", err)
	}
	if _, err := r.Acquire(context.Background(), model, nil, time.Now().Add(-time.Second)); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("deadline: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := r.Acquire(ctx, model, nil, dl); !errors.Is(err, context.Canceled) {
		t.Fatalf("ctx: %v", err)
	}
}

func TestConcurrentAcquireNeverExceedsSlots(t *testing.T) {
	r := newRouter(t, Config{})
	register(t, r, nodeA, &fakeSession{gen: 1})
	var wg sync.WaitGroup
	var mu sync.Mutex
	won := 0
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := r.Acquire(context.Background(), model, nil, time.Now().Add(time.Second)); err == nil {
				mu.Lock()
				won++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won != 1 {
		t.Fatalf("won = %d", won)
	}
}

func TestRegisterValidationAndGenerations(t *testing.T) {
	r := newRouter(t, Config{})
	if err := r.Register(NodeInfo{NodeID: "bad", Session: &fakeSession{gen: 1}}); err == nil {
		t.Fatal("bad node ID accepted")
	}
	if err := r.Register(NodeInfo{NodeID: nodeA}); err == nil {
		t.Fatal("nil session accepted")
	}
	register(t, r, nodeA, &fakeSession{gen: 3})
	if err := r.Register(NodeInfo{NodeID: nodeA, Session: &fakeSession{gen: 3}}); err == nil {
		t.Fatal("equal generation accepted")
	}
	if err := r.Register(NodeInfo{NodeID: nodeA, Session: &fakeSession{gen: 2}}); err == nil {
		t.Fatal("older generation accepted")
	}
	if _, err := New(Config{}); err == nil {
		t.Fatal("missing encoder accepted")
	}
}

func TestNewSessionFencesActiveAttempt(t *testing.T) {
	r := newRouter(t, Config{})
	old := &fakeSession{gen: 1, ackOnCancel: true, script: func(s *fakeStream, f StartFrame) { s.ev <- ev(s, f, EventAccepted) }}
	register(t, r, nodeA, old)
	a := acquire(t, r)
	done := make(chan error, 1)
	accepted := make(chan struct{})
	go func() {
		_, err := a.(requestcoord.AcceptedAttempt).RunAccepted(context.Background(), req, func() error { close(accepted); return nil }, func(string) error { return nil })
		done <- err
	}()
	<-accepted
	fresh := &fakeSession{gen: 2}
	register(t, r, nodeA, fresh)
	if err := <-done; !errors.Is(err, requestcoord.ErrNodeFailed) {
		t.Fatalf("err = %v", err)
	}
	old.wg.Wait()
	if got := old.last().cancelReasons(); len(got) != 1 || got[0] != ReasonSuperseded {
		t.Fatalf("cancels = %v", got)
	}
	// New session's slot is free, and releasing the stale attempt must not
	// free a newer session's claimed slot.
	b := acquire(t, r)
	a.Release(context.Background())
	if _, err := r.Acquire(context.Background(), model, nil, time.Now().Add(time.Second)); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("stale release freed new slot: %v", err)
	}
	b.Release(context.Background())
}

func TestLateFramesFromOldSessionRejectedAfterFence(t *testing.T) {
	r := newRouter(t, Config{})
	old := &fakeSession{gen: 1}
	register(t, r, nodeA, old)
	a := acquire(t, r)
	defer a.Release(context.Background())
	started := make(chan struct{})
	old.script = func(s *fakeStream, f StartFrame) {
		s.ev <- ev(s, f, EventAccepted)
		close(started)
	}
	done := make(chan error, 1)
	var got strings.Builder
	go func() {
		_, err := a.Run(context.Background(), req, func(s string) error { got.WriteString(s); return nil })
		done <- err
	}()
	<-started
	r.Unregister(nodeA, 1)
	old.wg.Wait()
	// Late frame after unregister: attempt must already be failing, nothing emitted.
	select {
	case old.last().ev <- delta(old.last(), old.last().frame, 0, "late"):
	default:
	}
	if err := <-done; !errors.Is(err, requestcoord.ErrNodeFailed) {
		t.Fatalf("err = %v", err)
	}
	if got.Len() != 0 {
		t.Fatalf("late text delivered: %q", got.String())
	}
	if _, err := r.Acquire(context.Background(), model, nil, time.Now().Add(time.Second)); !errors.Is(err, requestcoord.ErrNoCapacity) {
		t.Fatalf("unregistered node still routable: %v", err)
	}
	r.Unregister(nodeA, 1) // idempotent
}

func TestNoGoroutineLeaks(t *testing.T) {
	before := runtime.NumGoroutine()
	for i := 0; i < 20; i++ {
		r := newRouter(t, Config{})
		sess := &fakeSession{gen: 1, ackOnCancel: true, script: func(s *fakeStream, f StartFrame) {
			s.ev <- ev(s, f, EventAccepted)
			s.ev <- delta(s, f, 0, "a")
			s.ev <- delta(s, f, 5, "b")
		}}
		register(t, r, nodeA, sess)
		a := acquire(t, r)
		_, _ = a.Run(context.Background(), req, func(string) error { return nil })
		sess.wg.Wait()
		a.Release(context.Background())
	}
	deadline := time.Now().Add(2 * time.Second)
	for runtime.NumGoroutine() > before && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n := runtime.NumGoroutine(); n > before {
		t.Fatalf("goroutines: before=%d after=%d", before, n)
	}
}
