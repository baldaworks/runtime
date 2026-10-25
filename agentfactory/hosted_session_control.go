package agentfactory

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/baldaworks/runtime/v2/agentconfig"
	"github.com/baldaworks/runtime/v2/agentproduct"
	"github.com/baldaworks/runtime/v2/hostedagent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"
)

type hostedRecipe struct {
	providerType               string
	configuredType             string
	driverType                 string
	systemInstructions         string
	apiKey                     string
	model                      string
	timeout                    time.Duration
	reasoningEffort            string
	request                    BuildRequest
	resolvedMCP                map[string]agentconfig.MCPServerConfig
	create                     constructor
	availabilityIndependentMCP bool
}

type hostedSessionControl struct {
	recipe hostedRecipe
}

var _ agentproduct.SessionControl = (*hostedSessionControl)(nil)

func (c *hostedSessionControl) Format(state fmt.State, _ rune) {
	_, _ = io.WriteString(state, "hostedSessionControl{provider_type:"+c.recipe.providerType+"}")
}

func (c *hostedSessionControl) LogValue() slog.Value {
	return slog.GroupValue(slog.String("provider_type", c.recipe.providerType))
}

func hostedControlForConstruction(
	cfg agentconfig.ResolvedConfig,
	req BuildRequest,
	resolvedMCP map[string]agentconfig.MCPServerConfig,
	create constructor,
	availabilityIndependentMCP bool,
) agentproduct.SessionControl {
	if cfg.Type != agentconfig.AgentTypeOpenAI && cfg.Type != agentconfig.AgentTypeAIStudio {
		return nil
	}
	return &hostedSessionControl{recipe: hostedRecipe{
		providerType:               cfg.Type,
		configuredType:             cfg.ConfiguredType,
		driverType:                 cfg.DriverType,
		systemInstructions:         cfg.SystemInstructions,
		apiKey:                     cfg.APIKey,
		model:                      cfg.Model,
		timeout:                    cfg.Timeout,
		reasoningEffort:            cfg.ReasoningEffort,
		request:                    cloneBuildRequest(req),
		resolvedMCP:                cloneMCPServerConfigs(resolvedMCP),
		create:                     create,
		availabilityIndependentMCP: availabilityIndependentMCP,
	}}
}

func (c *hostedSessionControl) Inspect(_ context.Context, sess session.Session) agentproduct.SessionControlSnapshot {
	if sess == nil {
		return agentproduct.SessionControlSnapshot{Status: agentproduct.ControlFailed, Reason: agentproduct.ReasonInvalidSession}
	}
	return agentproduct.SessionControlSnapshot{
		Status:       agentproduct.ControlReady,
		CurrentModel: c.recipe.model,
		Models: []agentproduct.ModelDescriptor{{
			ID:    c.recipe.model,
			Label: c.recipe.model,
		}},
	}
}

func (c *hostedSessionControl) ValidateModel(_ context.Context, sess session.Session, model string) agentproduct.ModelValidation {
	if sess == nil {
		return agentproduct.ModelValidation{Status: agentproduct.ModelFailed, Reason: agentproduct.ReasonInvalidSession}
	}
	if err := c.recipe.validateModel(model); err != nil {
		return agentproduct.ModelValidation{Status: agentproduct.ModelIncompatible, Reason: agentproduct.ReasonInvalidModel}
	}
	return agentproduct.ModelValidation{Status: agentproduct.ModelSupported, Model: model}
}

func (c *hostedSessionControl) ChangeModel(ctx context.Context, sess session.Session, model string) agentproduct.ModelChange {
	validation := c.ValidateModel(ctx, sess, model)
	if validation.Status != agentproduct.ModelSupported {
		return agentproduct.ModelChange{Status: modelChangeStatusFromValidation(validation.Status), Reason: validation.Reason}
	}
	candidate, ok := c.recipe.buildCandidate(ctx, model)
	if !ok {
		return agentproduct.ModelChange{Status: agentproduct.ModelChangeFailed, Reason: agentproduct.ReasonPreDispatch}
	}
	return agentproduct.ModelChange{
		Status:         agentproduct.ModelReplacement,
		EffectiveModel: model,
		Replacement:    candidate,
	}
}

func (*hostedSessionControl) FastForward(context.Context, session.Session, agentproduct.ContextPackage) agentproduct.ContextChange {
	return agentproduct.UnsupportedFastForward()
}

func (r hostedRecipe) validateModel(model string) error {
	switch r.providerType {
	case agentconfig.AgentTypeOpenAI:
		return hostedagent.ValidateOpenAIModelOptions(r.apiKey, model, hostedagent.OpenAIModelOptions{
			Timeout: r.timeout, ReasoningEffort: r.reasoningEffort,
		})
	case agentconfig.AgentTypeAIStudio:
		return hostedagent.ValidateAIStudioModel(model)
	default:
		return errors.New("unsupported hosted provider")
	}
}

func (r hostedRecipe) buildCandidate(ctx context.Context, model string) (*agentproduct.Product, bool) {
	if ctx == nil || ctx.Err() != nil {
		return nil, false
	}
	cfg := agentconfig.ResolvedConfig{
		ConfiguredType:     r.configuredType,
		DriverType:         r.driverType,
		Type:               r.providerType,
		SystemInstructions: r.systemInstructions,
		APIKey:             r.apiKey,
		Model:              model,
		Timeout:            r.timeout,
		ReasoningEffort:    r.reasoningEffort,
	}
	req := cloneBuildRequest(r.request)
	resolvedMCP := cloneMCPServerConfigs(r.resolvedMCP)
	factory := &Factory{availabilityIndependentMCP: r.availabilityIndependentMCP}
	ag, err := r.create(ctx, cfg, req, factory, resolvedMCP)
	if err != nil || ag == nil {
		if closer, ok := ag.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, false
	}
	if ctx.Err() != nil {
		if closer, ok := ag.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, false
	}
	next := r
	next.model = model
	next.request = req
	next.resolvedMCP = resolvedMCP
	product, err := agentproduct.New(ag, &hostedSessionControl{recipe: next})
	if err != nil {
		if closer, ok := ag.(io.Closer); ok {
			_ = closer.Close()
		}
		return nil, false
	}
	return product, true
}

func modelChangeStatusFromValidation(status agentproduct.ModelValidationStatus) agentproduct.ModelChangeStatus {
	switch status {
	case agentproduct.ModelUnsupported:
		return agentproduct.ModelChangeUnsupported
	case agentproduct.ModelIncompatible:
		return agentproduct.ModelChangeIncompatible
	case agentproduct.ModelBusy:
		return agentproduct.ModelChangeBusy
	case agentproduct.ModelClosed:
		return agentproduct.ModelChangeClosed
	default:
		return agentproduct.ModelChangeFailed
	}
}

func cloneBuildRequest(req BuildRequest) BuildRequest {
	req.MCPServerIDs = append([]string(nil), req.MCPServerIDs...)
	req.Tools = append([]tool.Tool(nil), req.Tools...)
	req.Toolsets = append([]tool.Toolset(nil), req.Toolsets...)
	return req
}

func cloneMCPServerConfigs(configs map[string]agentconfig.MCPServerConfig) map[string]agentconfig.MCPServerConfig {
	if len(configs) == 0 {
		return nil
	}
	cloned := make(map[string]agentconfig.MCPServerConfig, len(configs))
	for id, cfg := range configs {
		cloned[id] = agentconfig.MCPServerConfig{
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
	}
	return cloned
}
