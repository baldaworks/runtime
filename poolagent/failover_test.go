package poolagent

import (
	"context"
	"errors"
	"iter"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/normahq/go-adk-acpagent/v2/acperror"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

// errCodeUsageLimit is the provider error code of an exhausted usage limit.
const errCodeUsageLimit = "usageLimitExceeded"

type agentMapCreator map[string]agent.Agent

func (c agentMapCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	return c[name], nil
}

type providerErrorMember struct {
	agent.Agent
	providerErr acperror.ProviderError
	beforeError *session.Event
	runs        int
}

func (m *providerErrorMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		m.runs++
		if m.beforeError != nil && !yield(m.beforeError, nil) {
			return
		}
		ev := &session.Event{}
		ev.LLMResponse = model.LLMResponse{ErrorCode: "acp_request_error:-32603", ErrorMessage: "Provider request failed"}
		ev.CustomMetadata = map[string]any{
			acperror.ProviderErrorMetadataKey: m.providerErr.Metadata(),
		}
		yield(ev, nil)
	}
}

type scriptedMember struct {
	agent.Agent
	name      string
	errorCode string
	runs      int
}

func (m *scriptedMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		m.runs++
		state := &session.Event{}
		state.Actions.StateDelta = map[string]any{"acp_session": m.name}
		if !yield(state, nil) {
			return
		}
		ev := &session.Event{}
		if m.errorCode != "" {
			ev.LLMResponse = model.LLMResponse{ErrorCode: m.errorCode, ErrorMessage: m.name + " failed"}
		}
		yield(ev, nil)
	}
}

type namedCreator struct {
	members map[string]*scriptedMember
}

func (c *namedCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	return c.members[name], nil
}

func newScriptedPool(codex, glm *scriptedMember) (*PoolAgent, *PoolExecutor, *time.Time) {
	creator := &namedCreator{members: map[string]*scriptedMember{"codex": codex, "glm": glm}}
	members := []MemberConfig{{Name: "codex"}, {Name: "glm"}}
	executor := NewPoolExecutor("pool", members, creator, AgentRequest{})
	clock := time.Now()
	executor.now = func() time.Time { return clock }
	return &PoolAgent{executor: executor}, executor, &clock
}

func runPool(t *testing.T, pool *PoolAgent) []*session.Event {
	t.Helper()
	var events []*session.Event
	for ev, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	return events
}

func TestPoolFailsOverOnProviderErrorEvent(t *testing.T) {
	codex := &scriptedMember{name: "codex", errorCode: errCodeUsageLimit}
	glm := &scriptedMember{name: "glm"}
	pool, _, _ := newScriptedPool(codex, glm)

	events := runPool(t, pool)
	for _, ev := range events {
		if ev.ErrorCode != "" || ev.Actions.StateDelta["acp_session"] == "codex" {
			t.Fatalf("failed member leaked an event: %+v", ev)
		}
	}
	if len(events) != 2 {
		t.Fatalf("want glm's two events, got %d", len(events))
	}
	if codex.runs != 1 || glm.runs != 1 {
		t.Fatalf("runs codex=%d glm=%d", codex.runs, glm.runs)
	}
}

