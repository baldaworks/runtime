package agentfactory

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/agentproduct"
	"github.com/baldaworks/runtime/v2/hostedagent"
	"github.com/baldaworks/runtime/v2/mcpregistry"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

const (
	hostedOldModel = "model-old"
	hostedNewModel = "model-new"
)

func TestHostedBuildProductValidationAndIndependentReplacement(t *testing.T) {
	for _, providerType := range []string{agentconfig.AgentTypeOpenAI, agentconfig.AgentTypeAIStudio} {
		t.Run(providerType, func(t *testing.T) {
			oldOpenAI, oldAIStudio, oldTransport := newOpenAIModel, newAIStudioModel, mcpTransportFactory
			t.Cleanup(func() {
				newOpenAIModel, newAIStudioModel, mcpTransportFactory = oldOpenAI, oldAIStudio, oldTransport
			})

			modelBuilds := 0
			newOpenAIModel = func(_ string, name string, _ time.Duration, _ string) (model.LLM, error) {
				modelBuilds++
				return fakeHostedModel{name: name}, nil
			}
			newAIStudioModel = func(context.Context, string, string) (model.LLM, error) {
				modelBuilds++
				return fakeHostedModel{name: "hosted"}, nil
			}
			var endpoints []string
			mcpTransportFactory = func(cfg agentconfig.MCPServerConfig) (mcp.Transport, error) {
				endpoints = append(endpoints, cfg.URL)
				return oldTransport(cfg)
			}

			provider := agentconfig.Config{Type: providerType, MCPServers: []string{"shared"}}
			switch providerType {
			case agentconfig.AgentTypeOpenAI:
				provider.OpenAI = &agentconfig.LocalAPIConfig{APIKey: "protected-api-key", Model: hostedOldModel}
			case agentconfig.AgentTypeAIStudio:
				provider.AIStudio = &agentconfig.LocalAPIConfig{APIKey: "protected-api-key", Model: hostedOldModel}
			}
			registry := mcpregistry.New(map[string]agentconfig.MCPServerConfig{
				"shared": {Type: agentconfig.MCPServerTypeHTTP, URL: "http://original.invalid", Headers: map[string]string{"Authorization": "protected-header"}},
			})
			factory := New(map[string]agentconfig.Config{"hosted": provider}, registry)
			product, err := factory.BuildProduct(t.Context(), BuildRequest{AgentID: "hosted", WorkingDirectory: t.TempDir()})
			if err != nil {
				t.Fatalf("BuildProduct: %v", err)
			}
			defer func() { _ = product.Close() }()
			if product.Control == nil {
				t.Fatal("hosted product has nil control")
			}
			if modelBuilds != 1 || len(endpoints) != 1 {
				t.Fatalf("initial builds = model:%d transport:%d", modelBuilds, len(endpoints))
			}
			formatted := fmt.Sprintf("%#v", product.Control)
			if strings.Contains(formatted, "protected-api-key") || strings.Contains(formatted, "protected-header") {
				t.Fatalf("control formatting exposed protected recipe: %s", formatted)
			}

			sess := newHostedControlSession(t)
			snapshot := product.Control.Inspect(t.Context(), sess)
			if snapshot.Status != agentproduct.ControlReady || snapshot.CurrentModel != "" || len(snapshot.Models) != 0 {
				t.Fatalf("Inspect = %#v", snapshot)
			}
			validation := product.Control.ValidateModel(t.Context(), sess, hostedNewModel)
			if validation.Status != agentproduct.ModelSupported || validation.Model != hostedNewModel {
				t.Fatalf("ValidateModel = %#v", validation)
			}
			if modelBuilds != 1 || len(endpoints) != 1 {
				t.Fatalf("validation constructed candidate: model:%d transport:%d", modelBuilds, len(endpoints))
			}

			registry.Set("shared", agentconfig.MCPServerConfig{Type: agentconfig.MCPServerTypeHTTP, URL: "http://changed.invalid"})
			change := product.Control.ChangeModel(t.Context(), sess, hostedNewModel)
			if change.Status != agentproduct.ModelReplacement || change.EffectiveModel != hostedNewModel || change.Replacement == nil {
				t.Fatalf("ChangeModel = %#v", change)
			}
			candidate := change.Replacement
			if candidate.Agent == product.Agent {
				t.Fatal("replacement reused incumbent Agent")
			}
			if modelBuilds != 2 || len(endpoints) != 2 || endpoints[0] != "http://original.invalid" || endpoints[1] != endpoints[0] {
				t.Fatalf("candidate recipe was not independent: model:%d endpoints:%v", modelBuilds, endpoints)
			}
			candidateSnapshot := candidate.Control.Inspect(t.Context(), sess)
			if candidateSnapshot.Status != agentproduct.ControlReady || candidateSnapshot.CurrentModel != "" || len(candidateSnapshot.Models) != 0 {
				t.Fatalf("candidate Inspect = %#v", candidateSnapshot)
			}
			if err := candidate.Close(); err != nil {
				t.Fatalf("candidate Close: %v", err)
			}
			if err := candidate.Close(); err != nil {
				t.Fatalf("candidate second Close: %v", err)
			}
			if got := product.Control.ValidateModel(t.Context(), sess, "model-next"); got.Status != agentproduct.ModelSupported {
				t.Fatalf("candidate close affected incumbent: %#v", got)
			}
		})
	}
}

