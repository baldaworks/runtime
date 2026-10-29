package hostedagent

import (
	"context"
	"iter"
	"net/http"
	"os"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"
	"google.golang.org/genai"
)

type denyRoundTripper struct{ calls int }

func (d *denyRoundTripper) RoundTrip(*http.Request) (*http.Response, error) {
	d.calls++
	return nil, context.Canceled
}

func TestNewAIStudioModelDefersClientAndCredentialWork(t *testing.T) {
	tests := []struct {
		name string
		env  map[string]string
		key  string
	}{
		{name: "explicit key", key: "protected-key"},
		{name: "environment key", env: map[string]string{"GOOGLE_API_KEY": "protected-env-key"}},
		{name: "vertex environment", env: map[string]string{"GOOGLE_GENAI_USE_VERTEXAI": "true", "GOOGLE_CLOUD_PROJECT": "project", "GOOGLE_CLOUD_LOCATION": "location"}},
		{name: "default credentials"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			for _, key := range []string{"GOOGLE_API_KEY", "GEMINI_API_KEY", "GOOGLE_GENAI_USE_VERTEXAI", "GOOGLE_CLOUD_PROJECT", "GOOGLE_CLOUD_LOCATION", "GOOGLE_APPLICATION_CREDENTIALS"} {
				t.Setenv(key, "")
			}
			for key, value := range test.env {
				t.Setenv(key, value)
			}
			transport := &denyRoundTripper{}
			oldTransport := http.DefaultTransport
			http.DefaultTransport = transport
			t.Cleanup(func() { http.DefaultTransport = oldTransport })

			got, err := NewAIStudioModel(t.Context(), test.key, "gemini-test")
			if err != nil {
				t.Fatalf("NewAIStudioModel: %v", err)
			}
			lazy, ok := got.(*lazyAIStudioModel)
			if !ok || lazy.model != nil || lazy.err != nil {
				t.Fatalf("construction initialized provider model: %#v", got)
			}
			if got.Name() != "gemini-test" {
				t.Fatalf("Name = %q", got.Name())
			}
			if transport.calls != 0 {
				t.Fatalf("construction network calls = %d", transport.calls)
			}
		})
	}
}

func TestNewAIStudioModelRejectsBlankBeforeInitialization(t *testing.T) {
	_, err := NewAIStudioModel(t.Context(), os.Getenv("GOOGLE_API_KEY"), " \t")
	if err == nil {
		t.Fatal("NewAIStudioModel blank model error = nil")
	}
}

func TestNewAIStudioModelPreservesValidatedRecipeModel(t *testing.T) {
	const recipe = "  gemini-recipe  "
	got, err := NewAIStudioModel(t.Context(), "key", recipe)
	if err != nil {
		t.Fatalf("NewAIStudioModel: %v", err)
	}
	if got.Name() != recipe {
		t.Fatalf("Name = %q, want exact recipe %q", got.Name(), recipe)
	}
	lazy := got.(*lazyAIStudioModel)
	if lazy.modelName != recipe || lazy.model != nil {
		t.Fatalf("lazy recipe state = %#v", lazy)
	}
}

type captureModel struct {
	req *model.LLMRequest
}

func (*captureModel) Name() string {
	return "capture-model"
}

func (m *captureModel) GenerateContent(_ context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	m.req = req
	return func(yield func(*model.LLMResponse, error) bool) {
		yield(&model.LLMResponse{
			Content: genai.NewContentFromText("ok", genai.RoleModel),
		}, nil)
	}
}

type pingArgs struct {
	Value string `json:"value"`
}

type pingResult struct {
	Value string `json:"value"`
}

func TestNew_PassesToolsToLLMAgent(t *testing.T) {
	m := &captureModel{}
	ping, err := functiontool.New(functiontool.Config{
		Name:        "ping",
		Description: "Returns the input value.",
	}, func(_ agent.Context, args pingArgs) (pingResult, error) {
		return pingResult(args), nil
	})
	if err != nil {
		t.Fatalf("functiontool.New() error = %v", err)
	}

	agentRuntime, err := New(Config{
		Name:        "hosted",
		Description: "Hosted test agent",
		Model:       m,
		Tools:       []tool.Tool{ping},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	sessionService := session.InMemoryService()
	r, err := runner.New(runner.Config{
		AppName:        "hostedagent-test",
		Agent:          agentRuntime,
		SessionService: sessionService,
	})
	if err != nil {
		t.Fatalf("runner.New() error = %v", err)
	}
	created, err := sessionService.Create(context.Background(), &session.CreateRequest{
		AppName: "hostedagent-test",
		UserID:  "test-user",
	})
	if err != nil {
		t.Fatalf("session.Create() error = %v", err)
	}

	for _, runErr := range r.Run(context.Background(), "test-user", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if runErr != nil {
			t.Fatalf("runner.Run() error = %v", runErr)
		}
	}

	if m.req == nil {
		t.Fatal("model did not receive a request")
	}
	if m.req.Config == nil || len(m.req.Config.Tools) != 1 {
		t.Fatalf("model request tools = %#v, want one tool", m.req.Config)
	}
	decls := m.req.Config.Tools[0].FunctionDeclarations
	if len(decls) != 1 || decls[0].Name != "ping" {
		t.Fatalf("function declarations = %#v, want ping", decls)
	}
}
