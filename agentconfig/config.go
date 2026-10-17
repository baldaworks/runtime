package agentconfig

import (
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/go-playground/validator/v10"
)

// MCPServerType represents the transport type for an MCP server.
type MCPServerType string

const (
	// MCPServerTypeStdio is the stdio transport type.
	MCPServerTypeStdio MCPServerType = "stdio"
	// MCPServerTypeHTTP is the HTTP transport type.
	MCPServerTypeHTTP MCPServerType = "http"
	// MCPServerTypeSSE is the SSE (Server-Sent Events) transport type.
	MCPServerTypeSSE MCPServerType = "sse"
)

// MCPServerConfig describes how to connect to an MCP server.
type MCPServerConfig struct {
	// Type selects the MCP transport implementation.
	Type MCPServerType `json:"type"                  yaml:"type"                  mapstructure:"type"                validate:"required,oneof=stdio http sse,mcp_requirements"`
	// Cmd is the stdio server executable path or argv prefix.
	Cmd []string `json:"cmd,omitempty"         yaml:"cmd,omitempty"         mapstructure:"cmd"                 validate:"omitempty,dive,notblank"`
	// Args appends additional stdio server arguments after Cmd.
	Args []string `json:"args,omitempty"        yaml:"args,omitempty"        mapstructure:"args"                validate:"omitempty,dive,notblank"`
	// Env defines environment variables for stdio server execution.
	Env map[string]string `json:"env,omitempty"         yaml:"env,omitempty"         mapstructure:"env"`
	// WorkingDir sets the stdio server process working directory.
	WorkingDir string `json:"working_dir,omitempty" yaml:"working_dir,omitempty" mapstructure:"working_dir"         validate:"omitempty,notblank"`
	// URL is the base endpoint for HTTP and SSE MCP transports.
	URL string `json:"url,omitempty"         yaml:"url,omitempty"         mapstructure:"url"`
	// Headers provides additional request headers for HTTP and SSE transports.
	Headers map[string]string `json:"headers,omitempty"     yaml:"headers,omitempty"     mapstructure:"headers"`
	// DedupKey marks endpoints that are known to serve the same MCP tool
	// namespace, so only one of them may be registered with an agent.
	//
	// It exists because ADK flattens every toolset into a single tool map and
	// packing is not idempotent: two servers exposing the same tool name abort
	// the turn with `duplicate tool`. A caller that knowingly registers one
	// server twice (for example a bundled server plus a session-scoped binding
	// of the same endpoint) sets the same DedupKey on both entries.
	//
	// The key is opaque and caller-defined; the runtime never derives it from
	// the URL or headers, because query parameters and headers can select a
	// tenant, an authorization context, or server behavior. Endpoints are
	// deduplicated only when both configs carry the same non-empty key, so a
	// zero value disables deduplication entirely.
	DedupKey string `json:"dedup_key,omitempty"   yaml:"dedup_key,omitempty"   mapstructure:"dedup_key"         validate:"omitempty,notblank"`
	// DedupPreferred selects the survivor within a DedupKey group when the
	// entries are not interchangeable.
	//
	// At most one config per DedupKey may set it. Balda needs it because its
	// session-scoped binding carries the context token that session-memory
	// tools require, while the plain bundled endpoint does not; keeping the
	// plain entry would silently strip that capability.
	//
	// If no member of a group is marked, the lexicographically smallest id
	// wins so the survivor stays deterministic.
	DedupPreferred bool `json:"dedup_preferred,omitempty" yaml:"dedup_preferred,omitempty" mapstructure:"dedup_preferred"`
}

