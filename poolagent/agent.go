package poolagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/normahq/runtime/v2/agentconfig"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"iter"
)

// MemberConfig identifies one provider that can satisfy a pool request.
type MemberConfig struct {
	// Name is the provider ID used when creating the member agent.
	Name string
	// Cfg is the validated provider configuration for the member.
	Cfg agentconfig.Config
}

// AgentCreator constructs one pool member agent on demand.
type AgentCreator interface {
	CreateAgent(ctx context.Context, name string, req AgentRequest) (agent.Agent, error)
}

// AgentRequest contains the normalized request passed to each pool member.
type AgentRequest struct {
	// Name is the per-member runtime agent name.
	Name string
	// Description is the per-member runtime agent description.
	Description string
	// SystemInstructions are forwarded to the selected member agent.
	SystemInstructions string
	// WorkingDirectory is the session working directory for the selected member.
	WorkingDirectory string
}

// PoolExecutor lazily selects and caches the first healthy pool member.
type PoolExecutor struct {
	poolName       string
	members        []MemberConfig
	agentCreator   AgentCreator
	req            AgentRequest
	mu             sync.Mutex
	cachedMember   *ownedPoolMember
	ownedMembers   []*ownedPoolMember
	creating       chan struct{}
	creationCancel context.CancelFunc
	closed         bool
	pendingErr     error
	closeOnce      sync.Once
	closeErr       error
}

type ownedPoolMember struct {
	agent.Agent
	cancel context.CancelFunc
}

const poolCloseTimeout = 5 * time.Second

// NewPoolExecutor creates a pool executor for ordered failover across members.
func NewPoolExecutor(poolName string, members []MemberConfig, agentCreator AgentCreator, req AgentRequest) *PoolExecutor {
	return &PoolExecutor{
		poolName:     poolName,
		members:      members,
		agentCreator: agentCreator,
		req:          req,
	}
}

// Agent returns the cached healthy agent or creates the first successful pool
// member in configured order.
func (p *PoolExecutor) Agent(ctx context.Context) (agent.Agent, error) {
	member, err := p.member(ctx)
	if err != nil {
		return nil, err
	}
	return member.Agent, nil
}

// member serializes creation without holding mu across an external constructor.
func (p *PoolExecutor) member(ctx context.Context) (*ownedPoolMember, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("pool is closed")
		}
		if p.cachedMember != nil {
			member := p.cachedMember
			p.mu.Unlock()
			return member, nil
		}
		if p.creating != nil {
			done := p.creating
			p.mu.Unlock()
			select {
			case <-done:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		createCtx, cancel := context.WithCancel(ctx)
		done := make(chan struct{})
		p.creating = done
		p.creationCancel = cancel
		p.mu.Unlock()

		inner, err := p.createMember(createCtx)
		if err != nil {
			cancel()
		}
		p.mu.Lock()
		closed := p.closed
		if !closed {
			var member *ownedPoolMember
			if err == nil {
				member = &ownedPoolMember{Agent: inner, cancel: cancel}
				p.cachedMember = member
				p.ownedMembers = append(p.ownedMembers, member)
			}
			p.creating = nil
			p.creationCancel = nil
			close(done)
			p.mu.Unlock()
			return member, err
		}
		p.mu.Unlock()
		// Keep this slot registered until late construction cleanup completes.
		cancel()
		var closeErr error
		if closer, ok := inner.(io.Closer); ok {
			closeErr = closer.Close()
		}
		p.mu.Lock()
		p.pendingErr = errors.Join(p.pendingErr, closeErr)
		p.creating = nil
		p.creationCancel = nil
		close(done)
		p.mu.Unlock()
		return nil, errors.Join(errors.New("pool is closed"), err, closeErr)
	}
}

func (p *PoolExecutor) createMember(ctx context.Context) (agent.Agent, error) {
	var lastErr error
	attemptErrors := make([]AttemptError, 0, len(p.members))
	for i, member := range p.members {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		req := p.req
		req.Name = p.poolName + "_" + member.Name
		inner, err := p.agentCreator.CreateAgent(ctx, member.Name, req)
		if err != nil {
			attemptErrors = append(attemptErrors, AttemptError{Member: member.Name, Index: i, Err: fmt.Sprintf("create agent %q: %v", member.Name, err)})
			lastErr = fmt.Errorf("pool %q: all members failed", p.poolName)
			continue
		}
		return inner, nil
	}
	return nil, &AllPoolMembersFailedError{PoolName: p.poolName, MemberNames: p.memberNames(), Errors: attemptErrors, Err: lastErr}
}

