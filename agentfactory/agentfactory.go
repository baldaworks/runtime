package agentfactory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/agentproduct"
	"github.com/baldaworks/runtime/v2/hostedagent"
	"github.com/baldaworks/runtime/v2/mcpregistry"
	"github.com/baldaworks/runtime/v2/poolagent"
	"github.com/baldaworks/runtime/v2/sessionstate"
	"github.com/go-playground/validator/v10"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/normahq/go-adk-acpagent/v2"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"
)

// BuildRequest defines the parameters for building a new agent instance.
type BuildRequest struct {
	// AgentID selects a provider entry from the factory registry.
	AgentID string `json:"agent_id" validate:"required,min=1"`
	// Name overrides the runtime agent display name.
	Name string `json:"name,omitempty"`
	// Description overrides the runtime agent description.
	Description string `json:"description,omitempty"`
	// Instruction overrides the provider-specific invocation instruction.
	Instruction string `json:"instruction,omitempty"`
	// GlobalInstruction is prepended ahead of Instruction for each invocation.
	GlobalInstruction string `json:"global_instruction,omitempty"`
	// ReasoningEffort overrides the provider reasoning effort when supported.
	ReasoningEffort string `json:"reasoning_effort,omitempty" validate:"omitempty,oneof=minimal low medium high xhigh"`
	// WorkingDirectory is the session working directory for the built agent.
	WorkingDirectory string `json:"working_directory" validate:"required,min=1"`
	// MCPServerIDs overrides provider-level MCP server references for this build.
	MCPServerIDs []string `json:"mcp_server_ids,omitempty"`
	// OutputKey stores the final visible model output in session state for this invocation.
	OutputKey string `json:"output_key,omitempty"`
	// Tools contains ADK-native tools supplied by the caller for this build.
	Tools []tool.Tool `json:"-"`
	// Toolsets contains ADK-native toolsets supplied by the caller for this build.
	Toolsets []tool.Toolset `json:"-"`
}

var buildRequestValidator = newBuildRequestValidator()

func newBuildRequestValidator() *validator.Validate {
	v := validator.New()
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			return fld.Name
		}
		return name
	})
	return v
}

// Validate validates the build request.
func (r BuildRequest) Validate() error {
	errs := make([]string, 0)
	if err := buildRequestValidator.Struct(r); err != nil {
		if invErr, ok := err.(*validator.InvalidValidationError); ok {
			return fmt.Errorf("validate build request: %w", invErr)
		}
		for _, validationErr := range err.(validator.ValidationErrors) {
			errs = append(errs, formatValidationError(validationErr))
		}
	}
	if r.AgentID != "" && strings.TrimSpace(r.AgentID) == "" {
		errs = append(errs, "agent_id is required")
	}
	if r.WorkingDirectory != "" && strings.TrimSpace(r.WorkingDirectory) == "" {
		errs = append(errs, "working_directory is required")
	}
	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("build request validation failed: %s", strings.Join(errs, "; "))
}

func formatValidationError(err validator.FieldError) string {
	field := err.Field()
	switch err.Tag() {
	case "required":
		return field + " is required"
	case "min":
		return field + " must be at least " + err.Param()
	default:
		return field + " failed validation rule " + err.Tag()
	}
}

// Option configures factory behavior.
type Option func(*Factory)

// WithAvailabilityIndependentMCP defers ACP startup and bounds lazy native MCP operations.
func WithAvailabilityIndependentMCP() Option {
	return func(f *Factory) { f.availabilityIndependentMCP = true }
}

// WithACPInitializeTimeout sets the ACP process launch and initialize budget.
// A positive duration overrides initialization only; session attachment and MCP
// budgets are unchanged. Zero preserves the existing behavior: initialization
// inherits SetupTimeout (10s with WithAvailabilityIndependentMCP, otherwise no
// additional deadline). Negative durations are rejected by Build. Hosted
// providers are unaffected by a valid option.
func WithACPInitializeTimeout(timeout time.Duration) Option {
	return func(f *Factory) { f.acpInitializeTimeout = timeout }
}

