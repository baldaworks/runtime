package agentfactory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync/atomic"
	"testing"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/agentproduct"
	"github.com/baldaworks/runtime/v2/mcpregistry"
	acpagent "github.com/normahq/go-adk-acpagent/v2"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

type productTestAgent struct {
	agent.Agent
	closes atomic.Int32
	err    error
}

type productTestACPAgent struct {
	*productTestAgent
	control *acpagent.SessionController
}

func (a *productTestACPAgent) SessionControl() *acpagent.SessionController { return a.control }

func (a *productTestAgent) Close() error {
	a.closes.Add(1)
	return a.err
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

func TestBuildProductAdaptsACPControlWithoutWrappingAgent(t *testing.T) {
	original := constructors[agentconfig.AgentTypeGenericACP]
	t.Cleanup(func() { constructors[agentconfig.AgentTypeGenericACP] = original })

	base, err := agent.New(agent.Config{Name: "acp-product-test"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	built := &productTestACPAgent{
		productTestAgent: &productTestAgent{Agent: base},
		control:          &acpagent.SessionController{},
	}
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
		t.Fatal("BuildProduct wrapped or replaced the ACP Agent")
	}
	if product.Control == nil {
		t.Fatal("ACP product has nil control")
	}
	if got := product.Control.Inspect(t.Context(), nil); got.Status != agentproduct.ControlFailed || got.Reason != agentproduct.ReasonInvalidSession {
		t.Fatalf("Inspect(nil) = %#v", got)
	}
	if err := product.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := built.closes.Load(); got != 1 {
		t.Fatalf("agent close count = %d, want 1", got)
	}
}

func TestBuildProductActualACPControlStaysLazyAndFailsClosed(t *testing.T) {
	methodLog := t.TempDir() + "/methods.jsonl"
	factory := New(map[string]agentconfig.Config{
		"test": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: helperACPCommand(t),
				Env: map[string]string{"GO_AGENTFACTORY_METHOD_LOG": methodLog},
			},
		},
	}, mcpregistry.New(nil), WithAvailabilityIndependentMCP())
	product, err := factory.BuildProduct(t.Context(), BuildRequest{AgentID: "test", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("BuildProduct: %v", err)
	}
	defer func() { _ = product.Close() }()
	if product.Control == nil {
		t.Fatal("actual ACP product has nil control")
	}
	if _, err := os.Stat(methodLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("BuildProduct started lazy ACP: %v", err)
	}

	svc := session.InMemoryService()
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "product", UserID: "user"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	got := product.Control.Inspect(t.Context(), created.Session)
	if got.Status != agentproduct.ControlUnsupported || got.Reason != agentproduct.ReasonUnavailable {
		t.Fatalf("Inspect unattached session = %#v", got)
	}
	methods, err := os.ReadFile(methodLog)
	if err != nil {
		t.Fatalf("read method log: %v", err)
	}
	if !bytes.Contains(methods, []byte(`"method":"initialize"`)) {
		t.Fatalf("Inspect did not initialize ACP: %s", methods)
	}
	if bytes.Contains(methods, []byte(`"method":"session/new"`)) || bytes.Contains(methods, []byte(`"method":"session/resume"`)) {
		t.Fatalf("Inspect created or resumed an unattached session: %s", methods)
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