func TestHostedInspectReportsOnlySuccessfulExactProductExecution(t *testing.T) {
	observation := &hostedModelObservation{}
	control := &hostedSessionControl{
		recipe:      hostedRecipe{model: hostedOldModel},
		observation: observation,
	}
	sess := newHostedControlSession(t)
	if got := control.Inspect(t.Context(), sess); got.CurrentModel != "" || len(got.Models) != 0 {
		t.Fatalf("Inspect before execution = %#v", got)
	}

	failed := observeHostedModel(errorHostedModel{name: hostedOldModel}, hostedOldModel, observation)
	sawFailure := false
	for _, err := range failed.GenerateContent(t.Context(), nil, false) {
		if err != nil {
			sawFailure = true
		}
	}
	if !sawFailure {
		t.Fatal("failed model returned no error")
	}
	if got := control.Inspect(t.Context(), sess); got.CurrentModel != "" {
		t.Fatalf("Inspect after failed execution = %#v", got)
	}

	succeeded := observeHostedModel(successHostedModel{name: hostedOldModel}, hostedOldModel, observation)
	for _, err := range succeeded.GenerateContent(t.Context(), nil, false) {
		if err != nil {
			t.Fatalf("successful model: %v", err)
		}
	}
	got := control.Inspect(t.Context(), sess)
	if got.CurrentModel != hostedOldModel || len(got.Models) != 1 || got.ControlRevision != "" {
		t.Fatalf("Inspect after successful local execution = %#v", got)
	}

	replacement := &hostedSessionControl{
		recipe:      hostedRecipe{model: hostedOldModel},
		observation: &hostedModelObservation{},
	}
	if got := replacement.Inspect(t.Context(), sess); got.CurrentModel != "" {
		t.Fatalf("replacement inherited incumbent observation = %#v", got)
	}
}