// WithPermissionHandler configures a default ACP permission callback for all built agents.
func WithPermissionHandler(handler acpagent.PermissionHandler) Option {
	return func(f *Factory) {
		f.permissionHandler = handler
	}
}

// WithStderrWriter configures where ACP subprocess stderr is written.
func WithStderrWriter(writer io.Writer) Option {
	return func(f *Factory) {
		if writer == nil {
			f.stderrWriter = os.Stderr
			return
		}
		f.stderrWriter = writer
	}
}

// withProcessEnvironment injects the Factory environment snapshot for tests.
func withProcessEnvironment(environment []string) Option {
	return func(f *Factory) {
		f.processEnvironment = append([]string{}, environment...)
	}
}

// constructor creates a new agent instance.
type constructor func(ctx context.Context, cfg agentconfig.ResolvedConfig, req BuildRequest, f *Factory, resolvedMCP map[string]agentconfig.MCPServerConfig) (agent.Agent, error)

// Factory is a registry of agent configurations.
type Factory struct {
	availabilityIndependentMCP bool
	acpInitializeTimeout       time.Duration
	registry                   map[string]agentconfig.Config
	mcpRegistry                mcpregistry.Reader
	permissionHandler          acpagent.PermissionHandler
	stderrWriter               io.Writer
	executablePath             string
	processEnvironment         []string
}

// New creates a new Factory from agent configurations and an MCP registry.
func New(agents map[string]agentconfig.Config, mcp mcpregistry.Reader, opts ...Option) *Factory {
	registry := make(map[string]agentconfig.Config, len(agents))
	for id, cfg := range agents {
		registry[id] = cfg
	}
	f := &Factory{
		registry:           registry,
		mcpRegistry:        mcp,
		stderrWriter:       os.Stderr,
		processEnvironment: append([]string{}, os.Environ()...),
	}
	if exePath, err := os.Executable(); err == nil {
		f.executablePath = exePath
	}
	for _, opt := range opts {
		if opt != nil {
			opt(f)
		}
	}
	return f
}

// GetAgentConfig returns the schema configuration for agentID.
func (f *Factory) GetAgentConfig(agentID string) (agentconfig.Config, error) {
	cfg, ok := f.registry[agentID]
	if !ok {
		return agentconfig.Config{}, fmt.Errorf("agent %q not found in registry", agentID)
	}
	return cfg, nil
}

// ValidateAgent checks the provider schema and required construction parameters.
// It does not start providers, acquire MCP connections, or issue model requests.
func (f *Factory) ValidateAgent(agentID string) error {
	cfg, err := f.GetAgentConfig(agentID)
	if err != nil {
		return err
	}
	if err := cfg.Validate(); err != nil {
		return err
	}
	resolvedCfg, err := agentconfig.NormalizeConfig(cfg, f.executablePath)
	if err != nil {
		return fmt.Errorf("normalize agent %q: %w", agentID, err)
	}
	if _, ok := constructors[resolvedCfg.Type]; !ok {
		return fmt.Errorf("agent type %q is not supported", resolvedCfg.Type)
	}
	switch resolvedCfg.Type {
	case agentconfig.AgentTypeOpenAI:
		return hostedagent.ValidateOpenAIModelOptions(resolvedCfg.APIKey, resolvedCfg.Model, hostedagent.OpenAIModelOptions{
			Timeout: resolvedCfg.Timeout, ReasoningEffort: resolvedCfg.ReasoningEffort,
		})
	case agentconfig.AgentTypeAIStudio:
		return hostedagent.ValidateAIStudioModel(resolvedCfg.Model)
	default:
		return nil
	}
}

// Build creates an agent.Agent instance from request.
func (f *Factory) Build(ctx context.Context, req BuildRequest) (agent.Agent, error) {
	return f.build(ctx, req)
}

