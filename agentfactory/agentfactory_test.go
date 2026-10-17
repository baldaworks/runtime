package agentfactory

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"iter"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/hostedagent"
	"github.com/baldaworks/runtime/v2/mcpregistry"
	"github.com/baldaworks/runtime/v2/sessionstate"
	acp "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/normahq/go-adk-acpagent/v2"
	"github.com/stretchr/testify/assert"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
)

type contextKey string

func TestFactory_CreateAgent(t *testing.T) {
	agents := map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: helperACPCommand(t),
			},
		},
		"test-agy": {
			Type: agentconfig.AgentTypeAgyACP,
			AgyACP: &agentconfig.ACPConfig{
				Cmd: helperACPCommand(t),
			},
		},
		"test-registry": {
			Type: agentconfig.AgentTypeRegistryACP,
			RegistryACP: &agentconfig.ACPConfig{
				RegistryID: "amp-acp",
				Cmd:        helperACPCommand(t),
			},
		},
	}
	f := New(agents, mcpregistry.New(nil))

	t.Run("Create ACP Agent", func(t *testing.T) {
		req := BuildRequest{
			AgentID:          "test-acp",
			Name:             "TestACP",
			Description:      "Test Description",
			WorkingDirectory: t.TempDir(),
		}
		ag, err := f.Build(context.Background(), req)
		assert.NoError(t, err)
		assert.NotNil(t, ag)
	})

	t.Run("Create Agy ACP Agent", func(t *testing.T) {
		req := BuildRequest{
			AgentID:          "test-agy",
			Name:             "TestAgy",
			Description:      "Test Agy Description",
			WorkingDirectory: t.TempDir(),
		}
		ag, err := f.Build(context.Background(), req)
		assert.NoError(t, err)
		assert.NotNil(t, ag)
	})

	t.Run("Create Registry ACP Agent", func(t *testing.T) {
		req := BuildRequest{
			AgentID:          "test-registry",
			Name:             "TestRegistry",
			Description:      "Test Registry Description",
			WorkingDirectory: t.TempDir(),
		}
		ag, err := f.Build(context.Background(), req)
		assert.NoError(t, err)
		assert.NotNil(t, ag)
	})

	t.Run("Unknown Agent", func(t *testing.T) {
		req := BuildRequest{
			AgentID:          "unknown",
			Name:             "Unknown",
			WorkingDirectory: t.TempDir(),
		}
		ag, err := f.Build(context.Background(), req)
		assert.Error(t, err)
		assert.Nil(t, ag)
		assert.Contains(t, err.Error(), "not found")
	})

	t.Run("Missing working directory", func(t *testing.T) {
		req := BuildRequest{
			AgentID: "test-acp",
			Name:    "TestACP",
		}
		ag, err := f.Build(context.Background(), req)
		assert.Error(t, err)
		assert.Nil(t, ag)
		assert.Contains(t, err.Error(), "working_directory is required")
	})
}

func helperACPCommand(t *testing.T) []string {
	t.Helper()
	return []string{
		"env",
		"GO_WANT_AGENTFACTORY_ACP_HELPER=1",
		os.Args[0],
		"-test.run=TestAgentFactoryACPHelperProcess",
		"--",
	}
}

func TestAgentFactoryACPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_AGENTFACTORY_ACP_HELPER") != "1" {
		return
	}

	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      nil,
				"error": map[string]any{
					"code":    -32700,
					"message": "parse error",
				},
			})
			continue
		}

		if path := os.Getenv("GO_AGENTFACTORY_METHOD_LOG"); path != "" {
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
			if err != nil {
				os.Exit(2)
			}
			_, _ = f.Write(append(append([]byte(nil), scanner.Bytes()...), '\n'))
			_ = f.Close()
		}
		if req.Method == os.Getenv("GO_AGENTFACTORY_STALLED_METHOD") {
			continue
		}
		if req.Method == acp.AgentMethodInitialize {
			if value := os.Getenv("GO_AGENTFACTORY_INITIALIZE_DELAY"); value != "" {
				delay, err := time.ParseDuration(value)
				if err != nil {
					os.Exit(2)
				}
				time.Sleep(delay)
			}
		}
		if req.Method == acp.AgentMethodSessionNew {
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"sessionId": "factory-fixture"}})
			continue
		}
		if req.Method == acp.AgentMethodSessionPrompt {
			_ = encoder.Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{"stopReason": "end_turn"}})
			continue
		}
		if req.Method == acp.AgentMethodInitialize {
			_ = encoder.Encode(map[string]any{
				"jsonrpc": "2.0",
				"id":      req.ID,
				"result": map[string]any{
					"protocolVersion": acp.ProtocolVersionNumber,
				},
			})
			continue
		}

		_ = encoder.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      req.ID,
			"error": map[string]any{
				"code":    -32601,
				"message": "unsupported",
			},
		})
	}
	os.Exit(0)
}

func TestFactoryBuild_NormalizesTemplatedACPCommand(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var capturedCommand []string
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedCommand = append([]string(nil), cfg.Command...)
		return nil, nil
	}

	agents := map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd:       []string{"custom-acp", "--model", "{{.Model}}"},
				Model:     "gpt-5.4",
				ExtraArgs: []string{"--trace", "--model={{.Model}}"},
			},
		},
	}
	f := New(agents, mcpregistry.New(nil))

	_, err := f.Build(context.Background(), BuildRequest{AgentID: "test-acp", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	want := []string{"custom-acp", "--model", "gpt-5.4", "--trace", "--model=gpt-5.4"}
	assert.Equal(t, want, capturedCommand)
}

func TestFactoryBuild_PropagatesACPReasoningEffort(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var capturedReasoningEffort string
	var capturedSessionConfig []acpagent.SessionConfigValue
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedReasoningEffort = cfg.ReasoningEffort
		capturedSessionConfig = cfg.SessionConfig
		return nil, nil
	}

	agents := map[string]agentconfig.Config{
		"acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd:             helperACPCommand(t),
				ReasoningEffort: "medium",
			},
		},
	}
	f := New(agents, mcpregistry.New(nil))

	_, err := f.Build(context.Background(), BuildRequest{AgentID: "acp", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	assert.Equal(t, "medium", capturedReasoningEffort)
	assert.Equal(t, []acpagent.SessionConfigValue{
		acpagent.SelectSessionConfigValue("reasoning_effort", "medium"),
	}, capturedSessionConfig)
}

func TestFactoryBuild_BuildRequestReasoningEffortOverridesProvider(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var capturedReasoningEffort string
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedReasoningEffort = cfg.ReasoningEffort
		return nil, nil
	}

	agents := map[string]agentconfig.Config{
		"acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd:             helperACPCommand(t),
				ReasoningEffort: "low",
			},
		},
	}
	f := New(agents, mcpregistry.New(nil))

	_, err := f.Build(context.Background(), BuildRequest{
		AgentID:          "acp",
		WorkingDirectory: t.TempDir(),
		ReasoningEffort:  "high",
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	assert.Equal(t, "high", capturedReasoningEffort)
}

func TestFactoryBuild_AllowsReasoningEffortOverrideForGenericACP(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var capturedReasoningEffort string
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedReasoningEffort = cfg.ReasoningEffort
		return nil, nil
	}
	agents := map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: helperACPCommand(t),
			},
		},
	}
	f := New(agents, mcpregistry.New(nil))

	_, err := f.Build(context.Background(), BuildRequest{
		AgentID:          "test-acp",
		WorkingDirectory: t.TempDir(),
		ReasoningEffort:  "high",
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	assert.Equal(t, "high", capturedReasoningEffort)
}

func TestFactoryBuild_UsesExactACPConfigOptionIDsInOrder(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var captured []acpagent.SessionConfigValue
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		captured = cfg.SessionConfig
		return nil, nil
	}
	f := New(map[string]agentconfig.Config{
		"custom": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd:                     helperACPCommand(t),
				Model:                   "current-model",
				ModelConfigID:           "provider_model",
				ReasoningEffort:         "medium",
				ReasoningEffortConfigID: "thought_level",
				Mode:                    "code",
			},
		},
	}, mcpregistry.New(nil))

	_, err := f.Build(context.Background(), BuildRequest{AgentID: "custom", WorkingDirectory: t.TempDir()})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	assert.Equal(t, []acpagent.SessionConfigValue{
		acpagent.SelectSessionConfigValue("provider_model", "current-model"),
		acpagent.SelectSessionConfigValue("thought_level", "medium"),
		acpagent.SelectSessionConfigValue("mode", "code"),
	}, captured)
}