// ACPConfig is an ACP runtime configuration block used by generic and alias types.
type ACPConfig struct {
	// Cmd is the ACP executable argv prefix before any alias-specific defaults.
	Cmd []string `json:"cmd,omitempty"        yaml:"cmd,omitempty"        mapstructure:"cmd"        validate:"omitempty,dive,notblank"`
	// ExtraArgs appends CLI arguments after the resolved ACP command.
	ExtraArgs []string `json:"extra_args,omitempty" yaml:"extra_args,omitempty" mapstructure:"extra_args" validate:"omitempty,dive,notblank"`
	// Model selects the runtime model identifier when the backend supports it.
	Model string `json:"model,omitempty"      yaml:"model,omitempty"      mapstructure:"model"      validate:"omitempty,notblank"`
	// ModelConfigID is the ACP session config option ID used for Model.
	ModelConfigID string `json:"model_config_id,omitempty" yaml:"model_config_id,omitempty" mapstructure:"model_config_id" validate:"omitempty,notblank"`
	// ReasoningEffort selects the provider reasoning effort when supported.
	ReasoningEffort string `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty" mapstructure:"reasoning_effort" validate:"omitempty,oneof=minimal low medium high xhigh"`
	// ReasoningEffortConfigID is the ACP session config option ID used for ReasoningEffort.
	ReasoningEffortConfigID string `json:"reasoning_effort_config_id,omitempty" yaml:"reasoning_effort_config_id,omitempty" mapstructure:"reasoning_effort_config_id" validate:"omitempty,notblank"`
	// Mode selects the runtime session mode when the backend supports it.
	Mode string `json:"mode,omitempty"       yaml:"mode,omitempty"       mapstructure:"mode"       validate:"omitempty,notblank"`
	// BridgeVersion selects the Codex ACP npm version or dist-tag.
	BridgeVersion string `json:"bridge_version,omitempty" yaml:"bridge_version,omitempty" mapstructure:"bridge_version" validate:"omitempty,notblank"`
	// RegistryID selects the agent identifier within the ACP Registry.
	RegistryID string `json:"registry_id,omitempty" yaml:"registry_id,omitempty" mapstructure:"registry_id" validate:"omitempty,notblank"`
	// InheritEnv controls whether the ACP subprocess starts from the runtime
	// process environment. Nil defaults to true.
	InheritEnv *bool `json:"inherit_env,omitempty" yaml:"inherit_env,omitempty" mapstructure:"inherit_env"`
	// Env overlays exact environment variables for the ACP subprocess.
	Env map[string]string `json:"env,omitempty" yaml:"env,omitempty" mapstructure:"env"`
}

const (
	codexACPPackageName    = "codex-acp"
	defaultCodexACPVersion = "1.11.0"
	acpRunPackageName      = "@baldaworks/acprun"
	defaultACPRunVersion   = "0.1.6"
	npxCommand             = "npx"
	npxYesFlag             = "-y"
)

func acpRunPackage(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "@")
	if version == "" {
		version = defaultACPRunVersion
	}
	return acpRunPackageName + "@" + version
}

// LocalAPIConfig is a local API-backed runtime configuration block.
type LocalAPIConfig struct {
	// APIKey authenticates requests to the hosted provider API.
	APIKey string `json:"api_key,omitempty" yaml:"api_key,omitempty" mapstructure:"api_key"`
	// Model selects the hosted provider model identifier.
	Model string `json:"model,omitempty"   yaml:"model,omitempty"   mapstructure:"model"   validate:"omitempty,notblank"`
	// Timeout limits one HTTP request to the hosted provider.
	Timeout string `json:"timeout,omitempty" yaml:"timeout,omitempty" mapstructure:"timeout"`
	// ReasoningEffort selects the hosted provider reasoning effort when supported.
	ReasoningEffort string `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty" mapstructure:"reasoning_effort" validate:"omitempty,oneof=none"`
}

// PoolConfig is the pool runtime configuration block.
type PoolConfig struct {
	// Members lists provider IDs in ordered failover order.
	Members []string `json:"members,omitempty" yaml:"members,omitempty" mapstructure:"members" validate:"omitempty,dive,notblank"`
}