func (p *PoolExecutor) memberNames() string {
	names := make([]string, len(p.members))
	for i, m := range p.members {
		names[i] = m.Name
	}
	return strings.Join(names, ", ")
}

// Close prevents further creation and closes all members owned by this pool.
func (p *PoolExecutor) Close() error {
	p.closeOnce.Do(func() {
		p.mu.Lock()
		p.closed = true
		cancel, done, members := p.creationCancel, p.creating, p.ownedMembers
		p.cachedMember = nil
		p.ownedMembers = nil
		p.mu.Unlock()
		if cancel != nil {
			cancel()
		}
		for _, member := range members {
			if closer, ok := member.Agent.(io.Closer); ok {
				p.closeErr = errors.Join(p.closeErr, closer.Close())
			}
			member.cancel()
		}
		if done != nil {
			timer := time.NewTimer(poolCloseTimeout)
			defer timer.Stop()
			select {
			case <-done:
			case <-timer.C:
				p.closeErr = errors.Join(p.closeErr, context.DeadlineExceeded)
			}
		}
		p.mu.Lock()
		p.closeErr = errors.Join(p.closeErr, p.pendingErr)
		p.mu.Unlock()
	})
	return p.closeErr
}

// A failed member remains owned until Close. Matching the cache entry prevents
// a late error from an older invocation from evicting its replacement.
func (p *PoolExecutor) discard(member *ownedPoolMember) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cachedMember == member {
		p.cachedMember = nil
	}
}

// AttemptError describes one failed member creation attempt.
type AttemptError struct {
	// Member is the member provider ID.
	Member string
	// Index is the zero-based member position in the configured failover order.
	Index int
	// Err is the member-specific failure text.
	Err string
}

// AllPoolMembersFailedError reports that every member failed to initialize.
type AllPoolMembersFailedError struct {
	// PoolName is the logical pool identifier.
	PoolName string
	// MemberNames is a comma-separated list of configured member IDs.
	MemberNames string
	// Errors contains the individual member failures in evaluation order.
	Errors []AttemptError
	// Err is the underlying summary error for errors.Is and errors.Unwrap.
	Err error
}

// Error formats the full failover failure report.
func (e *AllPoolMembersFailedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "pool %q: all %d members failed\n", e.PoolName, len(e.Errors))
	for _, ae := range e.Errors {
		fmt.Fprintf(&b, "  [%d] %s: %s\n", ae.Index+1, ae.Member, ae.Err)
	}
	return b.String()
}

// Unwrap returns the summary error for the failover failure.
func (e *AllPoolMembersFailedError) Unwrap() error {
	return e.Err
}

// PoolAgent exposes pool failover through the ADK agent interface.
type PoolAgent struct {
	agent.Agent
	executor *PoolExecutor
}

// NewPoolAgent creates an ADK agent that runs through a failover pool.
func NewPoolAgent(ctx context.Context, poolName string, members []MemberConfig, req AgentRequest, agentCreator AgentCreator) (*PoolAgent, error) {
	executor := NewPoolExecutor(poolName, members, agentCreator, req)

	_, err := executor.Agent(ctx)
	if err != nil {
		return nil, err
	}

	poolAgent := &PoolAgent{
		executor: executor,
	}

	base, err := agent.New(agent.Config{
		Name:        poolName,
		Description: fmt.Sprintf("Pool agent with %d members: %s", len(members), executor.memberNames()),
		Run:         poolAgent.run,
		SubAgents:   nil,
	})
	if err != nil {
		_ = executor.Close()
		return nil, fmt.Errorf("create adk pool agent: %w", err)
	}
	poolAgent.Agent = base
	return poolAgent, nil
}

func (p *PoolAgent) run(ctx agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		currentMember, err := p.executor.member(ctx)
		if err != nil {
			yield(nil, err)
			return
		}

		for done := false; !done; {
			run := currentMember.Run(ctx)
			retryAgent := false
			for ev, err := range run {
				if err != nil {
					p.executor.discard(currentMember)

					var retryErr error
					currentMember, retryErr = p.executor.member(ctx)
					if retryErr != nil {
						yield(nil, retryErr)
						return
					}
					retryAgent = true
					break
				}
				if !yield(ev, nil) {
					done = true
					break
				}
			}
			if retryAgent {
				continue
			}
			return
		}
	}
}

// Close closes the currently cached pool member.
func (p *PoolAgent) Close() error {
	return p.executor.Close()
}

var _ agent.Agent = (*PoolAgent)(nil)