func TestPoolTypedProviderErrorPolicy(t *testing.T) {
	tests := []struct {
		name         string
		kind         acperror.Kind
		scope        acperror.Scope
		wantFailover bool
		wantCooldown bool
		wantRetired  bool
	}{
		{name: "rate limit", kind: acperror.KindRateLimited, scope: acperror.ScopeAccount, wantFailover: true, wantCooldown: true},
		{name: "unavailable", kind: acperror.KindUnavailable, scope: acperror.ScopeProvider, wantFailover: true, wantCooldown: true},
		{name: "quota", kind: acperror.KindQuotaExceeded, scope: acperror.ScopeAccount, wantFailover: true, wantCooldown: true},
		{name: "authentication", kind: acperror.KindAuthenticationRequired, scope: acperror.ScopeAccount, wantFailover: true, wantRetired: true},
		{name: "payment", kind: acperror.KindPaymentRequired, scope: acperror.ScopeAccount, wantFailover: true, wantRetired: true},
		{name: "session exhausted", kind: acperror.KindSessionExhausted, scope: acperror.ScopeSession},
		{name: "invalid request", kind: acperror.KindInvalidRequest, scope: acperror.ScopeRequest},
		{name: "unknown", kind: acperror.KindUnknown, scope: acperror.ScopeAccount},
		{name: "health kind with session scope", kind: acperror.KindRateLimited, scope: acperror.ScopeSession},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			primary := &providerErrorMember{providerErr: acperror.ProviderError{Kind: tt.kind, Scope: tt.scope}}
			fallback := &scriptedMember{name: "fallback"}
			executor := NewPoolExecutor("pool", []MemberConfig{{Name: "primary"}, {Name: "fallback"}}, agentMapCreator{
				"primary": primary, "fallback": fallback,
			}, AgentRequest{})
			pool := &PoolAgent{executor: executor}

			events := runPool(t, pool)
			if got := fallback.runs > 0; got != tt.wantFailover {
				t.Fatalf("fallback used = %t, want %t", got, tt.wantFailover)
			}
			if got := executor.coolingDown("primary"); got != tt.wantCooldown {
				t.Fatalf("cooling down = %t, want %t", got, tt.wantCooldown)
			}
			if got := executor.isRetired("primary"); got != tt.wantRetired {
				t.Fatalf("retired = %t, want %t", got, tt.wantRetired)
			}
			if !tt.wantFailover && (len(events) != 1 || events[0].ErrorCode == "") {
				t.Fatalf("terminal provider error was not returned: %+v", events)
			}
		})
	}
}

func TestPoolMalformedTypedProviderErrorFailsClosed(t *testing.T) {
	eventMember := &providerErrorMember{providerErr: acperror.ProviderError{}}
	fallback := &scriptedMember{name: "fallback"}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "primary"}, {Name: "fallback"}}, agentMapCreator{
		"primary": eventMember, "fallback": fallback,
	}, AgentRequest{})
	pool := &PoolAgent{executor: executor}

	events := runPool(t, pool)
	if len(events) != 1 || events[0].ErrorCode == "" {
		t.Fatalf("malformed typed error was not returned: %+v", events)
	}
	if fallback.runs != 0 {
		t.Fatalf("malformed typed error triggered failover: runs=%d", fallback.runs)
	}
	if executor.coolingDown("primary") || executor.isRetired("primary") {
		t.Fatal("malformed typed error changed member health")
	}
}

func TestPoolTypedCooldownUsesBoundedRetryAt(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	tests := []struct {
		name    string
		kind    acperror.Kind
		retryAt *int64
		want    time.Duration
	}{
		{name: "transient default", kind: acperror.KindRateLimited, want: memberCooldown},
		{name: "quota default", kind: acperror.KindQuotaExceeded, want: quotaCooldown},
		{name: "future reset", kind: acperror.KindRateLimited, retryAt: int64Ptr(now.Add(2 * time.Hour).Unix()), want: 2 * time.Hour},
		{name: "near reset bounded", kind: acperror.KindRateLimited, retryAt: int64Ptr(now.Add(time.Second).Unix()), want: minimumRetryCooldown},
		{name: "far reset bounded", kind: acperror.KindQuotaExceeded, retryAt: int64Ptr(now.Add(7 * 24 * time.Hour).Unix()), want: maximumRetryCooldown},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			failure := &memberFailure{providerErr: &acperror.ProviderError{Kind: tt.kind, RetryAt: tt.retryAt}}
			if got := failure.cooldownUntil(now).Sub(now); got != tt.want {
				t.Fatalf("cooldown = %s, want %s", got, tt.want)
			}
		})
	}
}

