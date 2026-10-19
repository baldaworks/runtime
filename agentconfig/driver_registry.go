package agentconfig

import (
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// DriverClass identifies the runtime construction family for a provider.
type DriverClass string

const (
	// DriverClassHosted constructs an in-process hosted-model provider.
	DriverClassHosted DriverClass = "hosted"
	// DriverClassACP constructs an ACP subprocess provider.
	DriverClassACP DriverClass = "acp"
	// DriverClassPool constructs an ordered provider pool.
	DriverClassPool DriverClass = "pool"
)

// CapabilitySource describes where a provider capability is discovered.
type CapabilitySource string

const (
	// CapabilitySourceNone means the capability is unavailable.
	CapabilitySourceNone CapabilitySource = "none"
	// CapabilitySourceStatic means the driver descriptor defines the capability.
	CapabilitySourceStatic CapabilitySource = "static"
	// CapabilitySourceSession means the ACP session advertises the capability.
	CapabilitySourceSession CapabilitySource = "session"
	// CapabilitySourceProvider means the provider supplies the capability at runtime.
	CapabilitySourceProvider CapabilitySource = "provider"
)

// DriverDescriptor is the passive public description of a built-in provider driver.
type DriverDescriptor struct {
	Type        string
	Aliases     []string
	Class       DriverClass
	ConfigBlock string
	BackendType string
	// ContractVersion identifies the persisted configuration contract for this
	// canonical driver. It changes only when an older stored definition can no
	// longer be reconstructed and normalized with the same semantics.
	ContractVersion         uint32
	ImmutableIdentityFields []string
	SupportsEnvironment     bool
	ExecutableSource        CapabilitySource
	AuthenticationSource    CapabilitySource
	SessionOptionSource     CapabilitySource
	ModelDiscoverySource    CapabilitySource
	ConcurrencyPolicy       string
	Deprecated              bool
}

// DriverRegistry is an immutable registry of trusted built-in provider drivers.
type DriverRegistry struct {
	registrations map[string]*driverRegistration
	canonical     []*driverRegistration
}

// BuiltinDriverRegistry returns the process-wide immutable built-in registry.
func BuiltinDriverRegistry() *DriverRegistry {
	return builtinDriverRegistry
}

// Lookup returns a copied descriptor for a canonical type or compatibility alias.
func (r *DriverRegistry) Lookup(agentType string) (DriverDescriptor, bool) {
	registration, ok := r.lookup(agentType)
	if !ok {
		return DriverDescriptor{}, false
	}
	return cloneDriverDescriptor(registration.descriptor), true
}

// Descriptors returns copied canonical descriptors in stable registration order.
func (r *DriverRegistry) Descriptors() []DriverDescriptor {
	if r == nil {
		return nil
	}
	descriptors := make([]DriverDescriptor, 0, len(r.canonical))
	for _, registration := range r.canonical {
		descriptors = append(descriptors, cloneDriverDescriptor(registration.descriptor))
	}
	return descriptors
}

type driverRegistration struct {
	descriptor    DriverDescriptor
	blockPresent  func(Config) bool
	blockValid    func(Config) bool
	blockError    func(Config, string) string
	acpBlock      func(Config) *ACPConfig
	localAPIBlock func(Config) *LocalAPIConfig
	validate      func(Config) error
	resolve       func(Config, ResolvedConfig, string) (ResolvedConfig, error)
}

var builtinDriverRegistry = mustDriverRegistry(builtinDriverRegistrations())

func mustDriverRegistry(registrations []driverRegistration) *DriverRegistry {
	registry, err := newDriverRegistry(registrations)
	if err != nil {
		panic(err)
	}
	return registry
}

func newDriverRegistry(registrations []driverRegistration) (*DriverRegistry, error) {
	registry := &DriverRegistry{
		registrations: make(map[string]*driverRegistration),
		canonical:     make([]*driverRegistration, 0, len(registrations)),
	}
	for i := range registrations {
		registration := registrations[i]
		registration.descriptor.Type = strings.TrimSpace(registration.descriptor.Type)
		if registration.descriptor.Type == "" {
			return nil, fmt.Errorf("provider driver type must not be empty")
		}
		if registration.descriptor.ContractVersion == 0 {
			return nil, fmt.Errorf("provider driver %q contract version must not be zero", registration.descriptor.Type)
		}
		if registration.resolve == nil || registration.blockPresent == nil || registration.blockValid == nil || registration.blockError == nil {
			return nil, fmt.Errorf("provider driver %q is incomplete", registration.descriptor.Type)
		}
		stored := registration
		stored.descriptor = cloneDriverDescriptor(stored.descriptor)
		if err := registry.addKey(stored.descriptor.Type, &stored); err != nil {
			return nil, err
		}
		seenAliases := make(map[string]struct{}, len(stored.descriptor.Aliases))
		for aliasIndex, rawAlias := range stored.descriptor.Aliases {
			alias := strings.TrimSpace(rawAlias)
			if alias == "" {
				return nil, fmt.Errorf("provider driver %q has an empty alias", stored.descriptor.Type)
			}
			if _, ok := seenAliases[alias]; ok {
				return nil, fmt.Errorf("provider driver %q repeats alias %q", stored.descriptor.Type, alias)
			}
			seenAliases[alias] = struct{}{}
			stored.descriptor.Aliases[aliasIndex] = alias
			if err := registry.addKey(alias, &stored); err != nil {
				return nil, err
			}
		}
		registry.canonical = append(registry.canonical, &stored)
		registry.registrations[stored.descriptor.Type] = &stored
		for _, alias := range stored.descriptor.Aliases {
			registry.registrations[alias] = &stored
		}
	}
	return registry, nil
}

func (r *DriverRegistry) addKey(key string, registration *driverRegistration) error {
	if _, exists := r.registrations[key]; exists {
		return fmt.Errorf("duplicate provider driver key %q", key)
	}
	r.registrations[key] = registration
	return nil
}

func (r *DriverRegistry) lookup(agentType string) (*driverRegistration, bool) {
	if r == nil {
		return nil, false
	}
	registration, ok := r.registrations[strings.TrimSpace(agentType)]
	return registration, ok
}

func (r *DriverRegistry) supportedTypes() []string {
	types := make([]string, 0, len(r.registrations))
	for _, registration := range r.canonical {
		if registration.descriptor.Deprecated {
			continue
		}
		types = append(types, registration.descriptor.Type)
		types = append(types, registration.descriptor.Aliases...)
	}
	return types
}

func cloneDriverDescriptor(descriptor DriverDescriptor) DriverDescriptor {
	descriptor.Aliases = append([]string(nil), descriptor.Aliases...)
	descriptor.ImmutableIdentityFields = append([]string(nil), descriptor.ImmutableIdentityFields...)
	return descriptor
}

// EnvironmentPolicy is a validated ACP subprocess environment overlay.
// Values intentionally remain private so metadata projections cannot expose them.
type EnvironmentPolicy struct {
	inherit bool
	values  map[string]string
}

// Inherits reports whether the runtime process environment is used as the base.
func (p EnvironmentPolicy) Inherits() bool {
	return p.inherit
}

// Names returns configured overlay names without exposing their values.
func (p EnvironmentPolicy) Names() []string {
	names := make([]string, 0, len(p.values))
	for name := range p.values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// LogValue exposes only environment names and inheritance state to slog.
func (p EnvironmentPolicy) LogValue() slog.Value {
	return slog.GroupValue(
		slog.Bool("inherit", p.inherit),
		slog.Any("names", p.Names()),
	)
}

// Format keeps environment values out of formatted diagnostics.
func (p EnvironmentPolicy) Format(state fmt.State, _ rune) {
	_, _ = fmt.Fprintf(state, "EnvironmentPolicy{inherit:%t,names:%q}", p.inherit, p.Names())
}

// Resolve returns the exact deterministic process environment for an ACP child.
func (p EnvironmentPolicy) Resolve(base []string) ([]string, error) {
	if err := validateEnvironmentConfig(p.values); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(base)+len(p.values))
	opaque := make([]string, 0)
	if p.inherit {
		for _, entry := range base {
			name, value, ok := strings.Cut(entry, "=")
			if !ok || name == "" {
				opaque = append(opaque, entry)
				continue
			}
			values[name] = value
		}
	}
	for name, value := range p.values {
		values[name] = value
	}

	result := make([]string, 0, len(opaque)+len(values))
	result = append(result, opaque...)
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result, nil
}

func newEnvironmentPolicy(inherit *bool, values map[string]string) EnvironmentPolicy {
	policy := EnvironmentPolicy{inherit: true, values: cloneStringMap(values)}
	if inherit != nil {
		policy.inherit = *inherit
	}
	return policy
}

func cloneStringMap(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	cloned := make(map[string]string, len(values))
	for name, value := range values {
		cloned[name] = value
	}
	return cloned
}

func validateEnvironmentConfig(values map[string]string) error {
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		value := values[name]
		if name == "" {
			return fmt.Errorf("environment variable name must not be empty")
		}
		if strings.ContainsAny(name, "=\x00") {
			return fmt.Errorf("environment variable name %q must not contain '=' or NUL", name)
		}
		if strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment variable %q value must not contain NUL", name)
		}
	}
	return nil
}