// Config describes how to run an agent.
//
// The schema is strict and discriminated by type:
//
//	type: <agent_type>
//	<agent_type>:
//	  ...type-specific config...
type Config struct {
	// Type selects the runtime backend implementation.
	Type string `json:"type"                           yaml:"type"                           mapstructure:"type"                validate:"required,agent_type,agent_blocks"`
	// MCPServers lists MCP server IDs resolved by higher-level registries.
	MCPServers []string `json:"mcp_servers,omitempty"          yaml:"mcp_servers,omitempty"          mapstructure:"mcp_servers"         validate:"omitempty,dive,notblank"`
	// SystemInstructions defines provider-level instructions applied by default.
	SystemInstructions string `json:"system_instructions,omitempty"  yaml:"system_instructions,omitempty"  mapstructure:"system_instructions" validate:"omitempty,notblank"`

	// GenericACP configures a custom ACP executable.
	GenericACP *ACPConfig `json:"generic_acp,omitempty"     yaml:"generic_acp,omitempty"     mapstructure:"generic_acp"`
	// GeminiACP configures the Gemini ACP alias.
	GeminiACP *ACPConfig `json:"gemini_acp,omitempty"      yaml:"gemini_acp,omitempty"      mapstructure:"gemini_acp"`
	// CodexACP configures the Codex ACP alias.
	CodexACP *ACPConfig `json:"codex_acp,omitempty"       yaml:"codex_acp,omitempty"       mapstructure:"codex_acp"`
	// OpenCodeACP configures the OpenCode ACP alias.
	OpenCodeACP *ACPConfig `json:"opencode_acp,omitempty"    yaml:"opencode_acp,omitempty"    mapstructure:"opencode_acp"`
	// CopilotACP configures the Copilot ACP alias.
	CopilotACP *ACPConfig `json:"copilot_acp,omitempty"     yaml:"copilot_acp,omitempty"     mapstructure:"copilot_acp"`
	// ClaudeCodeACP configures the Claude Code ACP alias.
	ClaudeCodeACP *ACPConfig `json:"claude_code_acp,omitempty" yaml:"claude_code_acp,omitempty" mapstructure:"claude_code_acp"`
	// GrokACP configures the Grok Build ACP alias.
	GrokACP *ACPConfig `json:"grok_acp,omitempty"        yaml:"grok_acp,omitempty"        mapstructure:"grok_acp"`
	// RegistryACP configures an ACP Registry agent.
	RegistryACP *ACPConfig `json:"registry_acp,omitempty"    yaml:"registry_acp,omitempty"    mapstructure:"registry_acp"`
	// AgyACP configures the Antigravity CLI ACP alias.
	AgyACP *ACPConfig `json:"agy_acp,omitempty"         yaml:"agy_acp,omitempty"         mapstructure:"agy_acp"`
	// AntigravityACP configures the Antigravity CLI ACP alias (alias for agy_acp).
	AntigravityACP *ACPConfig `json:"antigravity_acp,omitempty" yaml:"antigravity_acp,omitempty" mapstructure:"antigravity_acp"`
	// OpenAI configures a local OpenAI-backed hosted agent.
	OpenAI *LocalAPIConfig `json:"openai,omitempty"         yaml:"openai,omitempty"          mapstructure:"openai"`
	// AIStudio configures a local Google AI Studio-backed hosted agent.
	AIStudio *LocalAPIConfig `json:"aistudio,omitempty"       yaml:"aistudio,omitempty"        mapstructure:"aistudio"`
	// PoolConfig configures an ordered failover pool.
	PoolConfig *PoolConfig `json:"pool,omitempty"           yaml:"pool,omitempty"            mapstructure:"pool"`
}

// ResolvedConfig is a runtime-ready agent configuration produced from Config normalization.
type ResolvedConfig struct {
	// ConfiguredType is the provider type exactly as selected in configuration.
	ConfiguredType string
	// DriverType is the canonical provider driver type after alias resolution.
	DriverType string
	// Type is the normalized runtime backend type.
	Type string
	// MCPServers are the resolved MCP server IDs referenced by the runtime.
	MCPServers []string
	// SystemInstructions are the normalized provider-level instructions.
	SystemInstructions string
	// ReasoningEffort is the normalized provider reasoning effort.
	ReasoningEffort string
	// ModelConfigID is the ACP session config option ID used for Model.
	ModelConfigID string
	// ReasoningEffortConfigID is the ACP session config option ID used for ReasoningEffort.
	ReasoningEffortConfigID string

	// Command is the runtime command argv for ACP-backed agents.
	Command []string
	// Environment is the validated ACP process environment policy.
	Environment EnvironmentPolicy
	// RegistryID is the normalized external ACP Registry identifier.
	RegistryID string
	// APIKey is the resolved hosted-provider credential.
	APIKey string
	// Model is the resolved runtime model identifier.
	Model string
	// Timeout limits one hosted-provider HTTP request. Zero uses the default.
	Timeout time.Duration
	// Mode is the resolved runtime mode identifier.
	Mode string
	// PoolMembers are the resolved provider IDs in pool failover order.
	PoolMembers []string
}