// BuildProduct creates an Agent and transfers its lifecycle ownership to a
// capability-aware product. Callers must close the Product, not Product.Agent.
// Providers without exact-session control expose a nil Product.Control.
func (f *Factory) BuildProduct(ctx context.Context, req BuildRequest) (*agentproduct.Product, error) {
	ag, err := f.build(ctx, req)
	if err != nil {
		return nil, err
	}
	product, err := agentproduct.New(ag, sessionControlForAgent(ag))
	if err != nil {
		if closer, ok := ag.(io.Closer); ok {
			return nil, errors.Join(err, closer.Close())
		}
		return nil, err
	}
	return product, nil
}

func (f *Factory) build(ctx context.Context, req BuildRequest) (agent.Agent, error) {
	if f.acpInitializeTimeout < 0 {
		return nil, fmt.Errorf("acp initialize timeout must not be negative")
	}
	if err := req.Validate(); err != nil {
		return nil, err
	}
	req.AgentID = strings.TrimSpace(req.AgentID)
	req.WorkingDirectory = strings.TrimSpace(req.WorkingDirectory)

	schemaCfg, ok := f.registry[req.AgentID]
	if !ok {
		return nil, fmt.Errorf("agent %q not found or unsupported", req.AgentID)
	}
	cfg, err := agentconfig.NormalizeConfig(schemaCfg, f.executablePath)
	if err != nil {
		return nil, fmt.Errorf("normalize agent %q: %w", req.AgentID, err)
	}
	create, ok := constructors[cfg.Type]
	if !ok {
		return nil, fmt.Errorf("unsupported agent type %q for agent %q", cfg.Type, req.AgentID)
	}

	mcpServerIDs := cfg.MCPServers
	if req.MCPServerIDs != nil {
		mcpServerIDs = req.MCPServerIDs
	}

	resolvedMCP, err := f.resolveMCPServers(req.AgentID, mcpServerIDs)
	if err != nil {
		return nil, err
	}

	resolvedMCP, err = selectMCPServers(resolvedMCP)
	if err != nil {
		return nil, err
	}

	ag, err := create(ctx, cfg, req, f, resolvedMCP)
	if err != nil {
		return nil, fmt.Errorf("build agent %q: %w", req.AgentID, err)
	}
	return ag, nil
}

// BuildSessionState builds canonical ADK session state for runtime sessions.
//
// The returned state is backend-agnostic and currently always includes the
// canonical per-session working directory at key [sessionstate.CWDKey].
func (f *Factory) BuildSessionState(agentID, workspaceDir string) (map[string]any, error) {
	trimmedAgentID := strings.TrimSpace(agentID)
	if trimmedAgentID == "" {
		return nil, fmt.Errorf("agent id is required")
	}
	if _, err := f.GetAgentConfig(trimmedAgentID); err != nil {
		return nil, err
	}

	absCWD, err := normalizeSessionCWD(workspaceDir)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		sessionstate.CWDKey: absCWD,
	}, nil
}

func normalizeSessionCWD(workspaceDir string) (string, error) {
	cwd := strings.TrimSpace(workspaceDir)
	if cwd == "" {
		return "", fmt.Errorf("session cwd is empty")
	}

	absCWD, err := filepath.Abs(cwd)
	if err != nil {
		return "", fmt.Errorf("resolve session cwd %q: %w", cwd, err)
	}
	info, err := os.Stat(absCWD)
	if err != nil {
		return "", fmt.Errorf("stat session cwd %q: %w", absCWD, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("session cwd %q is not a directory", absCWD)
	}
	return absCWD, nil
}

func (f *Factory) resolveMCPServers(agentID string, ids []string) (map[string]agentconfig.MCPServerConfig, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	resolved := make(map[string]agentconfig.MCPServerConfig, len(ids))
	for i, id := range ids {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			return nil, fmt.Errorf("agent %q has empty mcp_servers[%d]", agentID, i)
		}
		if f.mcpRegistry == nil {
			return nil, fmt.Errorf("agent %q references unknown mcp server %q", agentID, trimmed)
		}
		cfg, ok := f.mcpRegistry.Get(trimmed)
		if !ok {
			return nil, fmt.Errorf("agent %q references unknown mcp server %q", agentID, trimmed)
		}
		resolved[trimmed] = hydrateMCPServerConfig(cfg)
	}
	return resolved, nil
}