func configuredBlockNames(cfg Config) []string {
	blocks := make([]string, 0, 13)
	appendIf := func(name string, present bool) {
		if present {
			blocks = append(blocks, name)
		}
	}
	appendIf(AgentTypeGenericACP, cfg.GenericACP != nil)
	appendIf(AgentTypeGeminiACP, cfg.GeminiACP != nil)
	appendIf(AgentTypeCodexACP, cfg.CodexACP != nil)
	appendIf(AgentTypeOpenCodeACP, cfg.OpenCodeACP != nil)
	appendIf(AgentTypeCopilotACP, cfg.CopilotACP != nil)
	appendIf(AgentTypeClaudeCodeACP, cfg.ClaudeCodeACP != nil)
	appendIf(AgentTypeGrokACP, cfg.GrokACP != nil)
	appendIf(AgentTypeRegistryACP, cfg.RegistryACP != nil)
	appendIf(AgentTypeAgyACP, cfg.AgyACP != nil)
	appendIf(AgentTypeAntigravityACP, cfg.AntigravityACP != nil)
	appendIf(AgentTypeOpenAI, cfg.OpenAI != nil)
	appendIf(AgentTypeAIStudio, cfg.AIStudio != nil)
	appendIf(AgentTypePool, cfg.PoolConfig != nil)
	return blocks
}

