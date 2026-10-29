package agentfactory

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/hostedagent"
	"github.com/baldaworks/runtime/v2/mcpregistry"
	acpagent "github.com/normahq/go-adk-acpagent/v2"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
)

type successHostedModel struct{ name string }

func (m successHostedModel) Name() string { return m.name }

func (successHostedModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) { yield(&model.LLMResponse{}, nil) }
}

type errorHostedModel struct{ name string }

func (m errorHostedModel) Name() string { return m.name }

func (errorHostedModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		if yield(&model.LLMResponse{Partial: true}, nil) {
			yield(nil, errors.New("provider unavailable"))
		}
	}
}

type countingMCPRegistry struct{ gets int }

func (r *countingMCPRegistry) Get(string) (agentconfig.MCPServerConfig, bool) {
	r.gets++
	return agentconfig.MCPServerConfig{}, false
}

type constructionDenyTransport struct{ calls int }

func (d *constructionDenyTransport) RoundTrip(*http.Request) (*http.Response, error) {
	d.calls++
	return nil, errors.New("network denied")
}

func TestModelOverrideRejectedBeforeResourceResolution(t *testing.T) {
	tests := []struct {
		name     string
		provider agentconfig.Config
		override string
		want     error
	}{
		{
			name: "blank",
			provider: agentconfig.Config{Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{
				APIKey: "key", Model: "recipe",
			}, MCPServers: []string{"side-effect"}},
			override: " \t ",
			want:     ErrInvalidModelOverride,
		},
		{
			name:     "unsupported pool",
			provider: agentconfig.Config{Type: agentconfig.AgentTypePool, PoolConfig: &agentconfig.PoolConfig{Members: []string{"member"}}, MCPServers: []string{"side-effect"}},
			override: "target",
			want:     ErrModelOverrideUnsupported,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			registry := &countingMCPRegistry{}
			_, err := New(map[string]agentconfig.Config{"provider": test.provider}, registry).BuildProduct(
				t.Context(),
				BuildRequest{AgentID: "provider", WorkingDirectory: t.TempDir(), ModelOverride: &test.override},
			)
			if !errors.Is(err, test.want) {
				t.Fatalf("BuildProduct error = %v, want %v", err, test.want)
			}
			if registry.gets != 0 {
				t.Fatalf("MCP resolution count = %d, want 0", registry.gets)
			}
		})
	}
}

func TestACPModelOverrideUsesSemanticDesiredModelAndCopiesInput(t *testing.T) {
	original := newACPAgent
	t.Cleanup(func() { newACPAgent = original })
	var captured []acpagent.Config
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		captured = append(captured, cfg)
		return agent.New(agent.Config{Name: fmt.Sprintf("acp-%d", len(captured))})
	}
	provider := agentconfig.Config{Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{
		Cmd: []string{"never-started"}, Model: "recipe-model", ModelConfigID: "literal-recipe-id",
	}}
	factory := New(map[string]agentconfig.Config{"acp": provider}, mcpregistry.New(nil))
	if _, err := factory.BuildProduct(t.Context(), BuildRequest{AgentID: "acp", WorkingDirectory: t.TempDir()}); err != nil {
		t.Fatalf("nil override: %v", err)
	}
	requested := "  desired-model  "
	if _, err := factory.BuildProduct(t.Context(), BuildRequest{AgentID: "acp", WorkingDirectory: t.TempDir(), ModelOverride: &requested}); err != nil {
		t.Fatalf("present override: %v", err)
	}
	requested = "caller-mutated"
	if len(captured) != 2 {
		t.Fatalf("captured configs = %d", len(captured))
	}
	if captured[0].DesiredModel != nil || len(captured[0].SessionConfig) != 1 || captured[0].SessionConfig[0].Value != "recipe-model" {
		t.Fatalf("nil override config = %#v", captured[0])
	}
	if captured[1].DesiredModel == nil || *captured[1].DesiredModel != "desired-model" || len(captured[1].SessionConfig) != 0 {
		t.Fatalf("semantic override config = %#v", captured[1])
	}
}

