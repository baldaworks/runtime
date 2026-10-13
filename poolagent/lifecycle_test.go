package poolagent

import (
	"context"
	"errors"
	"google.golang.org/adk/v2/session"
	"io"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
)

type lifecycleMember struct {
	agent.Agent
	closes atomic.Int32
}

func (m *lifecycleMember) Close() error { m.closes.Add(1); return nil }

type lifecycleCreator struct {
	member  *lifecycleMember
	calls   atomic.Int32
	started chan struct{}
	release chan struct{}
}

func (c *lifecycleCreator) CreateAgent(context.Context, string, AgentRequest) (agent.Agent, error) {
	c.calls.Add(1)
	if c.started != nil {
		close(c.started)
		<-c.release
	}
	return c.member, nil
}

func TestPoolCloseFencesCreation(t *testing.T) {
	c := &lifecycleCreator{member: &lifecycleMember{}}
	p := NewPoolExecutor("pool", []MemberConfig{{Name: "member"}}, c, AgentRequest{})
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Agent(context.Background()); err == nil {
		t.Fatal("closed pool created an agent")
	}
	if c.calls.Load() != 0 {
		t.Fatal("closed pool invoked member constructor")
	}
}

func TestPoolCloseConcurrent(t *testing.T) {
	c := &lifecycleCreator{member: &lifecycleMember{}, started: make(chan struct{}), release: make(chan struct{})}
	p := NewPoolExecutor("pool", []MemberConfig{{Name: "member"}}, c, AgentRequest{})
	created := make(chan struct{})
	go func() { _, _ = p.Agent(context.Background()); close(created) }()
	<-c.started
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() { _ = p.Close() })
	}
	close(c.release)
	<-created
	wg.Wait()
	if got := c.member.closes.Load(); got != 1 {
		t.Fatalf("member Close calls=%d, want1", got)
	}
	if _, err := p.Agent(context.Background()); err == nil {
		t.Fatal("closed pool returned a member")
	}
}

var _ io.Closer = (*lifecycleMember)(nil)

func TestPoolClosePendingTimeout(t *testing.T) {
	c := &lifecycleCreator{member: &lifecycleMember{}, started: make(chan struct{}), release: make(chan struct{})}
	p := NewPoolExecutor("pool", []MemberConfig{{Name: "member"}}, c, AgentRequest{})
	result := make(chan error, 1)
	go func() { _, err := p.Agent(context.Background()); result <- err }()
	<-c.started
	if err := p.Close(); !errors.Is(err, context.DeadlineExceeded) {
		close(c.release)
		t.Fatalf("stuck constructor shutdown error=%v", err)
	}
	close(c.release)
	if err := <-result; err == nil {
		t.Fatal("late member escaped closed pool")
	}
	if got := c.member.closes.Load(); got != 1 {
		t.Fatalf("late member closes=%d", got)
	}
}

type retryMember struct {
	agent.Agent
	closes atomic.Int32
	fail   bool
}

func (m *retryMember) Close() error { m.closes.Add(1); return nil }
func (m *retryMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if m.fail {
			yield(nil, errors.New("run failed"))
		} else {
			yield(nil, nil)
		}
	}
}

type retryCreator struct {
	members []*retryMember
	calls   int
}

func (c *retryCreator) CreateAgent(context.Context, string, AgentRequest) (agent.Agent, error) {
	member := c.members[c.calls]
	c.calls++
	return member, nil
}

type retryContext struct {
	agent.InvocationContext
	context.Context
}

func (c retryContext) Deadline() (time.Time, bool) { return c.Context.Deadline() }
func (c retryContext) Done() <-chan struct{}       { return c.Context.Done() }
func (c retryContext) Err() error                  { return c.Context.Err() }
func (c retryContext) Value(k any) any             { return c.Context.Value(k) }

func TestPoolRetryRetainsMemberOwnership(t *testing.T) {
	first, second := &retryMember{fail: true}, &retryMember{}
	c := &retryCreator{members: []*retryMember{first, second}}
	p := NewPoolExecutor("pool", []MemberConfig{{Name: "member"}}, c, AgentRequest{})
	pool := &PoolAgent{executor: p}
	events := 0
	for _, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			t.Fatal(err)
		}
		events++
	}
	if events != 1 || c.calls != 2 {
		t.Fatalf("retry events=%d calls=%d", events, c.calls)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if first.closes.Load() != 1 || second.closes.Load() != 1 {
		t.Fatal("retry lost ownership of a member")
	}
}

func TestPoolRetryStaleErrorKeepsCurrentMember(t *testing.T) {
	first, second := &retryMember{}, &retryMember{}
	c := &retryCreator{members: []*retryMember{first, second}}
	p := NewPoolExecutor("pool", []MemberConfig{{Name: "member"}}, c, AgentRequest{})
	old, err := p.member(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.discard(old)
	current, err := p.Agent(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	p.discard(old)
	got, err := p.Agent(context.Background())
	if err != nil || got != current || c.calls != 2 {
		t.Fatalf("stale failure replaced current member: got=%v err=%v calls=%d", got, err, c.calls)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
}

type contextCreator struct {
	ctx    context.Context
	member *lifecycleMember
}

func (c *contextCreator) CreateAgent(ctx context.Context, _ string, _ AgentRequest) (agent.Agent, error) {
	c.ctx = ctx
	return c.member, nil
}

func TestPoolCloseOwnsSuccessfulCreationContext(t *testing.T) {
	c := &contextCreator{member: &lifecycleMember{}}
	p := NewPoolExecutor("pool", []MemberConfig{{Name: "member"}}, c, AgentRequest{})
	if _, err := p.Agent(context.Background()); err != nil {
		t.Fatal(err)
	}
	if c.ctx.Err() != nil {
		t.Fatal("successful member construction context canceled before owner Close")
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if c.ctx.Err() != context.Canceled {
		t.Fatal("member construction context remained live after owner Close")
	}
}
