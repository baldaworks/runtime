package agentfactory

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/normahq/go-adk-acpagent/v2"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	runnerpkg "google.golang.org/adk/v2/runner"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"
)

func TestACPInitializeOptionProviderRouting(t *testing.T) {
	original := newACPAgent
	t.Cleanup(func() { newACPAgent = original })
	var captured acpagent.Config
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) { captured = cfg; return nil, nil }
	cfg := &agentconfig.ACPConfig{Cmd: []string{"chosen-backend", "--exact"}, RegistryID: "fixture"}
	providers := []agentconfig.Config{
		{Type: agentconfig.AgentTypeGenericACP, GenericACP: cfg},
		{Type: agentconfig.AgentTypeCodexACP, CodexACP: cfg},
		{Type: agentconfig.AgentTypeOpenCodeACP, OpenCodeACP: cfg},
		{Type: agentconfig.AgentTypeCopilotACP, CopilotACP: cfg},
		{Type: agentconfig.AgentTypeClaudeCodeACP, ClaudeCodeACP: cfg},
		{Type: agentconfig.AgentTypeClaudeACP, ClaudeCodeACP: cfg},
		{Type: agentconfig.AgentTypeGrokACP, GrokACP: cfg},
		{Type: agentconfig.AgentTypeRegistryACP, RegistryACP: cfg},
		{Type: agentconfig.AgentTypeAgyACP, AgyACP: cfg},
		{Type: agentconfig.AgentTypeAntigravityACP, AntigravityACP: cfg},
	}
	for _, provider := range providers {
		for _, lazy := range []bool{false, true} {
			t.Run(provider.Type+"/"+map[bool]string{false: "eager", true: "lazy"}[lazy], func(t *testing.T) {
				for _, budget := range []time.Duration{0, 60 * time.Second} {
					options := []Option{WithACPInitializeTimeout(budget)}
					if lazy {
						options = append(options, WithAvailabilityIndependentMCP())
					}
					f := New(map[string]agentconfig.Config{"fixture": provider}, nil, options...)
					if _, err := f.Build(t.Context(), BuildRequest{AgentID: "fixture", WorkingDirectory: t.TempDir()}); err != nil {
						t.Fatal(err)
					}
					wantSetup := time.Duration(0)
					if lazy {
						wantSetup = 10 * time.Second
					}
					if captured.InitializeTimeout != budget || captured.SetupTimeout != wantSetup || captured.LazyInitialization != lazy {
						t.Fatalf("timeouts/lazy = %v/%v/%v, want %v/%v/%v", captured.InitializeTimeout, captured.SetupTimeout, captured.LazyInitialization, budget, wantSetup, lazy)
					}
					if !reflect.DeepEqual(captured.Command, cfg.Cmd) {
						t.Fatalf("command = %v, want %v", captured.Command, cfg.Cmd)
					}
				}
			})
		}
	}
	// An omitted option also retains the availability-independent 10s inheritance.
	if _, err := New(map[string]agentconfig.Config{"fixture": providers[0]}, nil, WithAvailabilityIndependentMCP()).Build(t.Context(), BuildRequest{AgentID: "fixture", WorkingDirectory: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
	if captured.InitializeTimeout != 0 || captured.SetupTimeout != 10*time.Second {
		t.Fatalf("default timeouts = %v/%v", captured.InitializeTimeout, captured.SetupTimeout)
	}
}

func TestACPInitializeOptionRejectsNegativeAtBuild(t *testing.T) {
	f := New(map[string]agentconfig.Config{"fixture": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: []string{"/does/not/exist"}}}}, nil, WithACPInitializeTimeout(-time.Second))
	_, err := f.Build(t.Context(), BuildRequest{AgentID: "fixture", WorkingDirectory: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "acp initialize timeout must not be negative") {
		t.Fatalf("Build() error = %v", err)
	}
}

func TestACPInitializeOptionHostedNeutrality(t *testing.T) {
	original := newOpenAIModel
	t.Cleanup(func() { newOpenAIModel = original })
	newOpenAIModel = func(_, _ string, timeout time.Duration, _ string) (model.LLM, error) {
		if timeout != 3*time.Second {
			t.Errorf("hosted timeout = %v", timeout)
		}
		return &fakeHostedModel{}, nil
	}
	f := New(map[string]agentconfig.Config{"hosted": {Type: agentconfig.AgentTypeOpenAI, OpenAI: &agentconfig.LocalAPIConfig{APIKey: "fixture", Model: "fixture", Timeout: "3s"}}}, nil, WithACPInitializeTimeout(time.Nanosecond))
	if _, err := f.Build(t.Context(), BuildRequest{AgentID: "hosted", WorkingDirectory: t.TempDir()}); err != nil {
		t.Fatal(err)
	}
}

