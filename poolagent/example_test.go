package poolagent

import (
	"context"
	"fmt"
	"iter"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

type exampleCreator struct{}

func (exampleCreator) CreateAgent(ctx context.Context, name string, _ AgentRequest) (agent.Agent, error) {
	if name == "primary" {
		return nil, fmt.Errorf("backend unavailable")
	}

	return agent.New(agent.Config{
		Name:        name,
		Description: "example",
		Run: func(agent.InvocationContext) iter.Seq2[*session.Event, error] {
			return func(func(*session.Event, error) bool) {}
		},
	})
}

func ExampleNewPoolExecutor() {
	executor := NewPoolExecutor("writers", []MemberConfig{
		{Name: "primary", Cfg: agentconfig.Config{Type: agentconfig.AgentTypeOpenAI}},
		{Name: "secondary", Cfg: agentconfig.Config{Type: agentconfig.AgentTypeOpenAI}},
	}, exampleCreator{}, AgentRequest{})

	selected, err := executor.Agent(context.Background())
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println(selected.Name())
	// Output:
	// secondary
}