// Description returns a human-readable description of the agent config.
// Format: "name: type=<type> model=<model> mode=<mode>" with missing parts omitted.
func (c Config) Description(name string) string {
	var parts []string
	if c.Type != "" {
		parts = append(parts, fmt.Sprintf("type=%s", c.Type))
	}
	if spec, ok := c.selectedACPBlock(); ok {
		if spec.Model != "" {
			parts = append(parts, fmt.Sprintf("model=%s", spec.Model))
		}
		if spec.Mode != "" {
			parts = append(parts, fmt.Sprintf("mode=%s", spec.Mode))
		}
	}
	if spec, ok := c.selectedLocalAPIBlock(); ok {
		if spec.Model != "" {
			parts = append(parts, fmt.Sprintf("model=%s", spec.Model))
		}
	}
	if len(parts) == 0 {
		return name
	}
	return fmt.Sprintf("%s: %s", name, strings.Join(parts, " "))
}

// Description returns a human-readable description of the resolved runtime config.
func (c ResolvedConfig) Description(name string) string {
	var parts []string
	if c.Type != "" {
		parts = append(parts, fmt.Sprintf("type=%s", c.Type))
	}
	if c.Model != "" {
		parts = append(parts, fmt.Sprintf("model=%s", c.Model))
	}
	if c.Mode != "" {
		parts = append(parts, fmt.Sprintf("mode=%s", c.Mode))
	}
	if len(parts) == 0 {
		return name
	}
	return fmt.Sprintf("%s: %s", name, strings.Join(parts, " "))
}

var configValidator = newConfigValidator()

func newConfigValidator() *validator.Validate {
	v := validator.New()
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("json"), ",", 2)[0]
		if name == "" || name == "-" {
			return fld.Name
		}
		return name
	})
	_ = v.RegisterValidation("notblank", validateNotBlank)
	_ = v.RegisterValidation("agent_type", validateAgentType)
	_ = v.RegisterValidation("agent_blocks", validateAgentBlocks)
	_ = v.RegisterValidation("mcp_requirements", validateMCPRequirements)
	return v
}

// Validate validates the agent configuration.
func (c Config) Validate() error {
	errs := make([]string, 0)

	if err := configValidator.Struct(c); err != nil {
		if invErr, ok := err.(*validator.InvalidValidationError); ok {
			return fmt.Errorf("validate agent config: %w", invErr)
		}
		for _, validationErr := range err.(validator.ValidationErrors) {
			if validationErr.Tag() == "agent_blocks" {
				errs = append(errs, explainAgentBlocksError(c))
				continue
			}
			errs = append(errs, formatValidationError(validationErr))
		}
	}

	if len(errs) == 0 {
		if err := validateAgentConfigSemantics(c); err != nil {
			errs = append(errs, err.Error())
		}
	}

	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)

	return fmt.Errorf("agent config schema validation failed: %s", strings.Join(errs, "; "))
}

func formatValidationError(err validator.FieldError) string {
	field := err.Field()
	switch err.Tag() {
	case "required":
		return field + " is required"
	case "oneof":
		return field + " must be one of: " + err.Param()
	case "agent_type":
		return field + " must be one of: " + strings.Join(SupportedAgentTypes(), " ")
	case "min":
		return field + " must be at least " + err.Param()
	case "notblank":
		return field + " must have at least 1 character"
	case "agent_blocks":
		return "type-specific block configuration is invalid for selected type"
	case "mcp_requirements":
		return "mcp server type-specific requirements are invalid"
	default:
		return field + " failed validation rule " + err.Tag()
	}
}

func validateAgentType(fl validator.FieldLevel) bool {
	_, ok := builtinDriverRegistry.lookup(strings.TrimSpace(fl.Field().String()))
	return ok
}