func TestPoolCooldownDeadlineNeverMovesEarlier(t *testing.T) {
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "primary"}, {Name: "fallback"}}, agentMapCreator{}, AgentRequest{})
	now := time.Unix(1_800_000_000, 0)
	executor.now = func() time.Time { return now }
	long := now.Add(maximumRetryCooldown)
	executor.startCooldownUntil("primary", long)
	executor.startCooldownUntil("primary", now.Add(memberCooldown))

	until, cooling := executor.cooldownUntil("primary")
	if !cooling || !until.Equal(long) {
		t.Fatalf("cooldown shortened to %s, want %s", until, long)
	}
}

func TestSingleMemberTypedTerminalErrorIsNotReplayedAndStartsCooldown(t *testing.T) {
	member := &providerErrorMember{providerErr: acperror.ProviderError{
		Kind: acperror.KindRateLimited, Scope: acperror.ScopeAccount,
	}}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "only"}}, agentMapCreator{"only": member}, AgentRequest{})
	pool := &PoolAgent{executor: executor}

	events := runPool(t, pool)
	if member.runs != 1 || len(events) != 1 || events[0].ErrorCode == "" {
		t.Fatalf("terminal event was replayed or lost: runs=%d events=%+v", member.runs, events)
	}
	if !executor.coolingDown("only") {
		t.Fatal("typed provider cooldown was not applied to a single-member pool")
	}
}

func int64Ptr(value int64) *int64 { return &value }

func TestPoolDoesNotReplayAfterToolEvent(t *testing.T) {
	tool := &session.Event{}
	tool.Content = &genai.Content{Role: genai.RoleModel, Parts: []*genai.Part{
		genai.NewPartFromFunctionCall("write_file", map[string]any{"path": "result.txt"}),
	}}
	primary := &providerErrorMember{
		providerErr: acperror.ProviderError{Kind: acperror.KindUnavailable, Scope: acperror.ScopeProvider},
		beforeError: tool,
	}
	fallback := &scriptedMember{name: "fallback"}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "primary"}, {Name: "fallback"}}, agentMapCreator{
		"primary": primary, "fallback": fallback,
	}, AgentRequest{})
	pool := &PoolAgent{executor: executor}

	events := runPool(t, pool)
	if len(events) != 2 || events[0].Content == nil || events[1].ErrorCode == "" {
		t.Fatalf("want tool event followed by terminal error, got %+v", events)
	}
	if fallback.runs != 0 {
		t.Fatalf("turn replayed after tool event: fallback runs=%d", fallback.runs)
	}
	if !executor.coolingDown("primary") {
		t.Fatal("provider health cooldown was not applied after the terminal error")
	}
}

func TestPoolSkipsCoolingMemberThenReturnsToPrimary(t *testing.T) {
	codex := &scriptedMember{name: "codex", errorCode: errCodeUsageLimit}
	glm := &scriptedMember{name: "glm"}
	pool, _, clock := newScriptedPool(codex, glm)

	runPool(t, pool)
	runPool(t, pool)
	if codex.runs != 1 || glm.runs != 2 {
		t.Fatalf("codex must be skipped while cooling: codex=%d glm=%d", codex.runs, glm.runs)
	}

	*clock = clock.Add(memberCooldown + time.Second)
	codex.errorCode = ""
	runPool(t, pool)
	if codex.runs != 2 {
		t.Fatalf("pool must return to codex after cooldown: codex=%d", codex.runs)
	}
}

func TestPoolSurfacesErrorWhenEveryMemberFails(t *testing.T) {
	codex := &scriptedMember{name: "codex", errorCode: errCodeUsageLimit}
	glm := &scriptedMember{name: "glm", errorCode: "rateLimited"}
	pool, _, _ := newScriptedPool(codex, glm)

	events := runPool(t, pool)
	last := events[len(events)-1]
	if len(events) != 1 || last.ErrorCode != "rateLimited" {
		t.Fatalf("want only last member's error event, got %+v", events)
	}
}

type erroringMember struct {
	agent.Agent
	runs      int
	afterText bool
}

