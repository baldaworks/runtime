package appconfig

import (
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/go-playground/validator/v10"
	"github.com/go-viper/mapstructure/v2"
)

// RuntimeConfig contains runtime agent-factory settings.
//
// Config shape:
//
//	runtime:
//	  providers: ...
//	  mcp_servers: ...
type RuntimeConfig struct {
	// Providers contains named runtime provider definitions.
	Providers map[string]agentconfig.Config `json:"providers,omitempty"   mapstructure:"providers"   validate:"required,gt=0"`
	// MCPServers contains named MCP server definitions shared by providers.
	MCPServers map[string]agentconfig.MCPServerConfig `json:"mcp_servers,omitempty" mapstructure:"mcp_servers" validate:"omitempty"`
}

// Validate validates the runtime config.
func (c RuntimeConfig) Validate() error {
	errList := make([]string, 0)

	if err := runtimeConfigValidator.Struct(c); err != nil {
		if invErr, ok := err.(*validator.InvalidValidationError); ok {
			return fmt.Errorf("validate runtime config: %w", invErr)
		}
		for _, validationErr := range err.(validator.ValidationErrors) {
			errList = append(errList, formatValidationError(validationErr))
		}
	}

	for name, providerCfg := range c.Providers {
		if err := providerCfg.Validate(); err != nil {
			errList = append(errList, fmt.Sprintf("agent %q: %v", name, err))
		}
	}
	for name, mcpCfg := range c.MCPServers {
		if err := agentconfig.ValidateMCPServerConfig(mcpCfg); err != nil {
			errList = append(errList, fmt.Sprintf("mcp %q: %v", name, err))
		}
	}

	if len(errList) == 0 {
		return nil
	}
	sort.Strings(errList)
	return fmt.Errorf("runtime config validation failed: %s", strings.Join(errList, "; "))
}

// ValidateSettings decodes and validates raw "runtime" settings.
func ValidateSettings(settings map[string]any) error {
	if err := detectLegacyNormaKey(settings); err != nil {
		return err
	}

	var cfg RuntimeConfig
	decoder, err := mapstructure.NewDecoder(&mapstructure.DecoderConfig{
		Metadata: nil,
		Result:   &cfg,
		TagName:  "mapstructure",
	})
	if err != nil {
		return fmt.Errorf("failed to create decoder: %w", err)
	}
	if err := decoder.Decode(settings); err != nil {
		return fmt.Errorf("failed to decode settings: %w", err)
	}
	return cfg.Validate()
}

func detectLegacyNormaKey(settings map[string]any) error {
	if _, hasLegacyNorma := settings["norma"]; hasLegacyNorma {
		return fmt.Errorf("legacy root key 'norma' detected; this package expects the 'runtime' section contents")
	}

	return nil
}

var runtimeConfigValidator = newRuntimeConfigValidator()

func newRuntimeConfigValidator() *validator.Validate {
	v := validator.New()
	v.RegisterTagNameFunc(func(fld reflect.StructField) string {
		name := strings.SplitN(fld.Tag.Get("mapstructure"), ",", 2)[0]
		if name == "" || name == "-" {
			return fld.Name
		}
		return name
	})
	return v
}

func formatValidationError(err validator.FieldError) string {
	field := err.Field()
	switch err.Tag() {
	case "required":
		return field + " is required"
	case "gt":
		return field + " must be greater than " + err.Param()
	case "min":
		return field + " must be at least " + err.Param()
	default:
		return field + " failed validation rule " + err.Tag()
	}
}