var processEnv = os.Environ

func hydrateMCPServerConfig(cfg agentconfig.MCPServerConfig) agentconfig.MCPServerConfig {
	hydrated := agentconfig.MCPServerConfig{
		Type:           cfg.Type,
		Cmd:            append([]string(nil), cfg.Cmd...),
		Args:           append([]string(nil), cfg.Args...),
		Env:            cloneStringMap(cfg.Env),
		WorkingDir:     cfg.WorkingDir,
		URL:            cfg.URL,
		Headers:        cloneStringMap(cfg.Headers),
		DedupKey:       cfg.DedupKey,
		DedupPreferred: cfg.DedupPreferred,
	}
	if hydrated.Type != agentconfig.MCPServerTypeStdio {
		return hydrated
	}
	hydrated.Env = mergeStringMaps(envMapFromSlice(processEnv()), hydrated.Env)
	return hydrated
}

func envMapFromSlice(entries []string) map[string]string {
	if len(entries) == 0 {
		return nil
	}
	env := make(map[string]string, len(entries))
	for _, entry := range entries {
		key, value, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		env[key] = value
	}
	return env
}

func mergeStringMaps(base, override map[string]string) map[string]string {
	switch {
	case len(base) == 0 && len(override) == 0:
		return nil
	case len(base) == 0:
		return cloneStringMap(override)
	case len(override) == 0:
		return cloneStringMap(base)
	}

	merged := cloneStringMap(base)
	for key, value := range override {
		merged[key] = value
	}
	return merged
}

func toRuntimeMCPServers(configs map[string]agentconfig.MCPServerConfig) map[string]acpagent.MCPServerConfig {
	if len(configs) == 0 {
		return nil
	}

	runtimeConfigs := make(map[string]acpagent.MCPServerConfig, len(configs))
	for name, cfg := range configs {
		runtimeConfigs[name] = acpagent.MCPServerConfig{
			Type:       toRuntimeMCPServerType(cfg.Type),
			Cmd:        append([]string(nil), cfg.Cmd...),
			Args:       append([]string(nil), cfg.Args...),
			Env:        cloneStringMap(cfg.Env),
			WorkingDir: cfg.WorkingDir,
			URL:        cfg.URL,
			Headers:    cloneStringMap(cfg.Headers),
		}
	}
	return runtimeConfigs
}