func (m *erroringMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		m.runs++
		if m.afterText {
			ev := &session.Event{}
			ev.Content = genai.NewContentFromText("partial", genai.RoleModel)
			if !yield(ev, nil) {
				return
			}
		}
		yield(nil, errors.New("boom"))
	}
}

func newErroringPool(first *erroringMember, second *scriptedMember) (*PoolAgent, *PoolExecutor) {
	creator := &mixedCreator{first: first, second: second}
	members := []MemberConfig{{Name: "first"}, {Name: "second"}}
	executor := NewPoolExecutor("pool", members, creator, AgentRequest{})
	return &PoolAgent{executor: executor}, executor
}

type mixedCreator struct {
	first  *erroringMember
	second *scriptedMember
}

func (c *mixedCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	if name == "first" {
		return c.first, nil
	}
	return c.second, nil
}

func TestPoolDoesNotRerunTurnAfterOutput(t *testing.T) {
	first := &erroringMember{afterText: true}
	second := &scriptedMember{name: "second"}
	pool, _ := newErroringPool(first, second)

	var events int
	var gotErr error
	for _, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			gotErr = err
			continue
		}
		events++
	}
	if gotErr == nil || events != 1 {
		t.Fatalf("want one event then the error, got events=%d err=%v", events, gotErr)
	}
	if second.runs != 0 {
		t.Fatalf("turn was rerun after output reached the consumer: runs=%d", second.runs)
	}
}

func TestPoolCanceledRunDoesNotPenalizeMember(t *testing.T) {
	first := &erroringMember{}
	second := &scriptedMember{name: "second"}
	pool, executor := newErroringPool(first, second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for range pool.run(retryContext{Context: ctx}) {
	}
	if executor.coolingDown("first") {
		t.Fatal("a canceled invocation put the member into cooldown")
	}
	if second.runs != 0 {
		t.Fatalf("canceled invocation failed over: runs=%d", second.runs)
	}
}

func TestPoolErrorBeforeOutputFailsOver(t *testing.T) {
	first := &erroringMember{}
	second := &scriptedMember{name: "second"}
	pool, executor := newErroringPool(first, second)

	runPool(t, pool)
	if first.runs != 1 || second.runs != 1 {
		t.Fatalf("runs first=%d second=%d", first.runs, second.runs)
	}
	if !executor.coolingDown("first") {
		t.Fatal("failed member must be cooling down")
	}
}

type countingCreator struct {
	failing map[string]bool
	created map[string]int
	members map[string]*scriptedMember
}

func (c *countingCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	c.created[name]++
	if c.failing[name] {
		return nil, errors.New("cannot start")
	}
	return c.members[name], nil
}

func newCountingPool(failing ...string) (*PoolAgent, *PoolExecutor, *countingCreator, *time.Time) {
	creator := &countingCreator{
		failing: map[string]bool{},
		created: map[string]int{},
		members: map[string]*scriptedMember{
			"primary":  {name: "primary"},
			"fallback": {name: "fallback"},
		},
	}
	for _, name := range failing {
		creator.failing[name] = true
	}
	members := []MemberConfig{{Name: "primary"}, {Name: "fallback"}}
	executor := NewPoolExecutor("pool", members, creator, AgentRequest{})
	clock := time.Now()
	executor.now = func() time.Time { return clock }
	return &PoolAgent{executor: executor}, executor, creator, &clock
}

func TestPoolCreationFailureHonorsCooldown(t *testing.T) {
	pool, _, creator, clock := newCountingPool("primary")

	runPool(t, pool)
	runPool(t, pool)
	if creator.created["primary"] != 1 || creator.created["fallback"] != 1 {
		t.Fatalf("members must not be recreated every turn: %v", creator.created)
	}

	*clock = clock.Add(memberCooldown + time.Second)
	runPool(t, pool)
	if creator.created["primary"] != 2 {
		t.Fatalf("primary must be probed once its cooldown ended: %v", creator.created)
	}
}

func TestPoolDoesNotProbeCoolingMembers(t *testing.T) {
	pool, executor, creator, _ := newCountingPool()
	executor.startCooldown("primary")
	executor.startCooldown("fallback")

	var gotErr error
	for _, err := range pool.run(retryContext{Context: context.Background()}) {
		gotErr = err
	}
	var all *AllPoolMembersFailedError
	if !errors.As(gotErr, &all) {
		t.Fatalf("want AllPoolMembersFailedError, got %v", gotErr)
	}
	if len(creator.created) != 0 {
		t.Fatalf("cooling members were probed: %v", creator.created)
	}
}

func TestSingleMemberPoolNeverCoolsDown(t *testing.T) {
	creator := &countingCreator{
		failing: map[string]bool{},
		created: map[string]int{},
		members: map[string]*scriptedMember{"only": {name: "only"}},
	}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "only"}}, creator, AgentRequest{})
	executor.startCooldown("only")
	if executor.coolingDown("only") {
		t.Fatal("a pool without alternatives must not cool its only member down")
	}
}

