package agentfactory

import (
	"context"
	"io"
	"sync/atomic"
	"testing"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/mcpregistry"
	"google.golang.org/adk/v2/agent"
)

type productTestAgent struct {
	agent.Agent
	closes atomic.Int32
}

func (a *productTestAgent) Close() error {
	a.closes.Add(1)
	return nil
}

func TestBuildProductPreservesAgentAndOwnsClose(t *testing.T) {
	original := constructors[agentconfig.AgentTypeGenericACP]
	t.Cleanup(func() { constructors[agentconfig.AgentTypeGenericACP] = original })

	base, err := agent.New(agent.Config{Name: "product-test"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	built := &productTestAgent{Agent: base}
	constructors[agentconfig.AgentTypeGenericACP] = func(context.Context, agentconfig.ResolvedConfig, BuildRequest, *Factory, map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
		return built, nil
	}
	factory := New(map[string]agentconfig.Config{
		"test": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: []string{"unused"}}},
	}, mcpregistry.New(nil))
	product, err := factory.BuildProduct(context.Background(), BuildRequest{AgentID: "test", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("BuildProduct: %v", err)
	}
	if product.Agent != built {
		t.Fatal("BuildProduct wrapped or replaced the constructed Agent")
	}
	if product.Control != nil {
		t.Fatal("agent-only product unexpectedly advertises control")
	}
	if _, ok := product.Agent.(io.Closer); !ok {
		t.Fatal("BuildProduct lost an optional Agent interface")
	}
	if err := product.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := built.closes.Load(); got != 1 {
		t.Fatalf("agent close count = %d, want 1", got)
	}
}

func TestBuildLegacyBehaviorIsUnchanged(t *testing.T) {
	original := constructors[agentconfig.AgentTypeGenericACP]
	t.Cleanup(func() { constructors[agentconfig.AgentTypeGenericACP] = original })

	base, err := agent.New(agent.Config{Name: "legacy-test"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	built := &productTestAgent{Agent: base}
	constructors[agentconfig.AgentTypeGenericACP] = func(context.Context, agentconfig.ResolvedConfig, BuildRequest, *Factory, map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
		return built, nil
	}
	factory := New(map[string]agentconfig.Config{
		"test": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: []string{"unused"}}},
	}, mcpregistry.New(nil))
	ag, err := factory.Build(context.Background(), BuildRequest{AgentID: "test", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if ag != built {
		t.Fatal("Build no longer returns the constructed Agent directly")
	}
	if got := built.closes.Load(); got != 0 {
		t.Fatalf("Build transferred or closed legacy ownership: %d", got)
	}
}

func TestBuildProductPoolHasNoControl(t *testing.T) {
	original := constructors[agentconfig.AgentTypeGenericACP]
	t.Cleanup(func() { constructors[agentconfig.AgentTypeGenericACP] = original })
	base, err := agent.New(agent.Config{Name: "pool-member"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	constructors[agentconfig.AgentTypeGenericACP] = func(context.Context, agentconfig.ResolvedConfig, BuildRequest, *Factory, map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
		return base, nil
	}
	factory := New(map[string]agentconfig.Config{
		"pool": {
			Type:       agentconfig.AgentTypePool,
			PoolConfig: &agentconfig.PoolConfig{Members: []string{"member"}},
		},
		"member": {
			Type:       agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{Cmd: []string{"unused"}},
		},
	}, mcpregistry.New(nil))
	product, err := factory.BuildProduct(context.Background(), BuildRequest{AgentID: "pool", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("BuildProduct: %v", err)
	}
	defer func() { _ = product.Close() }()
	if product.Control != nil {
		t.Fatal("pool product unexpectedly advertises session control")
	}
}