func TestACPBuildProductDoesNotStartProvider(t *testing.T) {
	methodLog := filepath.Join(t.TempDir(), "methods")
	command := helperACPCommand(t)
	command = append(command[:1], append([]string{"GO_AGENTFACTORY_METHOD_LOG=" + methodLog}, command[1:]...)...)
	override := "desired-model"
	product, err := New(map[string]agentconfig.Config{
		"acp": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: command, Model: "recipe"}},
	}, mcpregistry.New(nil)).BuildProduct(t.Context(), BuildRequest{
		AgentID: "acp", WorkingDirectory: t.TempDir(), ModelOverride: &override,
	})
	if err != nil {
		t.Fatalf("BuildProduct: %v", err)
	}
	if _, err := os.Stat(methodLog); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("ACP process started during construction: %v", err)
	}
	if err := product.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestHostedBuildProductDoesNotUseProviderNetwork(t *testing.T) {
	for _, providerType := range []string{agentconfig.AgentTypeOpenAI, agentconfig.AgentTypeAIStudio} {
		t.Run(providerType, func(t *testing.T) {
			transport := &constructionDenyTransport{}
			oldTransport := http.DefaultTransport
			http.DefaultTransport = transport
			t.Cleanup(func() { http.DefaultTransport = oldTransport })
			provider := agentconfig.Config{Type: providerType}
			if providerType == agentconfig.AgentTypeOpenAI {
				provider.OpenAI = &agentconfig.LocalAPIConfig{APIKey: "key", Model: "recipe"}
			} else {
				provider.AIStudio = &agentconfig.LocalAPIConfig{APIKey: "key", Model: "recipe"}
			}
			override := "desired"
			product, err := New(map[string]agentconfig.Config{"hosted": provider}, mcpregistry.New(nil)).BuildProduct(
				t.Context(), BuildRequest{AgentID: "hosted", WorkingDirectory: t.TempDir(), ModelOverride: &override},
			)
			if err != nil {
				t.Fatalf("BuildProduct: %v", err)
			}
			if transport.calls != 0 {
				t.Fatalf("provider network calls during construction = %d", transport.calls)
			}
			if err := product.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

func TestHostedModelOverrideReplacesOnlyLocalSelection(t *testing.T) {
	originalOpenAI, originalStudio, originalHosted := newOpenAIModel, newAIStudioModel, newHostedAgent
	t.Cleanup(func() {
		newOpenAIModel, newAIStudioModel, newHostedAgent = originalOpenAI, originalStudio, originalHosted
	})
	var selected []string
	var descriptions []string
	newOpenAIModel = func(_ string, modelName string, _ time.Duration, _ string) (model.LLM, error) {
		selected = append(selected, modelName)
		return successHostedModel{name: modelName}, nil
	}
	newAIStudioModel = func(_ context.Context, _ string, modelName string) (model.LLM, error) {
		selected = append(selected, modelName)
		return successHostedModel{name: modelName}, nil
	}
	newHostedAgent = func(cfg hostedagent.Config) (agent.Agent, error) {
		descriptions = append(descriptions, cfg.Description)
		return agent.New(agent.Config{Name: cfg.Name})
	}
	for _, providerType := range []string{agentconfig.AgentTypeOpenAI, agentconfig.AgentTypeAIStudio} {
		provider := agentconfig.Config{Type: providerType}
		if providerType == agentconfig.AgentTypeOpenAI {
			provider.OpenAI = &agentconfig.LocalAPIConfig{APIKey: "key", Model: "recipe"}
		} else {
			provider.AIStudio = &agentconfig.LocalAPIConfig{APIKey: "key", Model: "recipe"}
		}
		override := "override-" + providerType
		product, err := New(map[string]agentconfig.Config{"hosted": provider}, mcpregistry.New(nil)).BuildProduct(
			t.Context(), BuildRequest{AgentID: "hosted", WorkingDirectory: t.TempDir(), ModelOverride: &override},
		)
		if err != nil {
			t.Fatalf("%s BuildProduct: %v", providerType, err)
		}
		if err := product.Close(); err != nil {
			t.Fatalf("%s Close: %v", providerType, err)
		}
	}
	if len(selected) != 2 || selected[0] != "override-openai" || selected[1] != "override-aistudio" {
		t.Fatalf("selected models = %v", selected)
	}
	if len(descriptions) != 2 || descriptions[0] != "hosted: type=openai model=recipe" || descriptions[1] != "hosted: type=aistudio model=recipe" {
		t.Fatalf("override changed non-model recipe fields: %v", descriptions)
	}
}

func TestAIStudioNilOverridePreservesExactRecipeAndPresentOverrideNormalizes(t *testing.T) {
	originalStudio, originalHosted := newAIStudioModel, newHostedAgent
	t.Cleanup(func() { newAIStudioModel, newHostedAgent = originalStudio, originalHosted })
	var constructorModels []string
	var hostedModels []string
	newAIStudioModel = func(_ context.Context, _ string, modelName string) (model.LLM, error) {
		constructorModels = append(constructorModels, modelName)
		return successHostedModel{name: modelName}, nil
	}
	newHostedAgent = func(cfg hostedagent.Config) (agent.Agent, error) {
		hostedModels = append(hostedModels, cfg.Model.Name())
		return agent.New(agent.Config{Name: cfg.Name})
	}
	const recipe = "  recipe-model  "
	factory := New(map[string]agentconfig.Config{
		"studio": {Type: agentconfig.AgentTypeAIStudio, AIStudio: &agentconfig.LocalAPIConfig{APIKey: "key", Model: recipe}},
	}, mcpregistry.New(nil))
	product, err := factory.BuildProduct(t.Context(), BuildRequest{AgentID: "studio", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("nil override BuildProduct: %v", err)
	}
	if err := product.Close(); err != nil {
		t.Fatalf("nil override Close: %v", err)
	}

	override := "  requested-model  "
	product, err = factory.BuildProduct(t.Context(), BuildRequest{
		AgentID: "studio", WorkingDirectory: t.TempDir(), ModelOverride: &override,
	})
	if err != nil {
		t.Fatalf("present override BuildProduct: %v", err)
	}
	if err := product.Close(); err != nil {
		t.Fatalf("present override Close: %v", err)
	}
	if len(constructorModels) != 2 || constructorModels[0] != recipe || constructorModels[1] != "requested-model" {
		t.Fatalf("constructor models = %#v", constructorModels)
	}
	if len(hostedModels) != 2 || hostedModels[0] != recipe || hostedModels[1] != "requested-model" {
		t.Fatalf("hosted model names = %#v", hostedModels)
	}
}