func TestPoolErrorAfterOutputIsTerminal(t *testing.T) {
	first := &erroringMember{afterText: true}
	second := &scriptedMember{name: "second"}
	pool, _ := newErroringPool(first, second)

	var events int
	var errs []error
	for ev, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if ev.Content != nil {
			events++
		}
	}
	if events != 1 || len(errs) != 1 || errs[0].Error() != "boom" {
		t.Fatalf("want one event then the original error, got events=%d errs=%v", events, errs)
	}
}

type partialThenErrorMember struct {
	agent.Agent
	runs int
}

func (m *partialThenErrorMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		m.runs++
		text := &session.Event{}
		text.Content = genai.NewContentFromText("partial", genai.RoleModel)
		if !yield(text, nil) {
			return
		}
		failed := &session.Event{}
		failed.LLMResponse = model.LLMResponse{ErrorCode: errCodeUsageLimit, ErrorMessage: "limit"}
		yield(failed, nil)
	}
}

type partialCreator struct {
	first  *partialThenErrorMember
	second *scriptedMember
}

func (c *partialCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	if name == "first" {
		return c.first, nil
	}
	return c.second, nil
}

func TestPoolProviderErrorAfterOutputStartsCooldownWithoutRerun(t *testing.T) {
	first := &partialThenErrorMember{}
	second := &scriptedMember{name: "second"}
	creator := &partialCreator{first: first, second: second}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "first"}, {Name: "second"}}, creator, AgentRequest{})
	pool := &PoolAgent{executor: executor}

	var events []*session.Event
	for ev, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, ev)
	}
	if len(events) != 2 || events[1].ErrorCode != errCodeUsageLimit {
		t.Fatalf("want the partial text and then the provider error, got %d events", len(events))
	}
	if second.runs != 0 {
		t.Fatalf("turn was rerun on the fallback after output: runs=%d", second.runs)
	}
	if !executor.coolingDown("first") {
		t.Fatal("a provider that failed after output must start a cooldown")
	}

	runPool(t, pool)
	if first.runs != 1 || second.runs != 1 {
		t.Fatalf("next turn must go to the fallback: first=%d second=%d", first.runs, second.runs)
	}
}

type cancelingCreator struct{ cancel context.CancelFunc }

func (c *cancelingCreator) CreateAgent(ctx context.Context, _ string, _ AgentRequest) (agent.Agent, error) {
	c.cancel()
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestPoolCanceledCreationDoesNotStartCooldown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	creator := &cancelingCreator{cancel: cancel}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "a"}, {Name: "b"}}, creator, AgentRequest{})

	if _, err := executor.member(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("member() error = %v, want context.Canceled", err)
	}
	if executor.coolingDown("a") || executor.coolingDown("b") {
		t.Fatal("a canceled creation put a member into cooldown")
	}
}

type closableScripted struct {
	scriptedMember
	closes int
}

func (m *closableScripted) Close() error { m.closes++; return nil }