const (
	// AgentTypeGenericACP is the type for custom ACP CLI executables.
	AgentTypeGenericACP = "generic_acp"

	// AgentTypeGeminiACP is a deprecated alias for Gemini CLI ACP mode.
	AgentTypeGeminiACP = "gemini_acp"
	// AgentTypeCodexACP is the alias for Codex ACP bridge mode.
	AgentTypeCodexACP = "codex_acp"
	// AgentTypeOpenCodeACP is the alias for OpenCode CLI ACP mode.
	AgentTypeOpenCodeACP = "opencode_acp"
	// AgentTypeCopilotACP is the alias for Copilot CLI ACP mode.
	AgentTypeCopilotACP = "copilot_acp"
	// AgentTypeClaudeCodeACP is the alias for Claude Code ACP mode.
	AgentTypeClaudeCodeACP = "claude_code_acp"
	// AgentTypeClaudeACP is a compatibility alias for Claude Code ACP mode.
	AgentTypeClaudeACP = "claude_acp"
	// AgentTypeGrokACP is the alias for Grok Build ACP mode.
	AgentTypeGrokACP = "grok_acp"
	// AgentTypeRegistryACP is the type for ACP agents resolved from the ACP registry.
	AgentTypeRegistryACP = "registry_acp"
	// AgentTypeAgyACP is the alias for Antigravity CLI ACP mode.
	AgentTypeAgyACP = "agy_acp"
	// AgentTypeAntigravityACP is a compatibility alias for Antigravity CLI ACP mode.
	AgentTypeAntigravityACP = "antigravity_acp"
	// AgentTypeOpenAI is the type for local OpenAI-backed providers.
	AgentTypeOpenAI = "openai"
	// AgentTypeAIStudio is the type for local Google AI Studio-backed providers.
	AgentTypeAIStudio = "aistudio"
	// AgentTypePool is the pool type with ordered failover.
	AgentTypePool = "pool"
)

// SupportedAgentTypes returns all supported agent types.
func SupportedAgentTypes() []string {
	return builtinDriverRegistry.supportedTypes()
}

// IsValidAgentType reports whether the given type is a valid agent type.
func IsValidAgentType(agentType string) bool {
	registration, ok := builtinDriverRegistry.lookup(agentType)
	return ok && !registration.descriptor.Deprecated
}

// IsPoolType reports whether an agent type is a pool.
func IsPoolType(agentType string) bool {
	driver, ok := BuiltinDriverRegistry().Lookup(agentType)
	return ok && driver.Class == DriverClassPool
}

// IsACPType reports whether an agent type uses the ACP runtime.
func IsACPType(agentType string) bool {
	driver, ok := BuiltinDriverRegistry().Lookup(agentType)
	return ok && driver.Class == DriverClassACP && !driver.Deprecated
}

// IsPlannerSupportedType reports whether planner mode supports the agent type.
func IsPlannerSupportedType(agentType string) bool {
	return IsACPType(agentType)
}

// SupportedMCPServerTypes returns all supported MCP server types.
func SupportedMCPServerTypes() []MCPServerType {
	return []MCPServerType{
		MCPServerTypeStdio,
		MCPServerTypeHTTP,
		MCPServerTypeSSE,
	}
}

// IsValidMCPServerType reports whether the given type is a valid MCP server type.
func IsValidMCPServerType(serverType MCPServerType) bool {
	for _, t := range SupportedMCPServerTypes() {
		if t == serverType {
			return true
		}
	}
	return false
}

// ValidateMCPServerConfig validates an MCP server configuration.
func ValidateMCPServerConfig(cfg MCPServerConfig) error {
	errs := make([]string, 0)
	if cfg.DedupPreferred && strings.TrimSpace(cfg.DedupKey) == "" {
		errs = append(errs, "dedup_preferred requires dedup_key")
	}
	if err := configValidator.Struct(cfg); err != nil {
		if invErr, ok := err.(*validator.InvalidValidationError); ok {
			return fmt.Errorf("validate mcp server config: %w", invErr)
		}
		for _, validationErr := range err.(validator.ValidationErrors) {
			if validationErr.Tag() == "mcp_requirements" {
				errs = append(errs, explainMCPRequirementsError(cfg))
				continue
			}
			errs = append(errs, formatValidationError(validationErr))
		}
	}

	if len(errs) == 0 {
		return nil
	}
	sort.Strings(errs)
	return fmt.Errorf("mcp server config validation failed: %s", strings.Join(errs, "; "))
}

