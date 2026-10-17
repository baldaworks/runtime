package poolagent

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/normahq/go-adk-acpagent/v2/acperror"

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
	cooldowns      map[string]time.Time
	retired        map[string]struct{}
	generations    map[string]int
	now            func() time.Time
}

type ownedPoolMember struct {
	agent.Agent
	cancel context.CancelFunc
	name   string
	index  int
	// generation numbers the instances created for one member name, so a late
	// failure of a replaced instance cannot penalize its replacement.
	generation int
	// active counts invocations running on this instance. A retired instance
	// is closed as soon as the last of them ends.
	active  int
	retired bool
}

const poolCloseTimeout = 5 * time.Second

// memberCooldown is how long a member that failed at run time is skipped
// before the pool probes it again, so the first member can take over again.
const (
	memberCooldown       = 5 * time.Minute
	quotaCooldown        = time.Hour
	minimumRetryCooldown = 30 * time.Second
	maximumRetryCooldown = 24 * time.Hour
)

// NewPoolExecutor creates a pool executor for ordered failover across members.
func NewPoolExecutor(poolName string, members []MemberConfig, agentCreator AgentCreator, req AgentRequest) *PoolExecutor {
	return &PoolExecutor{
		poolName:     poolName,
		members:      members,
		now:          time.Now,
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

// member returns the cached member or creates one, without marking it in use.
func (p *PoolExecutor) member(ctx context.Context) (*ownedPoolMember, error) {
	return p.selectMember(ctx, false)
}

// acquireMember selects a member and marks it in use in one step under mu, so
// no other invocation can retire and close it between the choice and the run.
// The caller must release it.
func (p *PoolExecutor) acquireMember(ctx context.Context) (*ownedPoolMember, error) {
	return p.selectMember(ctx, true)
}

// selectMember serializes creation without holding mu across an external
// constructor.
func (p *PoolExecutor) selectMember(ctx context.Context, acquire bool) (*ownedPoolMember, error) {
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("pool is closed")
		}
		if p.cachedMember != nil {
			member := p.cachedMember
			if acquire {
				member.active++
			}
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

		inner, idx, err := p.createMember(createCtx)
		if err != nil {
			cancel()
		}
		p.mu.Lock()
		closed := p.closed
		if !closed {
			var member *ownedPoolMember
			if err == nil {
				name := p.members[idx].Name
				if p.generations == nil {
					p.generations = make(map[string]int)
				}
				p.generations[name]++
				member = &ownedPoolMember{Agent: inner, cancel: cancel, name: name, index: idx, generation: p.generations[name]}
				if acquire {
					member.active++
				}
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

// createMember builds the first member that can be created. Members that are
// cooling down are skipped; a member that cannot be created starts a cooldown
// so it is not probed again on every turn.
func (p *PoolExecutor) createMember(ctx context.Context) (agent.Agent, int, error) {
	var lastErr error
	attemptErrors := make([]AttemptError, 0, len(p.members))
	for i, member := range p.members {
		if p.isRetired(member.Name) {
			attemptErrors = append(attemptErrors, AttemptError{Member: member.Name, Index: i, Err: "retired until pool recreation"})
			lastErr = fmt.Errorf("pool %q: all members failed", p.poolName)
			continue
		}
		if until, cooling := p.cooldownUntil(member.Name); cooling {
			attemptErrors = append(attemptErrors, AttemptError{Member: member.Name, Index: i, Err: fmt.Sprintf("cooling down until %s", until.Format(time.RFC3339))})
			lastErr = fmt.Errorf("pool %q: all members failed", p.poolName)
			continue
		}
		if err := ctx.Err(); err != nil {
			return nil, 0, err
		}
		req := p.req
		req.Name = p.poolName + "_" + member.Name
		inner, err := p.agentCreator.CreateAgent(ctx, member.Name, req)
		if err != nil {
			// A canceled creation says nothing about the member's health.
			if ctxErr := ctx.Err(); ctxErr != nil {
				return nil, 0, ctxErr
			}
			p.startCooldown(member.Name)
			attemptErrors = append(attemptErrors, AttemptError{Member: member.Name, Index: i, Err: fmt.Sprintf("create agent %q: %v", member.Name, err)})
			lastErr = fmt.Errorf("pool %q: all members failed", p.poolName)
			continue
		}
		p.clearCooldown(member.Name)
		return inner, i, nil
	}
	return nil, 0, &AllPoolMembersFailedError{PoolName: p.poolName, MemberNames: p.memberNames(), Errors: attemptErrors, Err: lastErr}
}

// cooldownUntil reports whether the named member is still cooling down.
func (p *PoolExecutor) cooldownUntil(name string) (time.Time, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	until, ok := p.cooldowns[name]
	return until, ok && p.now().Before(until)
}

func (p *PoolExecutor) coolingDown(name string) bool {
	_, cooling := p.cooldownUntil(name)
	return cooling
}

func (p *PoolExecutor) isRetired(name string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	_, retired := p.retired[name]
	return retired
}

// startCooldown keeps the member out of selection for memberCooldown. A pool
// with a single member has nothing to fall back to, so it never cools down.
func (p *PoolExecutor) startCooldown(name string) {
	if len(p.members) < 2 {
		return
	}
	p.startCooldownUntil(name, p.now().Add(memberCooldown))
}

func (p *PoolExecutor) startCooldownUntil(name string, until time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cooldowns == nil {
		p.cooldowns = make(map[string]time.Time)
	}
	if current := p.cooldowns[name]; current.After(until) {
		return
	}
	p.cooldowns[name] = until
}

func (p *PoolExecutor) retireMember(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.retired == nil {
		p.retired = make(map[string]struct{})
	}
	p.retired[name] = struct{}{}
	delete(p.cooldowns, name)
}

func (p *PoolExecutor) clearCooldown(name string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	delete(p.cooldowns, name)
}

// fail applies the health action chosen independently from turn failover. A
// failure of an instance that was already replaced does not penalize its
// replacement.
func (p *PoolExecutor) fail(member *ownedPoolMember, failure *memberFailure) {
	p.mu.Lock()
	current := p.generations[member.name] == member.generation
	p.mu.Unlock()
	if !current {
		p.discard(member)
		return
	}
	switch failure.health {
	case memberHealthCooldown:
		if failure.providerErr == nil {
			p.startCooldown(member.name)
		} else {
			p.startCooldownUntil(member.name, failure.cooldownUntil(p.now()))
		}
		p.discard(member)
	case memberHealthRetire:
		p.retireMember(member.name)
		p.discard(member)
	case memberHealthNone:
	}
}

// refreshPrimary drops a cached fallback member once the cooldown of a
// higher-priority member has ended, so the pool probes the preferred provider
// again. A higher-priority member without a cooldown record is never dropped
// for: it only gets that far by failing, which records a cooldown.
func (p *PoolExecutor) refreshPrimary() {
	p.mu.Lock()
	if p.cachedMember == nil {
		p.mu.Unlock()
		return
	}
	for i := 0; i < p.cachedMember.index; i++ {
		if _, retired := p.retired[p.members[i].Name]; retired {
			continue
		}
		until, ok := p.cooldowns[p.members[i].Name]
		if ok && !p.now().Before(until) {
			member := p.cachedMember
			p.cachedMember = nil
			p.mu.Unlock()
			p.retire(member)
			return
		}
	}
	p.mu.Unlock()
}

// release ends an invocation and closes the member if it was retired meanwhile.
func (p *PoolExecutor) release(member *ownedPoolMember) {
	p.mu.Lock()
	member.active--
	closeNow := member.retired && member.active == 0
	p.mu.Unlock()
	if closeNow {
		p.closeMember(member)
	}
}

// retire marks a member that left the cache as finished and closes it once no
// invocation runs on it, so failed and replaced members do not accumulate.
func (p *PoolExecutor) retire(member *ownedPoolMember) {
	p.mu.Lock()
	member.retired = true
	closeNow := member.active == 0
	p.mu.Unlock()
	if closeNow {
		p.closeMember(member)
	}
}

// closeMember releases one member. It is a no-op when Close already took
// ownership of the member.
func (p *PoolExecutor) closeMember(member *ownedPoolMember) {
	p.mu.Lock()
	owned := false
	for i, m := range p.ownedMembers {
		if m == member {
			p.ownedMembers = append(p.ownedMembers[:i], p.ownedMembers[i+1:]...)
			owned = true
			break
		}
	}
	p.mu.Unlock()
	if !owned {
		return
	}
	var closeErr error
	if closer, ok := member.Agent.(io.Closer); ok {
		closeErr = closer.Close()
	}
	member.cancel()
	p.mu.Lock()
	p.pendingErr = errors.Join(p.pendingErr, closeErr)
	p.mu.Unlock()
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

// discard removes the member from the cache and retires it. Matching the cache
// entry prevents a late error from an older invocation from evicting its
// replacement.
func (p *PoolExecutor) discard(member *ownedPoolMember) {
	p.mu.Lock()
	if p.cachedMember != member {
		p.mu.Unlock()
		return
	}
	p.cachedMember = nil
	p.mu.Unlock()
	p.retire(member)
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
		yield = p.asPoolAuthor(yield)
		p.executor.refreshPrimary()
		currentMember, err := p.executor.acquireMember(ctx)
		if err != nil {
			yield(nil, err)
			return
		}

		// A single-member pool retries once: recreating the member can recover
		// from a transient failure, and there is no other member to fall to.
		maxAttempts := max(len(p.executor.members), 2)
		for attempt := 1; ; attempt++ {
			failure := p.runAcquired(ctx, currentMember, yield)
			if failure == nil {
				return
			}
			// A canceled invocation says nothing about the member's health.
			if ctx.Err() != nil {
				failure.yield(yield)
				return
			}
			p.executor.fail(currentMember, failure)
			// Output already reached the consumer, so rerunning the turn on
			// another member would duplicate it.
			if failure.afterOutput || !failure.failover || attempt >= maxAttempts || (failure.event != nil && len(p.executor.members) == 1) {
				failure.yield(yield)
				return
			}
			currentMember, err = p.executor.acquireMember(ctx)
			if err != nil {
				failure.yieldWithCause(yield, err)
				return
			}
		}
	}
}

// asPoolAuthor presents every event as authored by the pool itself. Members
// are an implementation detail of the failover, and the session history should
// read as one agent no matter which member produced a turn; the member names
// would otherwise show up as separate speakers in the conversation.
func (p *PoolAgent) asPoolAuthor(yield func(*session.Event, error) bool) func(*session.Event, error) bool {
	if p.Agent == nil {
		return yield
	}
	name := p.Name()
	return func(ev *session.Event, err error) bool {
		if ev != nil {
			ev.Author = name
		}
		return yield(ev, err)
	}
}

// runAcquired runs a member selected with acquireMember and always releases it,
// even when the consumer stops early or the member panics.
func (p *PoolAgent) runAcquired(ctx agent.InvocationContext, member *ownedPoolMember, yield func(*session.Event, error) bool) *memberFailure {
	defer p.executor.release(member)
	return runMember(ctx, member, yield)
}

// memberFailure is the error or provider error event that ended a member run,
// plus the independent replay and member-health decisions derived from it.
type memberFailure struct {
	err   error
	event *session.Event
	// afterOutput reports that the member already produced content.
	afterOutput bool
	failover    bool
	health      memberHealthAction
	providerErr *acperror.ProviderError
}

type memberHealthAction uint8

const (
	memberHealthNone memberHealthAction = iota
	memberHealthCooldown
	memberHealthRetire
)

func (f *memberFailure) cooldownUntil(now time.Time) time.Time {
	duration := memberCooldown
	if f.providerErr != nil && f.providerErr.Kind == acperror.KindQuotaExceeded {
		duration = quotaCooldown
	}
	if f.providerErr == nil || f.providerErr.RetryAt == nil {
		return now.Add(duration)
	}
	retryAt := time.Unix(*f.providerErr.RetryAt, 0)
	if !retryAt.After(now) {
		return now.Add(duration)
	}
	duration = retryAt.Sub(now)
	duration = max(duration, minimumRetryCooldown)
	duration = min(duration, maximumRetryCooldown)
	return now.Add(duration)
}

func classifyMemberFailure(ev *session.Event) (bool, memberHealthAction, *acperror.ProviderError) {
	if ev == nil || ev.ErrorCode == "" {
		return false, memberHealthNone, nil
	}
	providerErr, typed := acperror.FromADKMetadata(ev.CustomMetadata)
	if !typed {
		if _, present := ev.CustomMetadata[acperror.ProviderErrorMetadataKey]; present {
			return false, memberHealthNone, nil
		}
		return true, memberHealthCooldown, nil
	}
	healthScope := providerErr.Scope == acperror.ScopeAccount || providerErr.Scope == acperror.ScopeProvider
	if !healthScope {
		return false, memberHealthNone, providerErr
	}
	switch providerErr.Kind {
	case acperror.KindAuthenticationRequired, acperror.KindPaymentRequired:
		return true, memberHealthRetire, providerErr
	case acperror.KindQuotaExceeded, acperror.KindRateLimited, acperror.KindUnavailable:
		return true, memberHealthCooldown, providerErr
	default:
		return false, memberHealthNone, providerErr
	}
}

func (f *memberFailure) yield(yield func(*session.Event, error) bool) {
	yield(f.event, f.err)
}

// yieldWithCause reports the failure when no other member could take over. The
// member's own error or provider error event is what the caller needs to see;
// the selection error is attached to a Go error and dropped for an event.
func (f *memberFailure) yieldWithCause(yield func(*session.Event, error) bool, selection error) {
	if f.err != nil {
		yield(nil, errors.Join(f.err, selection))
		return
	}
	yield(f.event, nil)
}

// runMember streams one member. It returns a failure when the member returned
// an error, or reported a provider error (for example a usage limit) before
// producing any content, so policy can fail over without duplicating output.
// An error or provider error after content was produced is reported with
// afterOutput set: the turn is not rerun, but its health action still applies.
// Events without content (such as session state updates) are held back until
// the member produces content or finishes, so a failed member leaks none.
func runMember(ctx agent.InvocationContext, member *ownedPoolMember, yield func(*session.Event, error) bool) *memberFailure {
	var held []*session.Event
	produced := false
	for ev, err := range member.Run(ctx) {
		if err != nil {
			return &memberFailure{err: err, afterOutput: produced, failover: true, health: memberHealthCooldown}
		}
		if ev != nil && ev.ErrorCode != "" {
			failover, health, providerErr := classifyMemberFailure(ev)
			return &memberFailure{
				event: ev, afterOutput: produced, failover: failover,
				health: health, providerErr: providerErr,
			}
		}
		if !produced && !hasContent(ev) {
			held = append(held, ev)
			continue
		}
		produced = true
		for _, h := range held {
			if !yield(h, nil) {
				return nil
			}
		}
		held = nil
		if !yield(ev, nil) {
			return nil
		}
	}
	for _, h := range held {
		if !yield(h, nil) {
			return nil
		}
	}
	return nil
}

func hasContent(ev *session.Event) bool {
	return ev != nil && ev.Content != nil && len(ev.Content.Parts) > 0
}

// Close closes the currently cached pool member.
func (p *PoolAgent) Close() error {
	return p.executor.Close()
}

var _ agent.Agent = (*PoolAgent)(nil)
