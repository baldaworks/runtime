package poolagent

import (
	"context"
	"iter"
	"testing"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
)

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
	codex := &scriptedMember{name: "codex", errorCode: "usageLimitExceeded"}
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

func TestPoolSkipsCoolingMemberThenReturnsToPrimary(t *testing.T) {
	codex := &scriptedMember{name: "codex", errorCode: "usageLimitExceeded"}
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
	codex := &scriptedMember{name: "codex", errorCode: "usageLimitExceeded"}
	glm := &scriptedMember{name: "glm", errorCode: "rateLimited"}
	pool, _, _ := newScriptedPool(codex, glm)

	events := runPool(t, pool)
	last := events[len(events)-1]
	if len(events) != 1 || last.ErrorCode != "rateLimited" {
		t.Fatalf("want only last member's error event, got %+v", events)
	}
}