// selectMCPServers applies caller-declared equivalence before provider-specific
// construction, so hosted and ACP agents receive the same effective endpoints.
func selectMCPServers(resolvedMCP map[string]agentconfig.MCPServerConfig) (map[string]agentconfig.MCPServerConfig, error) {
	ids := make([]string, 0, len(resolvedMCP))
	for id := range resolvedMCP {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	// Equivalence is never inferred from the URL or headers: query parameters
	// and headers can select a tenant or an authorization context, so two
	// endpoints that differ that way are distinct servers. Only an explicit,
	// identical, non-empty DedupKey collapses configs.
	//
	// Within a group the config marked DedupPreferred survives; if none is
	// marked the lexicographically smallest id wins, so the survivor is always
	// deterministic and independent of map iteration order.
	keeperOf := make(map[string]string, len(ids))
	skip := make(map[string]bool, len(ids))
	for _, id := range ids {
		key := resolvedMCP[id].DedupKey
		if strings.TrimSpace(key) == "" {
			continue
		}
		keeper, ok := keeperOf[key]
		if !ok {
			keeperOf[key] = id
			continue
		}
		if resolvedMCP[id].DedupPreferred && resolvedMCP[keeper].DedupPreferred {
			return nil, fmt.Errorf("mcp dedup key %q has multiple preferred configs: %q and %q", key, keeper, id)
		}
		// A preferred entry replaces the current keeper; a preferred keeper is
		// never displaced.
		if resolvedMCP[id].DedupPreferred && !resolvedMCP[keeper].DedupPreferred {
			skip[keeper] = true
			keeperOf[key] = id
			continue
		}
		skip[id] = true
	}

	selected := make(map[string]agentconfig.MCPServerConfig, len(resolvedMCP))
	for _, id := range ids {
		if !skip[id] {
			selected[id] = resolvedMCP[id]
		}
	}
	return selected, nil
}

func hostedToolsets(requestToolsets []tool.Toolset, resolvedMCP map[string]agentconfig.MCPServerConfig, availabilityIndependent ...bool) ([]tool.Toolset, []*ownedMCPTransport, error) {
	toolsets := append([]tool.Toolset(nil), requestToolsets...)
	if len(resolvedMCP) == 0 {
		return toolsets, nil, nil
	}

	ids := make([]string, 0, len(resolvedMCP))
	for id := range resolvedMCP {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	var transports []*ownedMCPTransport
	for _, id := range ids {
		transport, err := mcpTransportFactory(resolvedMCP[id])
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("create mcp transport %q: %w", id, err), closeMCPTransports(transports))
		}
		owned := &ownedMCPTransport{Transport: transport}
		transports = append(transports, owned)
		toolset, err := mcptoolset.New(mcptoolset.Config{Transport: owned})
		if err != nil {
			return nil, nil, errors.Join(fmt.Errorf("create mcp toolset %q: %w", id, err), closeMCPTransports(transports))
		}
		if len(availabilityIndependent) > 0 && availabilityIndependent[0] {
			bounded := newBoundedMCPToolset(toolset, mcpSetupTimeout, mcpToolTimeout)
			owned.operations = bounded
			toolset = bounded
		}
		toolsets = append(toolsets, toolset)
	}
	return toolsets, transports, nil
}

// mcpTransportFactory builds the transport for one resolved MCP server config.
// It is a variable so tests can observe which endpoint actually survives
// deduplication instead of re-deriving the winner with a copy of the algorithm.
var mcpTransportFactory = mcpTransportForConfig

func mcpTransportForConfig(cfg agentconfig.MCPServerConfig) (mcp.Transport, error) {
	switch cfg.Type {
	case agentconfig.MCPServerTypeStdio:
		if len(cfg.Cmd) == 0 {
			return nil, fmt.Errorf("stdio mcp server requires cmd")
		}
		args := append([]string(nil), cfg.Cmd[1:]...)
		args = append(args, cfg.Args...)
		cmd := exec.Command(cfg.Cmd[0], args...)
		cmd.Dir = strings.TrimSpace(cfg.WorkingDir)
		cmd.Env = stringMapEnv(cfg.Env)
		return &mcp.CommandTransport{Command: cmd}, nil
	case agentconfig.MCPServerTypeHTTP:
		return &mcp.StreamableClientTransport{
			Endpoint:   strings.TrimSpace(cfg.URL),
			HTTPClient: httpClientWithHeaders(cfg.Headers),
		}, nil
	case agentconfig.MCPServerTypeSSE:
		return &mcp.SSEClientTransport{
			Endpoint:   strings.TrimSpace(cfg.URL),
			HTTPClient: httpClientWithHeaders(cfg.Headers),
		}, nil
	default:
		return nil, fmt.Errorf("unsupported mcp server type %q", cfg.Type)
	}
}

func stringMapEnv(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}
	keys := make([]string, 0, len(env))
	for key := range env {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, key := range keys {
		out = append(out, key+"="+env[key])
	}
	return out
}

func httpClientWithHeaders(headers map[string]string) *http.Client {
	client := &http.Client{}
	if len(headers) == 0 {
		*client = *http.DefaultClient
	}
	base := client.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	client.Transport = staticHeaderRoundTripper{base: base, headers: cloneStringMap(headers)}
	return client
}

type staticHeaderRoundTripper struct {
	base    http.RoundTripper
	headers map[string]string
}