type closableCreator struct {
	created []*closableScripted
	spec    map[string]string
}

func (c *closableCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	m := &closableScripted{scriptedMember: scriptedMember{name: name, errorCode: c.spec[name]}}
	c.created = append(c.created, m)
	return m, nil
}

func TestPoolSurfacesProviderErrorWhenNoMemberIsLeft(t *testing.T) {
	creator := &closableCreator{spec: map[string]string{"a": errCodeUsageLimit, "b": errCodeUsageLimit}}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "a"}, {Name: "b"}}, creator, AgentRequest{})
	executor.startCooldown("a")
	pool := &PoolAgent{executor: executor}

	var last *session.Event
	for ev, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			t.Fatalf("want the provider's own error event, got %v", err)
		}
		last = ev
	}
	if last == nil || last.ErrorCode != errCodeUsageLimit {
		t.Fatalf("want the usage limit event, got %+v", last)
	}
}

func TestPoolClosesFailedAndReplacedMembers(t *testing.T) {
	creator := &closableCreator{spec: map[string]string{"primary": errCodeUsageLimit}}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "primary"}, {Name: "fallback"}}, creator, AgentRequest{})
	clock := time.Now()
	executor.now = func() time.Time { return clock }
	pool := &PoolAgent{executor: executor}

	runPool(t, pool)
	if len(creator.created) != 2 || creator.created[0].closes != 1 {
		t.Fatalf("the failed primary must be closed once it failed: created=%d closes=%d", len(creator.created), creator.created[0].closes)
	}

	clock = clock.Add(memberCooldown + time.Second)
	creator.spec["primary"] = ""
	runPool(t, pool)
	if creator.created[1].closes != 1 {
		t.Fatalf("the fallback must be closed when the primary takes over: closes=%d", creator.created[1].closes)
	}
	if got := len(executor.ownedMembers); got != 1 {
		t.Fatalf("only the live primary may stay owned, got %d", got)
	}
	if err := executor.Close(); err != nil {
		t.Fatal(err)
	}
	if creator.created[0].closes != 1 || creator.created[1].closes != 1 {
		t.Fatal("a member was closed twice")
	}
}

func TestPoolLateFailureOfReplacedInstanceKeepsReplacementHealthy(t *testing.T) {
	creator := &closableCreator{spec: map[string]string{}}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "a"}, {Name: "b"}}, creator, AgentRequest{})

	old, err := executor.member(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	executor.discard(old)
	if _, err := executor.member(context.Background()); err != nil {
		t.Fatal(err)
	}
	executor.fail(old, &memberFailure{health: memberHealthCooldown})
	if executor.coolingDown("a") {
		t.Fatal("a late failure of a replaced instance cooled down its healthy replacement")
	}
}

func TestSingleMemberPoolRetriesOnceBeforeOutputOnly(t *testing.T) {
	before := &erroringMember{}
	creator := &repeatCreator{member: before}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "only"}}, creator, AgentRequest{})
	pool := &PoolAgent{executor: executor}
	for range pool.run(retryContext{Context: context.Background()}) {
	}
	if before.runs != 2 {
		t.Fatalf("a failing single member must run exactly twice, ran %d", before.runs)
	}

	after := &erroringMember{afterText: true}
	executor = NewPoolExecutor("pool", []MemberConfig{{Name: "only"}}, &repeatCreator{member: after}, AgentRequest{})
	pool = &PoolAgent{executor: executor}
	for range pool.run(retryContext{Context: context.Background()}) {
	}
	if after.runs != 1 {
		t.Fatalf("a single member that failed after output must not be rerun, ran %d", after.runs)
	}
}

type repeatCreator struct{ member agent.Agent }

func (c *repeatCreator) CreateAgent(context.Context, string, AgentRequest) (agent.Agent, error) {
	return c.member, nil
}