func TestACPConstructor_PassesConfiguredSlogLogger(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var capturedLogger *slog.Logger
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedLogger = cfg.Logger
		return nil, nil
	}

	ctx := context.Background()
	factory := New(map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: []string{"fake-acp", "serve"},
			},
		},
	}, nil)

	_, err := acpConstructor(ctx, agentconfig.ResolvedConfig{
		Type:    agentconfig.AgentTypeGenericACP,
		Command: []string{"fake-acp", "serve"},
	}, BuildRequest{
		AgentID:          "test-acp",
		Name:             "test-acp",
		Description:      "test",
		WorkingDirectory: t.TempDir(),
	}, factory, nil)
	if err != nil {
		t.Fatalf("acpConstructor() error = %v", err)
	}
	if capturedLogger == nil {
		t.Fatal("acpConstructor() did not pass logger to acpagent config")
	}
	if !capturedLogger.Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("captured logger should enable info level")
	}
}

func TestFactoryBuild_DefaultACPStderrWriterIsProcessStderr(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var capturedStderr any
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedStderr = cfg.Stderr
		return nil, nil
	}

	agents := map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: []string{"fake-acp", "serve"},
			},
		},
	}
	f := New(agents, mcpregistry.New(nil))

	_, err := f.Build(context.Background(), BuildRequest{
		AgentID:          "test-acp",
		WorkingDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	stderrFile, ok := capturedStderr.(*os.File)
	if !ok {
		t.Fatalf("ACP stderr writer type = %T, want *os.File", capturedStderr)
	}
	if stderrFile != os.Stderr {
		t.Fatalf("ACP stderr writer = %p, want process stderr %p", stderrFile, os.Stderr)
	}
}

func TestFactoryBuild_WithStderrWriterOverridesACPStderr(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var capturedStderr any
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedStderr = cfg.Stderr
		return nil, nil
	}

	agents := map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: []string{"fake-acp", "serve"},
			},
		},
	}
	customStderr := &bytes.Buffer{}
	f := New(agents, mcpregistry.New(nil), WithStderrWriter(customStderr))

	_, err := f.Build(context.Background(), BuildRequest{
		AgentID:          "test-acp",
		WorkingDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if capturedStderr != customStderr {
		t.Fatalf("ACP stderr writer = %T/%p, want custom writer %p", capturedStderr, capturedStderr, customStderr)
	}
}

func TestFactoryBuild_UsesBuildRequestMCPServerIDsOverride(t *testing.T) {
	origNewACPAgent := newACPAgent
	origProcessEnv := processEnv
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
		processEnv = origProcessEnv
	})

	var capturedMCP map[string]acpagent.MCPServerConfig
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedMCP = cfg.MCPServers
		return nil, nil
	}
	processEnv = func() []string {
		return []string{"BASE_TOKEN=from-process", "UNCHANGED=process-value"}
	}

	agents := map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: []string{"fake-acp", "serve"},
			},
			MCPServers: []string{"cfg"},
		},
	}
	reg := mcpregistry.New(map[string]agentconfig.MCPServerConfig{
		"cfg": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://cfg.example/mcp",
		},
		"override": {
			Type: agentconfig.MCPServerTypeStdio,
			Cmd:  []string{"override-server"},
			Env: map[string]string{
				"BASE_TOKEN": "from-config",
				"LOCAL_ONLY": "config-value",
				"EMPTY":      "",
			},
		},
	})
	f := New(agents, reg)

	_, err := f.Build(context.Background(), BuildRequest{
		AgentID:          "test-acp",
		WorkingDirectory: t.TempDir(),
		MCPServerIDs:     []string{"override"},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(capturedMCP) != 1 {
		t.Fatalf("len(capturedMCP) = %d, want 1", len(capturedMCP))
	}
	if _, ok := capturedMCP["override"]; !ok {
		t.Fatalf("captured MCP does not contain override server: %#v", capturedMCP)
	}
	if _, ok := capturedMCP["cfg"]; ok {
		t.Fatalf("captured MCP unexpectedly contains cfg server: %#v", capturedMCP)
	}
	override := capturedMCP["override"]
	if got := override.Env["BASE_TOKEN"]; got != "from-config" {
		t.Fatalf("override BASE_TOKEN = %q, want from-config", got)
	}
	if got := override.Env["LOCAL_ONLY"]; got != "config-value" {
		t.Fatalf("override LOCAL_ONLY = %q, want config-value", got)
	}
	if got := override.Env["UNCHANGED"]; got != "process-value" {
		t.Fatalf("override UNCHANGED = %q, want process-value", got)
	}
	if got := override.Env["EMPTY"]; got != "" {
		t.Fatalf("override EMPTY = %q, want empty string", got)
	}

	original, ok := reg.Get("override")
	if !ok {
		t.Fatal("registry missing override MCP server")
	}
	if got := original.Env["UNCHANGED"]; got != "" {
		t.Fatalf("registry env mutated, UNCHANGED = %q, want empty string", got)
	}
}

func TestFactoryBuildSessionState_UsesCanonicalCWDKey(t *testing.T) {
	workingDir := t.TempDir()
	f := New(map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: []string{"fake-acp", "serve"},
			},
		},
	}, nil)

	state, err := f.BuildSessionState("test-acp", workingDir)
	if err != nil {
		t.Fatalf("BuildSessionState() error = %v", err)
	}
	if len(state) != 1 {
		t.Fatalf("len(state) = %d, want 1", len(state))
	}
	gotCWD, ok := state[sessionstate.CWDKey].(string)
	if !ok {
		t.Fatalf("state[%q] type = %T, want string", sessionstate.CWDKey, state[sessionstate.CWDKey])
	}
	wantCWD, err := filepath.Abs(workingDir)
	if err != nil {
		t.Fatalf("filepath.Abs() error = %v", err)
	}
	if gotCWD != wantCWD {
		t.Fatalf("state[%q] = %q, want %q", sessionstate.CWDKey, gotCWD, wantCWD)
	}
}