func builtinDriverRegistrations() []driverRegistration {
	registrations := []driverRegistration{
		newACPDriver(DriverDescriptor{
			Type: AgentTypeGenericACP, Class: DriverClassACP, ConfigBlock: AgentTypeGenericACP,
		}, func(cfg Config) *ACPConfig { return cfg.GenericACP }, nil),
		newACPDriver(DriverDescriptor{
			Type: AgentTypeCodexACP, Class: DriverClassACP, ConfigBlock: AgentTypeCodexACP,
		}, func(cfg Config) *ACPConfig { return cfg.CodexACP }, func(spec ACPConfig) []string {
			return []string{npxCommand, npxYesFlag, codexACPPackage(spec.BridgeVersion)}
		}),
		newACPDriver(DriverDescriptor{
			Type: AgentTypeOpenCodeACP, Class: DriverClassACP, ConfigBlock: AgentTypeOpenCodeACP,
		}, func(cfg Config) *ACPConfig { return cfg.OpenCodeACP }, func(ACPConfig) []string {
			return []string{"opencode", "acp"}
		}),
		newACPDriver(DriverDescriptor{
			Type: AgentTypeCopilotACP, Class: DriverClassACP, ConfigBlock: AgentTypeCopilotACP,
		}, func(cfg Config) *ACPConfig {
			if cfg.CopilotACP == nil {
				return &ACPConfig{}
			}
			return cfg.CopilotACP
		}, func(ACPConfig) []string { return []string{"copilot", "--acp", "--stdio"} }),
		newACPDriver(DriverDescriptor{
			Type: AgentTypeClaudeCodeACP, Aliases: []string{AgentTypeClaudeACP}, Class: DriverClassACP, ConfigBlock: AgentTypeClaudeCodeACP,
		}, func(cfg Config) *ACPConfig { return cfg.ClaudeCodeACP }, func(ACPConfig) []string {
			return []string{"npx", "-y", "@zed-industries/claude-code-acp@latest"}
		}),
		newACPDriver(DriverDescriptor{
			Type: AgentTypeGrokACP, Class: DriverClassACP, ConfigBlock: AgentTypeGrokACP,
		}, func(cfg Config) *ACPConfig { return cfg.GrokACP }, func(ACPConfig) []string {
			return []string{"grok", "agent", "stdio"}
		}),
		newRegistryACPDriver(),
		newACPDriver(DriverDescriptor{
			Type: AgentTypeAgyACP, Aliases: []string{AgentTypeAntigravityACP}, Class: DriverClassACP, ConfigBlock: AgentTypeAgyACP,
		}, func(cfg Config) *ACPConfig {
			if cfg.AgyACP != nil {
				return cfg.AgyACP
			}
			return cfg.AntigravityACP
		}, func(spec ACPConfig) []string {
			return []string{npxCommand, npxYesFlag, acpRunPackage(spec.BridgeVersion), "run", "antigravity-acp"}
		}),
		newHostedDriver(AgentTypeOpenAI, func(cfg Config) *LocalAPIConfig { return cfg.OpenAI }, validateOpenAI, resolveOpenAI),
		newHostedDriver(AgentTypeAIStudio, func(cfg Config) *LocalAPIConfig { return cfg.AIStudio }, nil, resolveAIStudio),
		newPoolDriver(),
		newGeminiDriver(),
	}
	for i := range registrations {
		applyDescriptorDefaults(&registrations[i].descriptor)
	}
	return registrations
}