func (t staticHeaderRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	for key, value := range t.headers {
		clone.Header.Set(key, value)
	}
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	if req.Method != http.MethodDelete {
		return base.RoundTrip(clone)
	}
	// Older SDKs issue shutdown DELETE with a detached context and no timeout.
	// Bound only session deletion; ordinary calls and SSE remain unrestricted.
	ctx, cancel := context.WithTimeout(clone.Context(), mcpCloseTimeout)
	defer cancel()
	response, err := base.RoundTrip(clone.Clone(ctx))
	if response != nil && response.Body != nil {
		// The SDK does not consume the DELETE response body. Release it before
		// cancelling the request so it cannot retain a connection after shutdown.
		err = errors.Join(err, response.Body.Close())
		response.Body = http.NoBody
	}
	return response, err
}

func toRuntimeMCPServerType(serverType agentconfig.MCPServerType) acpagent.MCPServerType {
	switch serverType {
	case agentconfig.MCPServerTypeStdio:
		return acpagent.MCPServerTypeStdio
	case agentconfig.MCPServerTypeHTTP:
		return acpagent.MCPServerTypeHTTP
	case agentconfig.MCPServerTypeSSE:
		return acpagent.MCPServerTypeSSE
	default:
		return acpagent.MCPServerType(serverType)
	}
}

func cloneStringMap(src map[string]string) map[string]string {
	if len(src) == 0 {
		return nil
	}
	cloned := make(map[string]string, len(src))
	for k, v := range src {
		cloned[k] = v
	}
	return cloned
}

// constructors registry.
var constructors = map[string]constructor{
	agentconfig.AgentTypeGenericACP: acpConstructor,
	agentconfig.AgentTypeOpenAI:     openAIConstructor,
	agentconfig.AgentTypeAIStudio:   aistudioConstructor,
	agentconfig.AgentTypePool:       poolConstructor,
}

func newACPAgentDefault(cfg acpagent.Config) (agent.Agent, error) {
	return acpagent.New(cfg)
}

func newHostedAgentDefault(cfg hostedagent.Config) (agent.Agent, error) {
	return hostedagent.New(cfg)
}

func newOpenAIModelDefault(apiKey, modelName string, timeout time.Duration, reasoningEffort string) (model.LLM, error) {
	return hostedagent.NewOpenAIModelWithOptions(apiKey, modelName, hostedagent.OpenAIModelOptions{
		Timeout: timeout, ReasoningEffort: reasoningEffort,
	})
}

func newAIStudioModelDefault(ctx context.Context, apiKey, modelName string) (model.LLM, error) {
	return hostedagent.NewAIStudioModel(ctx, apiKey, modelName)
}

var newACPAgent = newACPAgentDefault

var newHostedAgent = newHostedAgentDefault

var newOpenAIModel = newOpenAIModelDefault

var newAIStudioModel = newAIStudioModelDefault

func effectiveName(req BuildRequest) string {
	name := strings.TrimSpace(req.Name)
	if name != "" {
		return name
	}
	return req.AgentID
}

func effectiveDescription(req BuildRequest, cfg agentconfig.ResolvedConfig) string {
	description := strings.TrimSpace(req.Description)
	if description != "" {
		return description
	}
	return cfg.Description(req.AgentID)
}

func effectiveInstruction(req BuildRequest, cfg agentconfig.ResolvedConfig) string {
	override := strings.TrimSpace(req.Instruction)
	if override != "" {
		return override
	}
	return strings.TrimSpace(cfg.SystemInstructions)
}

func effectiveGlobalInstruction(req BuildRequest) string {
	return strings.TrimSpace(req.GlobalInstruction)
}

func effectiveReasoningEffort(req BuildRequest, cfg agentconfig.ResolvedConfig, schemaType string) (string, error) {
	_ = schemaType
	override := strings.TrimSpace(req.ReasoningEffort)
	if override == "" {
		override = strings.TrimSpace(cfg.ReasoningEffort)
	}
	if override == "" {
		return "", nil
	}
	return override, nil
}