func TestFactoryBuildSessionState_InvalidCWD(t *testing.T) {
	f := New(map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: []string{"fake-acp", "serve"},
			},
		},
	}, nil)

	_, err := f.BuildSessionState("test-acp", filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("BuildSessionState() error = nil, want invalid cwd error")
	}
	if !strings.Contains(err.Error(), "stat session cwd") {
		t.Fatalf("BuildSessionState() error = %q, want stat session cwd", err)
	}
}

func TestFactoryBuildSessionState_UnknownAgent(t *testing.T) {
	f := New(map[string]agentconfig.Config{}, nil)
	_, err := f.BuildSessionState("unknown", t.TempDir())
	if err == nil {
		t.Fatal("BuildSessionState() error = nil, want unknown agent error")
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Fatalf("BuildSessionState() error = %q, want not found", err)
	}
}

func TestACPConstructor_UsesInstructionAndGlobalInstruction(t *testing.T) {
	origNewACPAgent := newACPAgent
	t.Cleanup(func() {
		newACPAgent = origNewACPAgent
	})

	var capturedInstruction string
	var capturedGlobalInstruction string
	var capturedOutputKey string
	newACPAgent = func(cfg acpagent.Config) (agent.Agent, error) {
		capturedInstruction = cfg.Instruction
		capturedGlobalInstruction = cfg.GlobalInstruction
		capturedOutputKey = cfg.OutputKey
		return nil, nil
	}
	factory := New(map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: []string{"fake-acp", "serve"},
			},
		},
	}, nil)

	_, err := acpConstructor(context.Background(), agentconfig.ResolvedConfig{
		Type:               agentconfig.AgentTypeGenericACP,
		Command:            []string{"fake-acp", "serve"},
		SystemInstructions: "from-config",
	}, BuildRequest{
		AgentID:           "test-acp",
		Instruction:       "from-request",
		GlobalInstruction: "global-request",
		OutputKey:         "agent_result",
		WorkingDirectory:  t.TempDir(),
	}, factory, nil)
	if err != nil {
		t.Fatalf("acpConstructor() error = %v", err)
	}

	if capturedInstruction != "from-request" {
		t.Fatalf("cfg.Instruction = %q, want from-request", capturedInstruction)
	}
	if capturedGlobalInstruction != "global-request" {
		t.Fatalf("cfg.GlobalInstruction = %q, want global-request", capturedGlobalInstruction)
	}
	if capturedOutputKey != "agent_result" {
		t.Fatalf("cfg.OutputKey = %q, want agent_result", capturedOutputKey)
	}
}