func applyDescriptorDefaults(descriptor *DriverDescriptor) {
	if descriptor.ContractVersion == 0 {
		descriptor.ContractVersion = 1
	}
	if descriptor.BackendType == "" {
		descriptor.BackendType = descriptor.Type
	}
	if descriptor.Class == DriverClassACP {
		descriptor.BackendType = AgentTypeGenericACP
		descriptor.SupportsEnvironment = true
		descriptor.ExecutableSource = CapabilitySourceStatic
		descriptor.AuthenticationSource = CapabilitySourceProvider
		descriptor.SessionOptionSource = CapabilitySourceSession
		descriptor.ModelDiscoverySource = CapabilitySourceSession
		descriptor.ConcurrencyPolicy = "provider_default"
	}
	if len(descriptor.ImmutableIdentityFields) == 0 {
		descriptor.ImmutableIdentityFields = []string{"type"}
	}
	if descriptor.ExecutableSource == "" {
		descriptor.ExecutableSource = CapabilitySourceNone
	}
	if descriptor.AuthenticationSource == "" {
		descriptor.AuthenticationSource = CapabilitySourceNone
	}
	if descriptor.SessionOptionSource == "" {
		descriptor.SessionOptionSource = CapabilitySourceNone
	}
	if descriptor.ModelDiscoverySource == "" {
		descriptor.ModelDiscoverySource = CapabilitySourceNone
	}
}

