package agentfactory

import (
	"context"
	"errors"
	"google.golang.org/adk/v2/session"
	"io"
	"iter"
	"sync"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/hostedagent"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
)

// ownedMCPTransport retains the actual SDK connections, including their private
// session-update capabilities. ADK's lazy toolset does not expose Close.
const mcpCloseTimeout = 5 * time.Second

type ownedMCPTransport struct {
	mcp.Transport
	operations  *boundedMCPToolset
	mu          sync.Mutex
	closed      bool
	pending     int
	pendingDone chan struct{}
	pendingErr  error
	nextID      uint64
	cancels     map[uint64]context.CancelFunc
	connections []mcp.Connection
	closeOnce   sync.Once
	closeErr    error
}

func (t *ownedMCPTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil, mcp.ErrConnectionClosed
	}
	t.pending++
	if t.pendingDone == nil {
		t.pendingDone = make(chan struct{})
	}
	// The SDK retains this context for SSE's response body. Link cancellation
	// only while Connect is pending; an established connection belongs to us.
	connectionCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stopCallerCancel := context.AfterFunc(ctx, cancel)
	id := t.nextID
	t.nextID++
	if t.cancels == nil {
		t.cancels = make(map[uint64]context.CancelFunc)
	}
	t.cancels[id] = cancel
	t.mu.Unlock()

	defer t.finishConnect()
	conn, err := t.Transport.Connect(connectionCtx)
	stopCallerCancel()
	if ctx.Err() != nil {
		err = errors.Join(err, ctx.Err())
	}
	t.mu.Lock()
	if t.closed || err != nil {
		closed := t.closed
		delete(t.cancels, id)
		t.mu.Unlock()
		cancel()
		if conn != nil {
			closeErr := conn.Close()
			err = errors.Join(err, closeErr)
			if closed {
				t.mu.Lock()
				t.pendingErr = errors.Join(t.pendingErr, closeErr)
				t.mu.Unlock()
			}
		}
		if closed {
			err = errors.Join(mcp.ErrConnectionClosed, err)
		}
		return nil, err
	}
	t.connections = append(t.connections, conn)
	t.mu.Unlock()
	return conn, nil
}

// Match the optional capability structurally so newer SDK consumers retain it
// without requiring a dependency upgrade in this module.
func (t *ownedMCPTransport) SupportsProtocolVersion(version string) bool {
	if p, ok := t.Transport.(interface{ SupportsProtocolVersion(version string) bool }); ok {
		return p.SupportsProtocolVersion(version)
	}
	return true
}

func (t *ownedMCPTransport) finishConnect() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.pending--
	if t.closed && t.pending == 0 {
		close(t.pendingDone)
	}
}

func (t *ownedMCPTransport) Close() error {
	t.closeOnce.Do(func() {
		if t.operations != nil {
			t.operations.stop()
		}
		t.mu.Lock()
		t.closed = true
		if t.pendingDone == nil {
			t.pendingDone = make(chan struct{})
		}
		if t.pending == 0 {
			close(t.pendingDone)
		}
		pendingDone := t.pendingDone
		cancels, connections := t.cancels, t.connections
		t.cancels = nil
		t.connections = nil
		t.mu.Unlock()
		for _, cancel := range cancels {
			cancel()
		}
		for _, conn := range connections {
			t.closeErr = errors.Join(t.closeErr, conn.Close())
		}
		timer := time.NewTimer(mcpCloseTimeout)
		defer timer.Stop()
		select {
		case <-pendingDone:
		case <-timer.C:
			t.closeErr = errors.Join(t.closeErr, context.DeadlineExceeded)
		}
		t.mu.Lock()
		t.closeErr = errors.Join(t.closeErr, t.pendingErr)
		t.mu.Unlock()
		if t.operations != nil {
			t.closeErr = errors.Join(t.closeErr, t.operations.Close())
		}
	})
	return t.closeErr
}

type hostedMCPAgent struct {
	agent.Agent
	transports []*ownedMCPTransport
	closeOnce  sync.Once
	closeErr   error
}

func (a *hostedMCPAgent) Close() error {
	a.closeOnce.Do(func() { a.closeErr = closeMCPTransports(a.transports) })
	return a.closeErr
}

func closeMCPTransports(transports []*ownedMCPTransport) error {
	var err error
	for _, transport := range transports {
		err = errors.Join(err, transport.Close())
	}
	return err
}

func buildHostedAgent(cfg hostedagent.Config, resolved map[string]agentconfig.MCPServerConfig, availabilityIndependent ...bool) (agent.Agent, error) {
	toolsets, transports, err := hostedToolsets(cfg.Toolsets, resolved, availabilityIndependent...)
	if err != nil {
		return nil, err
	}
	cfg.Toolsets = toolsets
	ag, err := newHostedAgent(cfg)
	if err != nil {
		return nil, errors.Join(err, closeMCPTransports(transports))
	}
	if len(transports) == 0 {
		return ag, nil
	}
	owned := &hostedMCPAgent{Agent: ag, transports: transports}
	if live, ok := ag.(hostedAgentCapabilities); ok {
		return &liveHostedMCPAgent{hostedMCPAgent: owned, hostedAgentCapabilities: live}, nil
	}
	return owned, nil
}

var _ io.Closer = (*hostedMCPAgent)(nil)

// Keep the hosted agent's public ADK live and workflow node capabilities.
type hostedAgentCapabilities interface {
	RunLive(ctx agent.InvocationContext) (agent.LiveSession, iter.Seq2[*session.Event, error], error)
	RunNode(ctx agent.Context, input any) iter.Seq2[*session.Event, error]
}
type liveHostedMCPAgent struct {
	*hostedMCPAgent
	hostedAgentCapabilities
}

func (a *hostedMCPAgent) FindAgent(name string) agent.Agent {
	if name == a.Name() {
		return a
	}
	return a.Agent.FindAgent(name)
}
func (a *liveHostedMCPAgent) FindAgent(name string) agent.Agent {
	if name == a.Name() {
		return a
	}
	return a.Agent.FindAgent(name)
}