func validateNotBlank(fl validator.FieldLevel) bool {
	if fl.Field().Kind() != reflect.String {
		return false
	}
	return strings.TrimSpace(fl.Field().String()) != ""
}

func validateMCPRequirements(fl validator.FieldLevel) bool {
	cfg, ok := fl.Parent().Interface().(MCPServerConfig)
	if !ok {
		return false
	}
	switch cfg.Type {
	case MCPServerTypeStdio:
		return len(cfg.Cmd) > 0
	case MCPServerTypeHTTP, MCPServerTypeSSE:
		return strings.TrimSpace(cfg.URL) != ""
	default:
		return false
	}
}

func validateAgentBlocks(fl validator.FieldLevel) bool {
	cfg, ok := fl.Parent().Interface().(Config)
	if !ok {
		return false
	}
	registration, ok := builtinDriverRegistry.lookup(cfg.Type)
	if !ok {
		return false
	}
	present := configuredBlockNames(cfg)
	if len(present) == 0 && registration.descriptor.Type == AgentTypeCopilotACP {
		return true
	}
	if len(present) != 1 {
		return false
	}
	return registration.blockValid(cfg)
}

func explainAgentBlocksError(cfg Config) string {
	registration, ok := builtinDriverRegistry.lookup(cfg.Type)
	if !ok {
		return fmt.Sprintf("unsupported agent type %q", cfg.Type)
	}
	present := configuredBlockNames(cfg)
	if len(present) == 0 && registration.descriptor.Type == AgentTypeCopilotACP {
		return ""
	}
	if len(present) != 1 {
		return "exactly one type-specific block must be set"
	}

	rawTypeName := strings.TrimSpace(cfg.Type)
	if !registration.blockPresent(cfg) {
		return fmt.Sprintf("%s block must be omitted when type is %s", present[0], rawTypeName)
	}
	if message := registration.blockError(cfg, rawTypeName); message != "" {
		return message
	}
	return "type-specific block configuration is invalid for selected type"
}

func explainMCPRequirementsError(cfg MCPServerConfig) string {
	switch cfg.Type {
	case MCPServerTypeStdio:
		return "cmd is required for stdio type"
	case MCPServerTypeHTTP, MCPServerTypeSSE:
		return "url is required for http/sse type"
	default:
		return "mcp server type-specific requirements are invalid"
	}
}

// NormalizeConfig canonicalizes aliases and returns a runtime-ready configuration.
func NormalizeConfig(cfg Config, executablePath string) (ResolvedConfig, error) {
	if err := validateAgentConfigSemantics(cfg); err != nil {
		return ResolvedConfig{}, err
	}
	registration, ok := builtinDriverRegistry.lookup(cfg.Type)
	if !ok {
		return ResolvedConfig{}, fmt.Errorf("unsupported agent type %q", cfg.Type)
	}
	resolved := ResolvedConfig{
		ConfiguredType:     strings.TrimSpace(cfg.Type),
		DriverType:         registration.descriptor.Type,
		MCPServers:         append([]string(nil), cfg.MCPServers...),
		SystemInstructions: cfg.SystemInstructions,
	}
	return registration.resolve(cfg, resolved, executablePath)
}

// NormalizeConfigs canonicalizes agent configs for a map of named config blocks.
func NormalizeConfigs(cfgs map[string]Config, executablePath string) (map[string]ResolvedConfig, error) {
	if len(cfgs) == 0 {
		return map[string]ResolvedConfig{}, nil
	}

	resolved := make(map[string]ResolvedConfig, len(cfgs))
	for name, cfg := range cfgs {
		normCfg, err := NormalizeConfig(cfg, executablePath)
		if err != nil {
			return nil, fmt.Errorf("normalize agent %q: %w", name, err)
		}
		resolved[name] = normCfg
	}

	return resolved, nil
}