var acpConstructor = func(ctx context.Context, cfg agentconfig.ResolvedConfig, req BuildRequest, f *Factory, resolvedMCP map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
	if cfg.Type != agentconfig.AgentTypeGenericACP {
		return nil, fmt.Errorf("unknown acp agent type %q", cfg.Type)
	}
	if hasRequestTools(req) {
		return nil, fmt.Errorf("%s agent does not support request tools", cfg.Type)
	}
	if len(cfg.Command) == 0 {
		return nil, fmt.Errorf("generic_acp agent requires cmd")
	}
	schemaCfg, err := f.GetAgentConfig(req.AgentID)
	if err != nil {
		return nil, err
	}
	reasoningEffort, err := effectiveReasoningEffort(req, cfg, schemaCfg.Type)
	if err != nil {
		return nil, err
	}

	setupTimeout := time.Duration(0)
	if f.availabilityIndependentMCP {
		setupTimeout = mcpSetupTimeout
	}
	environment, err := cfg.Environment.Resolve(f.processEnvironment)
	if err != nil {
		return nil, fmt.Errorf("resolve ACP process environment: %w", err)
	}
	return newACPAgent(acpagent.Config{
		Context:            ctx,
		LazyInitialization: f.availabilityIndependentMCP,
		SetupTimeout:       setupTimeout,
		InitializeTimeout:  f.acpInitializeTimeout,

		Name:              effectiveName(req),
		Description:       effectiveDescription(req, cfg),
		SessionConfig:     acpSessionConfigValues(cfg.Model, cfg.ModelConfigID, reasoningEffort, cfg.ReasoningEffortConfigID, cfg.Mode),
		Instruction:       effectiveInstruction(req, cfg),
		GlobalInstruction: effectiveGlobalInstruction(req),
		Command:           append([]string(nil), cfg.Command...),
		Environment:       environment,
		WorkingDir:        req.WorkingDirectory,
		ReasoningEffort:   reasoningEffort,
		Stderr:            f.stderrWriter,
		PermissionHandler: f.permissionHandler,
		Logger:            slog.Default().With("component", "runtime.agentfactory.acp"),
		MCPServers:        toRuntimeMCPServers(resolvedMCP),
		OutputKey:         strings.TrimSpace(req.OutputKey),
	})
}

func acpSessionConfigValues(modelName, modelConfigID, reasoningEffort, reasoningEffortConfigID, mode string) []acpagent.SessionConfigValue {
	values := make([]acpagent.SessionConfigValue, 0, 3)
	if modelName = strings.TrimSpace(modelName); modelName != "" {
		values = append(values, acpagent.SelectSessionConfigValue(defaultString(modelConfigID, "model"), modelName))
	}
	if reasoningEffort = strings.TrimSpace(reasoningEffort); reasoningEffort != "" {
		values = append(values, acpagent.SelectSessionConfigValue(defaultString(reasoningEffortConfigID, "reasoning_effort"), reasoningEffort))
	}
	if mode = strings.TrimSpace(mode); mode != "" {
		values = append(values, acpagent.SelectSessionConfigValue("mode", mode))
	}
	return values
}

func defaultString(value, fallback string) string {
	if value = strings.TrimSpace(value); value != "" {
		return value
	}
	return fallback
}

var poolConstructor = func(ctx context.Context, cfg agentconfig.ResolvedConfig, req BuildRequest, f *Factory, _ map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
	if hasRequestTools(req) {
		return nil, fmt.Errorf("pool agent does not support request tools")
	}
	members, err := validatePoolMembers(req.AgentID, cfg.PoolMembers, f.registry)
	if err != nil {
		return nil, err
	}

	poolMembers := make([]poolagent.MemberConfig, len(members))
	for i, m := range members {
		poolMembers[i] = poolagent.MemberConfig{Name: m.Name, Cfg: m.Cfg}
	}

	poolReq := poolagent.AgentRequest{
		Name:               effectiveName(req),
		Description:        effectiveDescription(req, cfg),
		SystemInstructions: effectiveInstruction(req, cfg),
		WorkingDirectory:   req.WorkingDirectory,
	}

	creator := &factoryAgentCreator{factory: f}
	return poolagent.NewPoolAgent(ctx, req.AgentID, poolMembers, poolReq, creator)
}