func TestPoolMemberIsInUseFromSelectionOn(t *testing.T) {
	creator := &closableCreator{spec: map[string]string{}}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "a"}, {Name: "b"}}, creator, AgentRequest{})

	selected, err := executor.acquireMember(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	// Another invocation evicts the member right after it was selected.
	executor.discard(selected)
	if creator.created[0].closes != 0 {
		t.Fatal("a selected member was closed before its invocation ran")
	}
	executor.release(selected)
	if creator.created[0].closes != 1 {
		t.Fatalf("the member must close once its last invocation ended: closes=%d", creator.created[0].closes)
	}
}

func TestPoolCachedMemberIsInUseFromSelectionOn(t *testing.T) {
	creator := &closableCreator{spec: map[string]string{}}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "a"}}, creator, AgentRequest{})
	first, err := executor.acquireMember(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	second, err := executor.acquireMember(context.Background())
	if err != nil || second != first {
		t.Fatalf("want the cached member, got %v err=%v", second, err)
	}
	executor.discard(first)
	executor.release(first)
	if creator.created[0].closes != 0 {
		t.Fatal("closed while a second invocation still runs on the member")
	}
	executor.release(second)
	if creator.created[0].closes != 1 {
		t.Fatalf("closes=%d, want 1 after the last release", creator.created[0].closes)
	}
}

func TestPoolConcurrentTurnsDoNotRunClosedMembers(t *testing.T) {
	creator := &concurrentCreator{}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "a"}, {Name: "b"}}, creator, AgentRequest{})
	clock := time.Now()
	var mu sync.Mutex
	executor.now = func() time.Time { mu.Lock(); defer mu.Unlock(); return clock }
	pool := &PoolAgent{executor: executor}

	var wg sync.WaitGroup
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%5 == 0 {
				mu.Lock()
				clock = clock.Add(memberCooldown + time.Second)
				mu.Unlock()
			}
			for range pool.run(retryContext{Context: context.Background()}) {
			}
		}()
	}
	wg.Wait()
	if n := creator.ranClosed.Load(); n != 0 {
		t.Fatalf("%d invocations ran on an already closed member", n)
	}
	if err := executor.Close(); err != nil {
		t.Fatal(err)
	}
}

type concurrentMember struct {
	agent.Agent
	name   string
	closed atomic.Bool
	owner  *concurrentCreator
}

func (m *concurrentMember) Close() error { m.closed.Store(true); return nil }

func (m *concurrentMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if m.closed.Load() {
			m.owner.ranClosed.Add(1)
		}
		time.Sleep(time.Millisecond)
		ev := &session.Event{}
		if m.name == "a" {
			ev.LLMResponse = model.LLMResponse{ErrorCode: errCodeUsageLimit}
		} else {
			ev.Content = genai.NewContentFromText("ok", genai.RoleModel)
		}
		yield(ev, nil)
	}
}

type concurrentCreator struct{ ranClosed atomic.Int32 }

func (c *concurrentCreator) CreateAgent(_ context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	return &concurrentMember{name: name, owner: c}, nil
}

func TestPoolEventsAreAuthoredByThePool(t *testing.T) {
	codex := &scriptedMember{name: "codex", errorCode: errCodeUsageLimit}
	glm := &scriptedMember{name: "glm"}
	creator := &namedCreator{members: map[string]*scriptedMember{"codex": codex, "glm": glm}}
	members := []MemberConfig{{Name: "codex"}, {Name: "glm"}}
	pool, err := NewPoolAgent(context.Background(), "pool", members, AgentRequest{}, creator)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()

	events := runPool(t, pool)
	if len(events) == 0 {
		t.Fatal("no events")
	}
	for _, ev := range events {
		if ev.Author != "pool" {
			t.Fatalf("event author = %q, want the pool name so member names do not appear as speakers", ev.Author)
		}
	}
}