func TestHostedProductRunnerPublishesLocalExecutionObservation(t *testing.T) {
	original := newOpenAIModel
	t.Cleanup(func() { newOpenAIModel = original })
	newOpenAIModel = func(_ string, name string, _ time.Duration, _ string) (model.LLM, error) {
		return successHostedModel{name: name}, nil
	}
	factory := New(map[string]agentconfig.Config{
		"hosted": {Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{APIKey: "key", Model: hostedOldModel}},
	}, mcpregistry.New(nil))
	product, err := factory.BuildProduct(t.Context(), BuildRequest{AgentID: "hosted", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("BuildProduct: %v", err)
	}
	defer func() { _ = product.Close() }()

	service := session.InMemoryService()
	created, err := service.Create(t.Context(), &session.CreateRequest{AppName: "hosted", UserID: "user"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	if got := product.Control.Inspect(t.Context(), created.Session); got.CurrentModel != "" {
		t.Fatalf("Inspect before runner execution = %#v", got)
	}
	run, err := runner.New(runner.Config{AppName: "hosted", Agent: product.Agent, SessionService: service})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	for _, runErr := range run.Run(t.Context(), "user", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("runner.Run: %v", runErr)
		}
	}
	got := product.Control.Inspect(t.Context(), created.Session)
	if got.CurrentModel != hostedOldModel || len(got.Models) != 1 {
		t.Fatalf("Inspect after runner execution = %#v", got)
	}
}

func TestHostedChangeModelPartialFailureReturnsNoCandidate(t *testing.T) {
	oldModel, oldHosted := newOpenAIModel, newHostedAgent
	t.Cleanup(func() { newOpenAIModel, newHostedAgent = oldModel, oldHosted })
	newOpenAIModel = func(_ string, name string, _ time.Duration, _ string) (model.LLM, error) {
		return fakeHostedModel{name: name}, nil
	}
	builds := 0
	newHostedAgent = func(cfg hostedagent.Config) (agent.Agent, error) {
		builds++
		if builds == 2 {
			return nil, fmt.Errorf("candidate construction failed with protected detail")
		}
		return newHostedAgentDefault(cfg)
	}
	factory := New(map[string]agentconfig.Config{
		"hosted": {Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{APIKey: "protected-api-key", Model: hostedOldModel}},
	}, mcpregistry.New(nil))
	product, err := factory.BuildProduct(t.Context(), BuildRequest{AgentID: "hosted", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("BuildProduct: %v", err)
	}
	defer func() { _ = product.Close() }()
	got := product.Control.ChangeModel(t.Context(), newHostedControlSession(t), hostedNewModel)
	if got.Status != agentproduct.ModelChangeFailed || got.Reason != agentproduct.ReasonPreDispatch || got.Replacement != nil {
		t.Fatalf("ChangeModel = %#v", got)
	}
	if strings.Contains(fmt.Sprintf("%#v", got), "protected") {
		t.Fatalf("failure exposed protected construction detail: %#v", got)
	}
}

func TestHostedChangeModelCancellationClosesLateCandidate(t *testing.T) {
	base, err := agent.New(agent.Config{Name: "late-candidate"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	late := &productTestAgent{Agent: base}
	entered := make(chan struct{})
	release := make(chan struct{})
	recipe := hostedRecipe{
		providerType: agentconfig.AgentTypeOpenAI,
		apiKey:       "key",
		model:        hostedOldModel,
		create: func(context.Context, agentconfig.ResolvedConfig, BuildRequest, *Factory, map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
			close(entered)
			<-release
			return late, nil
		},
	}
	control := &hostedSessionControl{recipe: recipe}
	sess := newHostedControlSession(t)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan agentproduct.ModelChange, 1)
	go func() {
		result <- control.ChangeModel(ctx, sess, hostedNewModel)
	}()
	<-entered
	cancel()
	close(release)
	got := <-result
	if got.Status != agentproduct.ModelChangeFailed || got.Reason != agentproduct.ReasonPreDispatch || got.Replacement != nil {
		t.Fatalf("ChangeModel after cancellation = %#v", got)
	}
	if closes := late.closes.Load(); closes != 1 {
		t.Fatalf("late candidate close count = %d, want 1", closes)
	}
}

func TestHostedCandidateCloseRedactsProviderError(t *testing.T) {
	base, err := agent.New(agent.Config{Name: "candidate-close"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	owned := &productTestAgent{Agent: base, err: fmt.Errorf("close endpoint with protected-token")}
	recipe := hostedRecipe{
		providerType: agentconfig.AgentTypeOpenAI,
		apiKey:       "key",
		model:        hostedOldModel,
		create: func(context.Context, agentconfig.ResolvedConfig, BuildRequest, *Factory, map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
			return owned, nil
		},
	}
	change := (&hostedSessionControl{recipe: recipe}).ChangeModel(t.Context(), newHostedControlSession(t), hostedNewModel)
	if change.Status != agentproduct.ModelReplacement || change.Replacement == nil {
		t.Fatalf("ChangeModel = %#v", change)
	}
	closeErr := change.Replacement.Close()
	if !errors.Is(closeErr, agentproduct.ErrCloseFailed) || strings.Contains(closeErr.Error(), "protected-token") {
		t.Fatalf("candidate Close error = %v", closeErr)
	}
	if closes := owned.closes.Load(); closes != 1 {
		t.Fatalf("candidate close count = %d, want 1", closes)
	}
}

func TestHostedControlRejectsMissingSessionWithoutConstruction(t *testing.T) {
	recipe := hostedRecipe{providerType: agentconfig.AgentTypeAIStudio, model: hostedOldModel}
	control := &hostedSessionControl{recipe: recipe}
	if got := control.Inspect(t.Context(), nil); got.Status != agentproduct.ControlFailed || got.Reason != agentproduct.ReasonInvalidSession {
		t.Fatalf("Inspect(nil) = %#v", got)
	}
	if got := control.ValidateModel(t.Context(), nil, hostedNewModel); got.Status != agentproduct.ModelFailed || got.Reason != agentproduct.ReasonInvalidSession {
		t.Fatalf("ValidateModel(nil) = %#v", got)
	}
	if got := control.ChangeModel(t.Context(), nil, hostedNewModel); got.Status != agentproduct.ModelChangeFailed || got.Reason != agentproduct.ReasonInvalidSession {
		t.Fatalf("ChangeModel(nil) = %#v", got)
	}
}

func newHostedControlSession(t *testing.T) session.Session {
	t.Helper()
	service := session.InMemoryService()
	created, err := service.Create(t.Context(), &session.CreateRequest{AppName: "hosted", UserID: "user"})
	if err != nil {
		t.Fatalf("Create session: %v", err)
	}
	return created.Session
}