var openAIConstructor = func(ctx context.Context, cfg agentconfig.ResolvedConfig, req BuildRequest, f *Factory, resolvedMCP map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
	if cfg.Type != agentconfig.AgentTypeOpenAI {
		return nil, fmt.Errorf("unknown openai agent type %q", cfg.Type)
	}
	llmModel, err := newOpenAIModel(cfg.APIKey, cfg.Model, cfg.Timeout, cfg.ReasoningEffort)
	if err != nil {
		return nil, err
	}

	return buildHostedAgent(hostedagent.Config{
		Name:              effectiveName(req),
		Description:       effectiveDescription(req, cfg),
		Instruction:       effectiveInstruction(req, cfg),
		GlobalInstruction: effectiveGlobalInstruction(req),
		Model:             llmModel,
		Tools:             append([]tool.Tool(nil), req.Tools...),
		Toolsets:          req.Toolsets,
	}, resolvedMCP, f.availabilityIndependentMCP)
}

var aistudioConstructor = func(ctx context.Context, cfg agentconfig.ResolvedConfig, req BuildRequest, f *Factory, resolvedMCP map[string]agentconfig.MCPServerConfig) (agent.Agent, error) {
	if cfg.Type != agentconfig.AgentTypeAIStudio {
		return nil, fmt.Errorf("unknown aistudio agent type %q", cfg.Type)
	}
	llmModel, err := newAIStudioModel(ctx, cfg.APIKey, cfg.Model)
	if err != nil {
		return nil, err
	}

	return buildHostedAgent(hostedagent.Config{
		Name:              effectiveName(req),
		Description:       effectiveDescription(req, cfg),
		Instruction:       effectiveInstruction(req, cfg),
		GlobalInstruction: effectiveGlobalInstruction(req),
		Model:             llmModel,
		Tools:             append([]tool.Tool(nil), req.Tools...),
		Toolsets:          req.Toolsets,
	}, resolvedMCP, f.availabilityIndependentMCP)
}

func hasRequestTools(req BuildRequest) bool {
	return len(req.Tools) > 0 || len(req.Toolsets) > 0
}

type factoryAgentCreator struct {
	factory *Factory
}

func (f *factoryAgentCreator) CreateAgent(ctx context.Context, name string, req poolagent.AgentRequest) (agent.Agent, error) {
	buildReq := BuildRequest{
		AgentID:          name,
		Name:             req.Name,
		Description:      req.Description,
		Instruction:      req.SystemInstructions,
		WorkingDirectory: req.WorkingDirectory,
	}
	return f.factory.Build(ctx, buildReq)
}

type poolMemberConfig struct {
	Name string
	Cfg  agentconfig.Config
}

func validatePoolMembers(poolName string, pool []string, registry map[string]agentconfig.Config) ([]poolMemberConfig, error) {
	if len(pool) == 0 {
		return nil, fmt.Errorf("pool agent requires pool members")
	}
	members := make([]poolMemberConfig, 0, len(pool))
	for i, memberName := range pool {
		memberName = strings.TrimSpace(memberName)
		if memberName == "" {
			return nil, fmt.Errorf("pool member at index %d is empty", i)
		}
		if memberName == poolName {
			return nil, fmt.Errorf("pool cannot reference itself")
		}
		memberCfg, ok := registry[memberName]
		if !ok {
			return nil, fmt.Errorf("pool references unknown agent %q", memberName)
		}
		if agentconfig.IsPoolType(memberCfg.Type) {
			return nil, fmt.Errorf("pool cannot contain nested pool %q", memberName)
		}
		members = append(members, poolMemberConfig{Name: memberName, Cfg: memberCfg})
	}
	return members, nil
}