func TestPoolTerminalErrorAfterPartialOutputIsAuthoredByThePool(t *testing.T) {
	first := &partialThenErrorMember{}
	second := &scriptedMember{name: "second"}
	creator := &partialCreator{first: first, second: second}
	members := []MemberConfig{{Name: "first"}, {Name: "second"}}
	pool, err := NewPoolAgent(context.Background(), "pool", members, AgentRequest{}, creator)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = pool.Close() }()

	events := runPool(t, pool)
	if len(events) != 2 {
		t.Fatalf("want the partial text and the terminal provider error, got %d events", len(events))
	}
	for _, ev := range events {
		if ev.Author != "pool" {
			t.Fatalf("event author = %q, want the pool name", ev.Author)
		}
	}
	if events[0].Content == nil || events[1].ErrorCode != errCodeUsageLimit {
		t.Fatalf("want text then the terminal error, got %+v / %+v", events[0], events[1])
	}
	if second.runs != 0 {
		t.Fatalf("the turn was rerun on another member after output: runs=%d", second.runs)
	}
}

// lifetimeMember fails when the context it was constructed with is canceled,
// like an ACP agent whose process dies with its construction context.
type lifetimeMember struct {
	agent.Agent
	constructed context.Context
	runs        int
}

func (m *lifetimeMember) Run(agent.InvocationContext) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		m.runs++
		if err := m.constructed.Err(); err != nil {
			yield(nil, err)
			return
		}
		ev := &session.Event{}
		ev.Content = genai.NewContentFromText("ok", genai.RoleModel)
		yield(ev, nil)
	}
}

type lifetimeCreator struct {
	members map[string]*lifetimeMember
}

func (c *lifetimeCreator) CreateAgent(ctx context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	m := &lifetimeMember{constructed: ctx}
	c.members[name] = m
	return m, nil
}

func TestPoolMemberOutlivesTheInvocationThatCreatedIt(t *testing.T) {
	creator := &lifetimeCreator{members: map[string]*lifetimeMember{}}
	members := []MemberConfig{{Name: "first"}, {Name: "second"}}
	executor := NewPoolExecutor("pool", members, creator, AgentRequest{})
	pool := &PoolAgent{executor: executor}

	first, cancelFirst := context.WithCancel(context.Background())
	for range pool.run(retryContext{Context: first}) {
	}
	cancelFirst() // the turn that created the member is over

	events := 0
	for ev, err := range pool.run(retryContext{Context: context.Background()}) {
		if err != nil {
			t.Fatalf("the next turn failed on the cached member: %v", err)
		}
		if ev.Content != nil {
			events++
		}
	}
	if events != 1 {
		t.Fatalf("want one reply in the second turn, got %d", events)
	}
	if len(creator.members) != 1 || creator.members["first"].runs != 2 {
		t.Fatalf("the same member must serve both turns: members=%d runs=%d", len(creator.members), creator.members["first"].runs)
	}
	if executor.coolingDown("first") {
		t.Fatal("a healthy member was put into cooldown")
	}
}

type abortableCreator struct {
	member *closableScripted
	gotCtx chan context.Context
}

func (c *abortableCreator) CreateAgent(ctx context.Context, _ string, _ AgentRequest) (agent.Agent, error) {
	c.gotCtx <- ctx
	<-ctx.Done()
	return c.member, nil // finished building only after the caller gave up
}

func TestPoolCallerCancelingConstructionDiscardsTheMember(t *testing.T) {
	creator := &abortableCreator{member: &closableScripted{scriptedMember: scriptedMember{name: "a"}}, gotCtx: make(chan context.Context, 1)}
	executor := NewPoolExecutor("pool", []MemberConfig{{Name: "a"}, {Name: "b"}}, creator, AgentRequest{})

	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		_, err := executor.member(ctx)
		result <- err
	}()
	<-creator.gotCtx
	cancel()
	select {
	case err := <-result:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("member() error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("canceling the caller did not abort the construction")
	}
	if creator.member.closes != 1 {
		t.Fatalf("a member built after the caller gave up must be closed once, closes=%d", creator.member.closes)
	}
	if executor.cachedMember != nil {
		t.Fatal("a member built for a canceled caller was cached")
	}
}