func newACPDriver(descriptor DriverDescriptor, block func(Config) *ACPConfig, defaultCommand func(ACPConfig) []string) driverRegistration {
	return driverRegistration{
		descriptor: descriptor,
		blockPresent: func(cfg Config) bool {
			return block(cfg) != nil
		},
		blockValid: func(cfg Config) bool {
			spec := block(cfg)
			if descriptor.Type == AgentTypeCopilotACP && cfg.CopilotACP == nil {
				return true
			}
			if spec == nil {
				return false
			}
			if descriptor.Type == AgentTypeGenericACP {
				return len(spec.Cmd) > 0
			}
			return true
		},
		blockError: func(cfg Config, rawType string) string {
			spec := block(cfg)
			if descriptor.Type == AgentTypeGenericACP && (spec == nil || len(spec.Cmd) == 0) {
				return fmt.Sprintf("cmd is required for type %s", rawType)
			}
			return ""
		},
		acpBlock: block,
		resolve: func(cfg Config, base ResolvedConfig, _ string) (ResolvedConfig, error) {
			spec := block(cfg)
			if spec == nil {
				if descriptor.Type != AgentTypeCopilotACP {
					return ResolvedConfig{}, fmt.Errorf("%s block is required", descriptor.ConfigBlock)
				}
				spec = &ACPConfig{}
			}
			resolvedSpec := copyACPConfig(*spec)
			if defaultCommand != nil {
				resolvedSpec.Cmd = commandOrDefault(spec.Cmd, defaultCommand(*spec))
			}
			return resolveACPConfig(base, AgentTypeGenericACP, resolvedSpec), nil
		},
	}
}

func newGeminiDriver() driverRegistration {
	driver := newACPDriver(DriverDescriptor{
		Type: AgentTypeGeminiACP, Class: DriverClassACP, ConfigBlock: AgentTypeGeminiACP, Deprecated: true,
	}, func(cfg Config) *ACPConfig { return cfg.GeminiACP }, func(spec ACPConfig) []string {
		return appendGeminiModelFlag([]string{"gemini", "--acp"}, spec.Model)
	})
	driver.blockValid = func(cfg Config) bool { return cfg.GeminiACP != nil && len(cfg.GeminiACP.Cmd) == 0 }
	driver.blockError = func(cfg Config, rawType string) string {
		if cfg.GeminiACP != nil && len(cfg.GeminiACP.Cmd) > 0 {
			return fmt.Sprintf("cmd must be omitted for type %s", rawType)
		}
		return ""
	}
	return driver
}

func newRegistryACPDriver() driverRegistration {
	driver := newACPDriver(DriverDescriptor{
		Type: AgentTypeRegistryACP, Class: DriverClassACP, ConfigBlock: AgentTypeRegistryACP,
		ImmutableIdentityFields: []string{"type", "registry_id"},
	}, func(cfg Config) *ACPConfig { return cfg.RegistryACP }, nil)
	driver.resolve = func(cfg Config, base ResolvedConfig, _ string) (ResolvedConfig, error) {
		if cfg.RegistryACP == nil {
			return ResolvedConfig{}, fmt.Errorf("registry_acp block is required")
		}
		spec := copyACPConfig(*cfg.RegistryACP)
		if strings.TrimSpace(spec.RegistryID) == "" && len(spec.Cmd) == 0 {
			return ResolvedConfig{}, fmt.Errorf("registry_id or cmd is required for type registry_acp")
		}
		if id := strings.TrimSpace(spec.RegistryID); id != "" {
			if adapt, ok := builtinRegistryACPAdapters[id]; ok {
				adapt(&spec)
			}
		}
		command := []string{npxCommand, npxYesFlag, acpRunPackage(spec.BridgeVersion), "run"}
		if id := strings.TrimSpace(spec.RegistryID); id != "" {
			command = append(command, id)
		}
		spec.Cmd = commandOrDefault(spec.Cmd, command)
		return resolveACPConfig(base, AgentTypeGenericACP, spec), nil
	}
	return driver
}

// builtinRegistryACPAdapters is intentionally private and source-defined.
// Registry metadata and YAML cannot install executable adapter code.
var builtinRegistryACPAdapters = map[string]func(*ACPConfig){}

