package agentfactory

import (
	"context"

	"github.com/baldaworks/runtime/v2/agentproduct"
	acpagent "github.com/normahq/go-adk-acpagent/v2"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

type acpControlSource interface {
	SessionControl() *acpagent.SessionController
}

type acpController interface {
	Inspect(ctx context.Context, sess session.Session) acpagent.SessionControlSnapshot
	ValidateModel(ctx context.Context, sess session.Session, model string) acpagent.ModelValidation
	ChangeModel(ctx context.Context, sess session.Session, model string) acpagent.ModelChange
}

type acpSessionControl struct {
	controller acpController
}

var _ agentproduct.SessionControl = (*acpSessionControl)(nil)

func sessionControlForAgent(ag agent.Agent) agentproduct.SessionControl {
	source, ok := ag.(acpControlSource)
	if !ok {
		return nil
	}
	controller := source.SessionControl()
	if controller == nil {
		return nil
	}
	return &acpSessionControl{controller: controller}
}

func (c *acpSessionControl) Inspect(ctx context.Context, sess session.Session) agentproduct.SessionControlSnapshot {
	return mapACPSnapshot(c.controller.Inspect(ctx, sess))
}

func (c *acpSessionControl) ValidateModel(ctx context.Context, sess session.Session, model string) agentproduct.ModelValidation {
	result := c.controller.ValidateModel(ctx, sess, model)
	mapped := agentproduct.ModelValidation{Reason: mapACPReason(result.Code)}
	switch result.Outcome {
	case acpagent.ModelValidationSupported:
		mapped.Status = agentproduct.ModelSupported
		mapped.Model = model
		mapped.Reason = agentproduct.ReasonNone
	case acpagent.ModelValidationUnsupported:
		mapped.Status = agentproduct.ModelUnsupported
		mapped.Reason = agentproduct.ReasonUnavailable
	case acpagent.ModelValidationIncompatible:
		mapped.Status = agentproduct.ModelIncompatible
		mapped.Reason = agentproduct.ReasonInvalidModel
	case acpagent.ModelValidationBusy:
		mapped.Status = agentproduct.ModelBusy
		mapped.Reason = agentproduct.ReasonActiveWork
	case acpagent.ModelValidationClosed:
		mapped.Status = agentproduct.ModelClosed
		mapped.Reason = agentproduct.ReasonClosed
	default:
		mapped.Status = agentproduct.ModelFailed
	}
	return mapped
}

func (c *acpSessionControl) ChangeModel(ctx context.Context, sess session.Session, model string) agentproduct.ModelChange {
	result := c.controller.ChangeModel(ctx, sess, model)
	mapped := agentproduct.ModelChange{Reason: mapACPReason(result.Code)}
	switch result.Outcome {
	case acpagent.ModelChangeApplied:
		mapped.Status = agentproduct.ModelApplied
		mapped.EffectiveModel = result.EffectiveModel
		mapped.Reason = agentproduct.ReasonNone
	case acpagent.ModelChangeUnsupported:
		mapped.Status = agentproduct.ModelChangeUnsupported
		mapped.Reason = agentproduct.ReasonUnavailable
	case acpagent.ModelChangeIncompatible:
		mapped.Status = agentproduct.ModelChangeIncompatible
		mapped.Reason = agentproduct.ReasonInvalidModel
	case acpagent.ModelChangeRejected:
		mapped.Status = agentproduct.ModelRejected
		mapped.Reason = agentproduct.ReasonProviderReject
	case acpagent.ModelChangeBusy:
		mapped.Status = agentproduct.ModelChangeBusy
		mapped.Reason = agentproduct.ReasonActiveWork
	case acpagent.ModelChangeAmbiguous:
		mapped.Status = agentproduct.ModelAmbiguous
		mapped.Reason = agentproduct.ReasonUncertain
	case acpagent.ModelChangeClosed:
		mapped.Status = agentproduct.ModelChangeClosed
		mapped.Reason = agentproduct.ReasonClosed
	default:
		mapped.Status = agentproduct.ModelChangeFailed
	}
	return mapped
}

func (*acpSessionControl) FastForward(context.Context, session.Session, agentproduct.ContextPackage) agentproduct.ContextChange {
	return agentproduct.UnsupportedFastForward()
}

func mapACPSnapshot(snapshot acpagent.SessionControlSnapshot) agentproduct.SessionControlSnapshot {
	mapped := agentproduct.SessionControlSnapshot{Reason: mapACPReason(snapshot.Code)}
	switch snapshot.Status {
	case acpagent.SessionControlAvailable:
		mapped.Status = agentproduct.ControlReady
		mapped.CurrentModel = snapshot.CurrentModel
		mapped.Models = make([]agentproduct.ModelDescriptor, len(snapshot.Models))
		for i, model := range snapshot.Models {
			mapped.Models[i] = agentproduct.ModelDescriptor{ID: model.ID, Label: model.Label}
		}
		mapped.Reason = agentproduct.ReasonNone
	case acpagent.SessionControlUnavailable:
		mapped.Status = agentproduct.ControlUnsupported
		mapped.Reason = agentproduct.ReasonUnavailable
	case acpagent.SessionControlBusy:
		mapped.Status = agentproduct.ControlBusy
		mapped.Reason = agentproduct.ReasonActiveWork
	case acpagent.SessionControlClosed:
		mapped.Status = agentproduct.ControlClosed
		mapped.Reason = agentproduct.ReasonClosed
	default:
		mapped.Status = agentproduct.ControlFailed
	}
	return mapped
}

func mapACPReason(code string) agentproduct.Reason {
	switch code {
	case "":
		return agentproduct.ReasonNone
	case "invalid_session":
		return agentproduct.ReasonInvalidSession
	case "invalid_state", "stale_snapshot":
		return agentproduct.ReasonBindingMismatch
	case "unknown_model":
		return agentproduct.ReasonInvalidModel
	case "busy":
		return agentproduct.ReasonActiveWork
	case "request_rejected", "not_applied":
		return agentproduct.ReasonProviderReject
	case "response_uncertain", "invalid_response", "state_write_failed":
		return agentproduct.ReasonUncertain
	case "closed":
		return agentproduct.ReasonClosed
	case "attachment_unavailable", "resume_unsupported", "unknown_session", "model_control_unavailable", "method_unsupported":
		return agentproduct.ReasonUnavailable
	default:
		return agentproduct.ReasonPreDispatch
	}
}