func initializeFixture(t *testing.T, delay, stalledMethod string, budget time.Duration, lazy, pool bool) (agent.Agent, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "methods")
	command := helperACPCommand(t)
	command = append(command[:1], append([]string{"GO_AGENTFACTORY_METHOD_LOG=" + path, "GO_AGENTFACTORY_INITIALIZE_DELAY=" + delay, "GO_AGENTFACTORY_STALLED_METHOD=" + stalledMethod}, command[1:]...)...)
	providers := map[string]agentconfig.Config{"fixture": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: command}}}
	id := "fixture"
	if pool {
		providers["pool"] = agentconfig.Config{Type: agentconfig.AgentTypePool, PoolConfig: &agentconfig.PoolConfig{Members: []string{"fixture"}}}
		id = "pool"
	}
	options := []Option{WithACPInitializeTimeout(budget)}
	if lazy {
		options = append(options, WithAvailabilityIndependentMCP())
	}
	built, err := New(providers, nil, options...).Build(t.Context(), BuildRequest{AgentID: id, WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := built.(io.Closer).Close(); err != nil {
			t.Error(err)
		}
	})
	return built, path
}

func fixtureTurn(t *testing.T, ctx context.Context, built agent.Agent) error {
	t.Helper()
	svc := session.InMemoryService()
	runner, err := runnerpkg.New(runnerpkg.Config{AppName: "initialize", Agent: built, SessionService: svc})
	if err != nil {
		t.Fatal(err)
	}
	created, err := svc.Create(t.Context(), &session.CreateRequest{AppName: "initialize", UserID: "u"})
	if err != nil {
		t.Fatal(err)
	}
	for _, err := range runner.Run(ctx, "u", created.Session.ID(), genai.NewContentFromText("hello", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			return err
		}
	}
	return nil
}

func TestACPInitializeSlowFirstTurnAndWarmReuse(t *testing.T) {
	// Real 11s handshake catches coupling to the production 10s attachment limit.
	built, path := initializeFixture(t, "11s", "", 60*time.Second, true, true)
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("idle pool launched ACP: %v", err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
	defer cancel()
	for range 2 {
		if err := fixtureTurn(t, ctx, built); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(data, []byte(`"method":"initialize"`)); got != 1 {
		t.Fatalf("initialize count = %d, log: %s", got, data)
	}
	if got := bytes.Count(data, []byte(`"method":"session/prompt"`)); got != 2 {
		t.Fatalf("prompt count = %d, log: %s", got, data)
	}
}

func TestACPInitializeOwnBudgetEagerAndLazy(t *testing.T) {
	for _, lazy := range []bool{false, true} {
		t.Run(map[bool]string{false: "eager", true: "lazy"}[lazy], func(t *testing.T) {
			command := helperACPCommand(t)
			command = append(command[:1], append([]string{"GO_AGENTFACTORY_INITIALIZE_DELAY=2s"}, command[1:]...)...)
			opts := []Option{WithACPInitializeTimeout(100 * time.Millisecond)}
			if lazy {
				opts = append(opts, WithAvailabilityIndependentMCP())
			}
			start := time.Now()
			built, err := New(map[string]agentconfig.Config{"fixture": {Type: agentconfig.AgentTypeGenericACP, GenericACP: &agentconfig.ACPConfig{Cmd: command}}}, nil, opts...).Build(t.Context(), BuildRequest{AgentID: "fixture", WorkingDirectory: t.TempDir()})
			if lazy {
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					if err := built.(io.Closer).Close(); err != nil {
						t.Error(err)
					}
				}()
				err = fixtureTurn(t, t.Context(), built)
			}
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("initialize error = %v", err)
			}
			if elapsed := time.Since(start); elapsed > time.Second {
				t.Fatalf("initialize exceeded independent budget plus scheduling tolerance: %v", elapsed)
			}
		})
	}
}

func TestACPInitializeDoesNotWidenAttachment(t *testing.T) {
	built, _ := initializeFixture(t, "", "session/new", 60*time.Second, true, false)
	start := time.Now()
	if err := fixtureTurn(t, t.Context(), built); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("attachment error = %v", err)
	}
	if elapsed := time.Since(start); elapsed < 9*time.Second || elapsed > 12*time.Second {
		t.Fatalf("attachment duration = %v, want 10s plus scheduling tolerance", elapsed)
	}
}

func TestACPInitializeCancellationAndOwnerClose(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "request cancellation", true: "owner close"}[shutdown], func(t *testing.T) {
			built, path := initializeFixture(t, "", "initialize", 60*time.Second, true, false)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			result := make(chan error, 1)
			go func() { result <- fixtureTurn(t, ctx, built) }()
			observed := time.NewTimer(3 * time.Second)
			defer observed.Stop()
			tick := time.NewTicker(10 * time.Millisecond)
			defer tick.Stop()
		waitInitialize:
			for {
				select {
				case <-observed.C:
					t.Fatal("child did not receive initialize")
				case <-tick.C:
					data, err := os.ReadFile(path)
					if err == nil && bytes.Contains(data, []byte(`"method":"initialize"`)) {
						break waitInitialize
					}
				}
			}
			start := time.Now()
			if shutdown {
				if err := built.(io.Closer).Close(); err != nil {
					t.Fatal(err)
				}
			} else {
				cancel()
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("canceled initialize = %v", err)
				}
			case <-time.After(6200 * time.Millisecond):
				t.Fatal("startup cancellation did not finish within cleanup tolerance")
			}
			if time.Since(start) > 6200*time.Millisecond {
				t.Fatal("shutdown waited for startup budget")
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if bytes.Count(data, []byte(`"method":"initialize"`)) != 1 || bytes.Contains(data, []byte(`"method":"session/new"`)) {
				t.Fatalf("cancellation attached or relaunched: %s", data)
			}
		})
	}
}