func newHostedDriver(agentType string, block func(Config) *LocalAPIConfig, validate func(Config) error, resolver func(Config, ResolvedConfig) (ResolvedConfig, error)) driverRegistration {
	return driverRegistration{
		descriptor: DriverDescriptor{
			Type: agentType, Class: DriverClassHosted, ConfigBlock: agentType,
			AuthenticationSource: CapabilitySourceStatic, ModelDiscoverySource: CapabilitySourceStatic,
			ConcurrencyPolicy: "provider_default",
		},
		blockPresent: func(cfg Config) bool { return block(cfg) != nil },
		blockValid:   func(cfg Config) bool { return block(cfg) != nil },
		blockError: func(_ Config, rawType string) string {
			return fmt.Sprintf("%s block is required for type %s", agentType, rawType)
		},
		localAPIBlock: block,
		validate:      validate,
		resolve: func(cfg Config, base ResolvedConfig, _ string) (ResolvedConfig, error) {
			return resolver(cfg, base)
		},
	}
}

func validateOpenAI(cfg Config) error {
	if cfg.OpenAI == nil {
		return nil
	}
	if _, err := parseOpenAITimeout(cfg.OpenAI.Timeout); err != nil {
		return err
	}
	switch cfg.OpenAI.ReasoningEffort {
	case "", "none":
		return nil
	default:
		return fmt.Errorf("openai.reasoning_effort currently supports only none: %q", cfg.OpenAI.ReasoningEffort)
	}
}

func newPoolDriver() driverRegistration {
	return driverRegistration{
		descriptor: DriverDescriptor{
			Type: AgentTypePool, Class: DriverClassPool, ConfigBlock: AgentTypePool,
			ConcurrencyPolicy: "orchestrated",
		},
		blockPresent: func(cfg Config) bool { return cfg.PoolConfig != nil },
		blockValid: func(cfg Config) bool {
			return cfg.PoolConfig != nil && len(cfg.PoolConfig.Members) > 0
		},
		blockError: func(cfg Config, _ string) string {
			if cfg.PoolConfig == nil || len(cfg.PoolConfig.Members) == 0 {
				return "pool.members is required for type pool"
			}
			return ""
		},
		resolve: func(cfg Config, base ResolvedConfig, _ string) (ResolvedConfig, error) {
			if cfg.PoolConfig == nil {
				return ResolvedConfig{}, fmt.Errorf("pool block is required")
			}
			base.Type = AgentTypePool
			base.PoolMembers = append([]string(nil), cfg.PoolConfig.Members...)
			return base, nil
		},
	}
}

func resolveOpenAI(cfg Config, base ResolvedConfig) (ResolvedConfig, error) {
	if cfg.OpenAI == nil {
		return ResolvedConfig{}, fmt.Errorf("openai block is required")
	}
	base.Type = AgentTypeOpenAI
	base.APIKey = cfg.OpenAI.APIKey
	base.Model = cfg.OpenAI.Model
	timeout, err := parseOpenAITimeout(cfg.OpenAI.Timeout)
	if err != nil {
		return ResolvedConfig{}, err
	}
	base.Timeout = timeout
	base.ReasoningEffort = cfg.OpenAI.ReasoningEffort
	return base, nil
}

func resolveAIStudio(cfg Config, base ResolvedConfig) (ResolvedConfig, error) {
	if cfg.AIStudio == nil {
		return ResolvedConfig{}, fmt.Errorf("aistudio block is required")
	}
	base.Type = AgentTypeAIStudio
	base.APIKey = cfg.AIStudio.APIKey
	base.Model = cfg.AIStudio.Model
	return base, nil
}

func copyACPConfig(spec ACPConfig) ACPConfig {
	spec.Cmd = append([]string(nil), spec.Cmd...)
	spec.ExtraArgs = append([]string(nil), spec.ExtraArgs...)
	spec.Env = cloneStringMap(spec.Env)
	return spec
}