type fakeHostedModel struct {
	name string
}

func (m fakeHostedModel) Name() string {
	return m.name
}

func (m fakeHostedModel) GenerateContent(context.Context, *model.LLMRequest, bool) iter.Seq2[*model.LLMResponse, error] {
	return func(func(*model.LLMResponse, error) bool) {}
}

type requestTool struct {
	name string
}

func (t requestTool) Name() string {
	return t.name
}

func (requestTool) Description() string {
	return "request test tool"
}

func (requestTool) IsLongRunning() bool {
	return false
}

type requestToolset struct {
	name string
}

func (t requestToolset) Name() string {
	return t.name
}

func (requestToolset) Tools(agent.ReadonlyContext) ([]tool.Tool, error) {
	return nil, nil
}

func TestFactoryBuild_OpenAIProvider(t *testing.T) {
	origNewOpenAIModel := newOpenAIModel
	origNewHostedAgent := newHostedAgent
	t.Cleanup(func() {
		newOpenAIModel = origNewOpenAIModel
		newHostedAgent = origNewHostedAgent
	})

	var capturedAPIKey string
	var capturedModelName string
	var capturedTimeout time.Duration
	var capturedReasoningEffort string
	newOpenAIModel = func(apiKey, modelName string, timeout time.Duration, reasoningEffort string) (model.LLM, error) {
		capturedAPIKey = apiKey
		capturedModelName = modelName
		capturedTimeout = timeout
		capturedReasoningEffort = reasoningEffort
		return fakeHostedModel{name: "remote-openai"}, nil
	}

	var capturedCfg hostedagent.Config
	newHostedAgent = func(cfg hostedagent.Config) (agent.Agent, error) {
		capturedCfg = cfg
		return nil, nil
	}

	f := New(map[string]agentconfig.Config{
		"openai": {
			Type: agentconfig.AgentTypeOpenAI,
			OpenAI: &agentconfig.LocalAPIConfig{
				APIKey:          "openai-test-key",
				Model:           "gpt-5",
				Timeout:         "180s",
				ReasoningEffort: "none",
			},
			SystemInstructions: "from-config",
			MCPServers:         []string{"docs"},
		},
	}, mcpregistry.New(map[string]agentconfig.MCPServerConfig{
		"docs": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://docs.example/mcp",
		},
	}))

	_, err := f.Build(context.Background(), BuildRequest{
		AgentID:           "openai",
		Name:              "shell-openai",
		Description:       "OpenAI shell agent",
		Instruction:       "from-request",
		GlobalInstruction: "global-request",
		WorkingDirectory:  t.TempDir(),
		Tools:             []tool.Tool{requestTool{name: "read_file"}},
		Toolsets:          []tool.Toolset{requestToolset{name: "review_tools"}},
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if capturedAPIKey != "openai-test-key" {
		t.Fatalf("openai api key = %q, want openai-test-key", capturedAPIKey)
	}
	if capturedModelName != "gpt-5" {
		t.Fatalf("openai model name = %q, want gpt-5", capturedModelName)
	}
	if capturedTimeout != 180*time.Second || capturedReasoningEffort != "none" {
		t.Fatalf("openai options = %v, %q", capturedTimeout, capturedReasoningEffort)
	}
	if capturedCfg.Name != "shell-openai" {
		t.Fatalf("hosted agent name = %q, want shell-openai", capturedCfg.Name)
	}
	if capturedCfg.Description != "OpenAI shell agent" {
		t.Fatalf("hosted agent description = %q, want OpenAI shell agent", capturedCfg.Description)
	}
	if capturedCfg.Instruction != "from-request" {
		t.Fatalf("hosted agent instruction = %q, want from-request", capturedCfg.Instruction)
	}
	if capturedCfg.GlobalInstruction != "global-request" {
		t.Fatalf("hosted agent global instruction = %q, want global-request", capturedCfg.GlobalInstruction)
	}
	if capturedCfg.Model == nil || capturedCfg.Model.Name() != "remote-openai" {
		t.Fatalf("hosted agent model = %#v, want remote-openai", capturedCfg.Model)
	}
	if len(capturedCfg.Tools) != 1 || capturedCfg.Tools[0].Name() != "read_file" {
		t.Fatalf("hosted agent tools = %#v, want read_file", capturedCfg.Tools)
	}
	if len(capturedCfg.Toolsets) != 2 {
		t.Fatalf("hosted agent toolsets = %#v, want request and mcp toolsets", capturedCfg.Toolsets)
	}
	if capturedCfg.Toolsets[0].Name() != "review_tools" {
		t.Fatalf("hosted agent request toolset = %q, want review_tools", capturedCfg.Toolsets[0].Name())
	}
	if capturedCfg.Toolsets[1].Name() != "mcp_tool_set" {
		t.Fatalf("hosted agent mcp toolset = %q, want mcp_tool_set", capturedCfg.Toolsets[1].Name())
	}
}

// TestFactoryBuildKeepsPreferredDedupEndpoint exercises the production path
// from registry lookup through config hydration and hosted toolset selection.
// The session-scoped endpoint has to survive because its capability token is
// required for the broker to inject the session context.
func TestFactoryBuildKeepsPreferredDedupEndpoint(t *testing.T) {
	originalModel := newOpenAIModel
	originalHostedAgent := newHostedAgent
	t.Cleanup(func() {
		newOpenAIModel = originalModel
		newHostedAgent = originalHostedAgent
	})
	newOpenAIModel = func(string, string, time.Duration, string) (model.LLM, error) {
		return fakeHostedModel{name: "test-openai"}, nil
	}
	newHostedAgent = func(hostedagent.Config) (agent.Agent, error) {
		return nil, nil
	}

	const endpoint = "http://127.0.0.1:34237/mcp/balda"
	const scopedEndpoint = endpoint + "?balda_context=abc123"
	endpoints := recordTransportEndpoints(t)
	f := New(map[string]agentconfig.Config{
		"openai": {
			Type: agentconfig.AgentTypeOpenAI,
			OpenAI: &agentconfig.LocalAPIConfig{
				APIKey: "test-key",
				Model:  "test-model",
			},
			MCPServers: []string{"balda", "balda-session"},
		},
	}, mcpregistry.New(map[string]agentconfig.MCPServerConfig{
		"balda": {
			Type:     agentconfig.MCPServerTypeHTTP,
			URL:      endpoint,
			DedupKey: "balda-bundled-mcp",
		},
		"balda-session": {
			Type:           agentconfig.MCPServerTypeHTTP,
			URL:            scopedEndpoint,
			DedupKey:       "balda-bundled-mcp",
			DedupPreferred: true,
		},
	}))

	if _, err := f.Build(context.Background(), BuildRequest{
		AgentID:          "openai",
		WorkingDirectory: t.TempDir(),
	}); err != nil {
		t.Fatalf("Build() error = %v", err)
	}
	if len(*endpoints) != 1 {
		t.Fatalf("transport constructions = %v, want one scoped endpoint", *endpoints)
	}
	if got := (*endpoints)[0]; got != scopedEndpoint {
		t.Fatalf("surviving endpoint = %q, want %q", got, scopedEndpoint)
	}
}

func TestMCPTransportForConfig_StdioPreservesProcessConfig(t *testing.T) {
	transport, err := mcpTransportForConfig(agentconfig.MCPServerConfig{
		Type:       agentconfig.MCPServerTypeStdio,
		Cmd:        []string{"mcp-server", "--from-cmd"},
		Args:       []string{"--from-args"},
		Env:        map[string]string{"BETA": "2", "ALPHA": "1"},
		WorkingDir: "/tmp/mcp-work",
	})
	if err != nil {
		t.Fatalf("mcpTransportForConfig() error = %v", err)
	}
	cmdTransport, ok := transport.(*mcp.CommandTransport)
	if !ok {
		t.Fatalf("transport = %T, want *mcp.CommandTransport", transport)
	}
	if cmdTransport.Command.Path != "mcp-server" {
		t.Fatalf("command path = %q, want mcp-server", cmdTransport.Command.Path)
	}
	assert.Equal(t, []string{"mcp-server", "--from-cmd", "--from-args"}, cmdTransport.Command.Args)
	assert.Equal(t, []string{"ALPHA=1", "BETA=2"}, cmdTransport.Command.Env)
	if cmdTransport.Command.Dir != "/tmp/mcp-work" {
		t.Fatalf("command dir = %q, want /tmp/mcp-work", cmdTransport.Command.Dir)
	}
}

func TestHTTPClientWithHeadersAddsConfiguredHeaders(t *testing.T) {
	req, err := http.NewRequest(http.MethodGet, "http://example.test/mcp", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	rt := staticHeaderRoundTripper{
		base: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			if got := req.Header.Get("Authorization"); got != "Bearer token" {
				t.Fatalf("Authorization header = %q, want Bearer token", got)
			}
			if got := req.Header.Get("X-Test"); got != "yes" {
				t.Fatalf("X-Test header = %q, want yes", got)
			}
			return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody, Header: make(http.Header)}, nil
		}),
		headers: map[string]string{"Authorization": "Bearer token", "X-Test": "yes"},
	}
	if _, err := rt.RoundTrip(req); err != nil {
		t.Fatalf("RoundTrip() error = %v", err)
	}
	if req.Header.Get("Authorization") != "" {
		t.Fatalf("RoundTrip mutated original request headers: %#v", req.Header)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestFactoryBuild_ACPRejectsRequestTools(t *testing.T) {
	f := New(map[string]agentconfig.Config{
		"test-acp": {
			Type: agentconfig.AgentTypeGenericACP,
			GenericACP: &agentconfig.ACPConfig{
				Cmd: []string{"echo"},
			},
		},
	}, mcpregistry.New(nil))

	_, err := f.Build(context.Background(), BuildRequest{
		AgentID:          "test-acp",
		WorkingDirectory: t.TempDir(),
		Tools:            []tool.Tool{requestTool{name: "read_file"}},
	})
	if err == nil {
		t.Fatal("Build() error = nil, want request tools rejection")
	}
	if !strings.Contains(err.Error(), "does not support request tools") {
		t.Fatalf("Build() error = %v, want request tools rejection", err)
	}
}

func TestFactoryBuild_PoolRejectsRequestTools(t *testing.T) {
	f := New(map[string]agentconfig.Config{
		"pool": {
			Type: agentconfig.AgentTypePool,
			PoolConfig: &agentconfig.PoolConfig{
				Members: []string{"openai"},
			},
		},
		"openai": {
			Type: agentconfig.AgentTypeOpenAI,
			OpenAI: &agentconfig.LocalAPIConfig{
				APIKey: "openai-test-key",
				Model:  "gpt-5",
			},
		},
	}, mcpregistry.New(nil))

	_, err := f.Build(context.Background(), BuildRequest{
		AgentID:          "pool",
		WorkingDirectory: t.TempDir(),
		Tools:            []tool.Tool{requestTool{name: "read_file"}},
	})
	if err == nil {
		t.Fatal("Build() error = nil, want request tools rejection")
	}
	if !strings.Contains(err.Error(), "pool agent does not support request tools") {
		t.Fatalf("Build() error = %v, want pool request tools rejection", err)
	}
}

func TestFactoryBuild_AIStudioProvider(t *testing.T) {
	origNewAIStudioModel := newAIStudioModel
	origNewHostedAgent := newHostedAgent
	t.Cleanup(func() {
		newAIStudioModel = origNewAIStudioModel
		newHostedAgent = origNewHostedAgent
	})

	var capturedCtx context.Context
	var capturedAPIKey string
	var capturedModelName string
	newAIStudioModel = func(ctx context.Context, apiKey, modelName string) (model.LLM, error) {
		capturedCtx = ctx
		capturedAPIKey = apiKey
		capturedModelName = modelName
		return fakeHostedModel{name: "remote-aistudio"}, nil
	}

	var capturedCfg hostedagent.Config
	newHostedAgent = func(cfg hostedagent.Config) (agent.Agent, error) {
		capturedCfg = cfg
		return nil, nil
	}

	ctx := context.WithValue(context.Background(), contextKey("provider"), "aistudio")
	f := New(map[string]agentconfig.Config{
		"aistudio": {
			Type: agentconfig.AgentTypeAIStudio,
			AIStudio: &agentconfig.LocalAPIConfig{
				APIKey: "aistudio-test-key",
				Model:  "gemini-2.5-flash",
			},
			MCPServers: []string{"workspace"},
		},
	}, mcpregistry.New(map[string]agentconfig.MCPServerConfig{
		"workspace": {
			Type: agentconfig.MCPServerTypeHTTP,
			URL:  "http://workspace.example/mcp",
		},
	}))

	_, err := f.Build(ctx, BuildRequest{
		AgentID:          "aistudio",
		WorkingDirectory: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("Build() error = %v", err)
	}

	if capturedCtx != ctx {
		t.Fatal("aistudio constructor did not pass the request context through to model creation")
	}
	if capturedAPIKey != "aistudio-test-key" {
		t.Fatalf("aistudio api key = %q, want aistudio-test-key", capturedAPIKey)
	}
	if capturedModelName != "gemini-2.5-flash" {
		t.Fatalf("aistudio model name = %q, want gemini-2.5-flash", capturedModelName)
	}
	if capturedCfg.Name != "aistudio" {
		t.Fatalf("hosted agent name = %q, want aistudio", capturedCfg.Name)
	}
	if capturedCfg.Model == nil || capturedCfg.Model.Name() != "remote-aistudio" {
		t.Fatalf("hosted agent model = %#v, want remote-aistudio", capturedCfg.Model)
	}
	if len(capturedCfg.Toolsets) != 1 || capturedCfg.Toolsets[0].Name() != "mcp_tool_set" {
		t.Fatalf("aistudio hosted toolsets = %#v, want one mcp toolset", capturedCfg.Toolsets)
	}
}

func TestValidatePoolMembers_AcceptsMixedACPAndHostedProviders(t *testing.T) {
	members, err := validatePoolMembers("pool", []string{"codex", "openai"}, map[string]agentconfig.Config{
		"codex": {
			Type: agentconfig.AgentTypeCodexACP,
			CodexACP: &agentconfig.ACPConfig{
				Model: "gpt-5-codex",
			},
		},
		"openai": {
			Type: agentconfig.AgentTypeOpenAI,
			OpenAI: &agentconfig.LocalAPIConfig{
				APIKey: "test-key",
				Model:  "gpt-5",
			},
		},
	})
	if err != nil {
		t.Fatalf("validatePoolMembers() error = %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("len(members) = %d, want 2", len(members))
	}
	if members[0].Name != "codex" || members[1].Name != "openai" {
		t.Fatalf("pool members = %#v, want codex then openai", members)
	}
}