// NormalizeACPConfig is kept for compatibility and delegates to NormalizeConfig.
func NormalizeACPConfig(cfg Config, executablePath string) (ResolvedConfig, error) {
	return NormalizeConfig(cfg, executablePath)
}

// NormalizeACPConfigs is kept for compatibility and delegates to NormalizeConfigs.
func NormalizeACPConfigs(cfgs map[string]Config, executablePath string) (map[string]ResolvedConfig, error) {
	return NormalizeConfigs(cfgs, executablePath)
}

func resolveACPConfig(base ResolvedConfig, resolvedType string, spec ACPConfig) ResolvedConfig {
	base.Type = resolvedType
	base.Environment = newEnvironmentPolicy(spec.InheritEnv, spec.Env)
	base.RegistryID = strings.TrimSpace(spec.RegistryID)
	base.Model = spec.Model
	base.ModelConfigID = strings.TrimSpace(spec.ModelConfigID)
	base.ReasoningEffort = strings.TrimSpace(spec.ReasoningEffort)
	base.ReasoningEffortConfigID = strings.TrimSpace(spec.ReasoningEffortConfigID)
	base.Mode = spec.Mode
	base.Command = resolveTemplatedArgs(spec.Cmd, spec.Model)
	if len(spec.ExtraArgs) > 0 {
		base.Command = append(base.Command, resolveTemplatedArgs(spec.ExtraArgs, spec.Model)...)
	}
	return base
}

func commandOrDefault(command, fallback []string) []string {
	if len(command) > 0 {
		return append([]string(nil), command...)
	}
	return append([]string(nil), fallback...)
}

func codexACPPackage(version string) string {
	version = strings.TrimPrefix(strings.TrimSpace(version), "@")
	if version == "" {
		version = defaultCodexACPVersion
	}
	return codexACPPackageName + "@" + version
}

func appendGeminiModelFlag(cmd []string, model string) []string {
	if model == "" {
		return cmd
	}
	return append(cmd, "--model", model)
}

func resolveTemplatedArgs(args []string, model string) []string {
	if len(args) == 0 {
		return nil
	}
	res := make([]string, len(args))
	for i, arg := range args {
		res[i] = strings.ReplaceAll(arg, "{{.Model}}", model)
	}
	return res
}

func (c Config) selectedACPBlock() (*ACPConfig, bool) {
	registration, ok := builtinDriverRegistry.lookup(c.Type)
	if !ok || registration.acpBlock == nil {
		return nil, false
	}
	block := registration.acpBlock(c)
	return block, block != nil
}

func (c Config) selectedLocalAPIBlock() (*LocalAPIConfig, bool) {
	registration, ok := builtinDriverRegistry.lookup(c.Type)
	if !ok || registration.localAPIBlock == nil {
		return nil, false
	}
	block := registration.localAPIBlock(c)
	return block, block != nil
}

func validateAgentConfigSemantics(cfg Config) error {
	registration, ok := builtinDriverRegistry.lookup(cfg.Type)
	if !ok {
		return fmt.Errorf("unsupported agent type %q", cfg.Type)
	}
	if registration.descriptor.Deprecated {
		return fmt.Errorf("%s is deprecated and no longer supported", registration.descriptor.Type)
	}
	if registration.validate != nil {
		if err := registration.validate(cfg); err != nil {
			return err
		}
	}
	if registration.acpBlock != nil {
		if block := registration.acpBlock(cfg); block != nil {
			return validateEnvironmentConfig(block.Env)
		}
	}
	return nil
}

func parseOpenAITimeout(value string) (time.Duration, error) {
	if value == "" {
		return 0, nil
	}
	timeout, err := time.ParseDuration(value)
	if err != nil || timeout <= 0 {
		return 0, fmt.Errorf("openai.timeout must be a positive duration: %q", value)
	}
	return timeout, nil
}

func canonicalAgentType(agentType string) string {
	registration, ok := builtinDriverRegistry.lookup(agentType)
	if !ok {
		return strings.TrimSpace(agentType)
	}
	return registration.descriptor.Type
}
