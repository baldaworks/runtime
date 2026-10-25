package agentproduct

import (
	"context"
	"errors"
	"unicode"
	"unicode/utf8"

	"google.golang.org/adk/v2/session"
)

// ErrInvalidContextPackage reports non-portable or excessive context input.
var ErrInvalidContextPackage = errors.New("invalid portable context package")

const (
	// MaxModels is the largest model list accepted from a control adapter.
	MaxModels = 128
	// MaxModelIDBytes bounds public model identifiers.
	MaxModelIDBytes = 256
	// MaxModelLabelBytes bounds public model labels.
	MaxModelLabelBytes = 512
	// MaxContextMessages bounds one portable context package.
	MaxContextMessages = 256
	// MaxContextMessageBytes bounds one portable context message.
	MaxContextMessageBytes = 32 << 10
	// MaxContextBytes bounds all text in one portable context package.
	MaxContextBytes = 256 << 10
)

// SessionControl is the single optional control-plane port for a Product.
// Presence does not imply support for a particular session or operation;
// Inspect is authoritative for the exact supplied session.
type SessionControl interface {
	Inspect(ctx context.Context, sess session.Session) SessionControlSnapshot
	ValidateModel(ctx context.Context, sess session.Session, model string) ModelValidation
	ChangeModel(ctx context.Context, sess session.Session, model string) ModelChange
	FastForward(ctx context.Context, sess session.Session, context ContextPackage) ContextChange
}

// ControlStatus describes whether exact-session control is available.
type ControlStatus string

const (
	// ControlReady means authoritative exact-session control is available.
	ControlReady ControlStatus = "ready"
	// ControlUnsupported means the exact session has no control support.
	ControlUnsupported ControlStatus = "unsupported"
	// ControlBusy means active provider work prevents safe inspection.
	ControlBusy ControlStatus = "busy"
	// ControlFailed means inspection failed without external mutation.
	ControlFailed ControlStatus = "failed"
	// ControlClosed means the owning Product is closed.
	ControlClosed ControlStatus = "closed"
)

// Reason is a bounded, provider-neutral diagnostic classification.
type Reason string

const (
	// ReasonNone means no additional diagnostic classification applies.
	ReasonNone Reason = ""
	// ReasonUnavailable classifies an absent provider operation.
	ReasonUnavailable Reason = "unavailable"
	// ReasonInvalidSession classifies an invalid session reference.
	ReasonInvalidSession Reason = "invalid_session"
	// ReasonBindingMismatch classifies a different product or native binding.
	ReasonBindingMismatch Reason = "binding_mismatch"
	// ReasonInvalidModel classifies an invalid public model value.
	ReasonInvalidModel Reason = "invalid_model"
	// ReasonInvalidContext classifies non-portable context input.
	ReasonInvalidContext Reason = "invalid_context"
	// ReasonActiveWork classifies a conflicting provider operation.
	ReasonActiveWork Reason = "active_work"
	// ReasonPreDispatch classifies a proven failure before provider dispatch.
	ReasonPreDispatch Reason = "pre_dispatch"
	// ReasonProviderReject classifies an authoritative provider rejection.
	ReasonProviderReject Reason = "provider_rejected"
	// ReasonUncertain classifies a result that requires reconciliation.
	ReasonUncertain Reason = "uncertain"
	// ReasonInvalidResult classifies unsafe output from a control adapter.
	ReasonInvalidResult Reason = "invalid_result"
	// ReasonClosed classifies an operation rejected by product lifecycle.
	ReasonClosed Reason = "closed"
)

// ModelDescriptor is bounded public model metadata.
type ModelDescriptor struct {
	ID    string
	Label string
}

// SessionControlSnapshot describes control support for one exact session.
type SessionControlSnapshot struct {
	Status             ControlStatus
	Models             []ModelDescriptor
	CurrentModel       string
	FastForwardSupport bool
	Reason             Reason
}

// ModelValidationStatus is a non-mutating validation outcome.
type ModelValidationStatus string

const (
	// ModelSupported means validation proved that the model can be requested.
	ModelSupported ModelValidationStatus = "supported"
	// ModelUnsupported means the exact session has no model control.
	ModelUnsupported ModelValidationStatus = "unsupported"
	// ModelIncompatible means the value cannot be used by this exact session.
	ModelIncompatible ModelValidationStatus = "incompatible"
	// ModelBusy means active provider work prevents safe validation.
	ModelBusy ModelValidationStatus = "busy"
	// ModelFailed means local validation failed without external mutation.
	ModelFailed ModelValidationStatus = "failed"
	// ModelClosed means the owning Product is closed.
	ModelClosed ModelValidationStatus = "closed"
)

// ModelValidation reports whether a model change can be attempted. Validation
// must not mutate provider state or construct a replacement Product.
type ModelValidation struct {
	Status ModelValidationStatus
	Model  string
	Reason Reason
}

// ModelChangeStatus is the terminal or uncertain result of a model change.
type ModelChangeStatus string

const (
	// ModelApplied means the existing provider session changed conclusively.
	ModelApplied ModelChangeStatus = "applied"
	// ModelReplacement returns an independently owned candidate Product.
	ModelReplacement ModelChangeStatus = "replacement"
	// ModelChangeUnsupported means the exact session has no model control.
	ModelChangeUnsupported ModelChangeStatus = "unsupported"
	// ModelChangeIncompatible means the requested value cannot be used safely.
	ModelChangeIncompatible ModelChangeStatus = "incompatible"
	// ModelRejected means the provider authoritatively rejected the request.
	ModelRejected ModelChangeStatus = "rejected"
	// ModelChangeBusy means active provider work prevents safe mutation.
	ModelChangeBusy ModelChangeStatus = "busy"
	// ModelChangeFailed means failure was proven before provider dispatch.
	ModelChangeFailed ModelChangeStatus = "failed"
	// ModelAmbiguous means mutation may have occurred and must be reconciled.
	ModelAmbiguous ModelChangeStatus = "ambiguous"
	// ModelChangeClosed means the owning Product is closed.
	ModelChangeClosed ModelChangeStatus = "closed"
)

// ModelChange reports a model mutation. Replacement is populated only when
// Status is ModelReplacement; its caller becomes the candidate owner.
type ModelChange struct {
	Status         ModelChangeStatus
	EffectiveModel string
	Replacement    *Product
	Reason         Reason
}

// ContextChangeStatus is the result of a portable context import.
type ContextChangeStatus string

const (
	// ContextReady means an import is prepared but is not yet committed.
	ContextReady ContextChangeStatus = "ready"
	// ContextSucceeded means the provider conclusively imported the package.
	ContextSucceeded ContextChangeStatus = "succeeded"
	// ContextUnsupported means no non-turn context import exists.
	ContextUnsupported ContextChangeStatus = "unsupported"
	// ContextIncompatible means the package cannot be imported safely.
	ContextIncompatible ContextChangeStatus = "incompatible"
	// ContextBusy means active provider work prevents safe import.
	ContextBusy ContextChangeStatus = "busy"
	// ContextFailed means failure was proven before provider dispatch.
	ContextFailed ContextChangeStatus = "failed"
	// ContextAmbiguous means import may have occurred and needs reconciliation.
	ContextAmbiguous ContextChangeStatus = "ambiguous"
	// ContextClosed means the owning Product is closed.
	ContextClosed ContextChangeStatus = "closed"
)

// ContextChange reports a provider-native import without an assistant turn.
type ContextChange struct {
	Status ContextChangeStatus
	Reason Reason
}

// ContextRole is an allowlisted portable visible-message role.
type ContextRole string

const (
	// ContextRoleUser identifies visible user-authored text.
	ContextRoleUser ContextRole = "user"
	// ContextRoleAssistant identifies visible assistant-authored text.
	ContextRoleAssistant ContextRole = "assistant"
)

// ContextMessage is one visible user or assistant message.
type ContextMessage struct {
	Role ContextRole
	Text string
}

// ContextPackage contains only validated visible text. It cannot represent
// system prompts, tool traffic, attachments, or provider-private state.
type ContextPackage struct {
	messages []ContextMessage
	bytes    int
}

// NewContextPackage validates and copies portable visible context.
func NewContextPackage(messages []ContextMessage) (ContextPackage, error) {
	if len(messages) == 0 || len(messages) > MaxContextMessages {
		return ContextPackage{}, ErrInvalidContextPackage
	}
	cloned := make([]ContextMessage, len(messages))
	total := 0
	for i, message := range messages {
		if message.Role != ContextRoleUser && message.Role != ContextRoleAssistant {
			return ContextPackage{}, ErrInvalidContextPackage
		}
		if !visibleText(message.Text) || len(message.Text) > MaxContextMessageBytes {
			return ContextPackage{}, ErrInvalidContextPackage
		}
		total += len(message.Text)
		if total > MaxContextBytes {
			return ContextPackage{}, ErrInvalidContextPackage
		}
		cloned[i] = message
	}
	return ContextPackage{messages: cloned, bytes: total}, nil
}

// Messages returns a copy of the portable messages.
func (p ContextPackage) Messages() []ContextMessage {
	return append([]ContextMessage(nil), p.messages...)
}

// ByteLen returns the total UTF-8 byte length of message text.
func (p ContextPackage) ByteLen() int { return p.bytes }

// Valid reports whether the package was constructed with NewContextPackage.
func (p ContextPackage) Valid() bool {
	if len(p.messages) == 0 || len(p.messages) > MaxContextMessages || p.bytes <= 0 || p.bytes > MaxContextBytes {
		return false
	}
	total := 0
	for _, message := range p.messages {
		if message.Role != ContextRoleUser && message.Role != ContextRoleAssistant ||
			!visibleText(message.Text) || len(message.Text) > MaxContextMessageBytes {
			return false
		}
		total += len(message.Text)
	}
	return total == p.bytes
}

func visibleText(text string) bool {
	if text == "" || !utf8.ValidString(text) {
		return false
	}
	for _, r := range text {
		if unicode.IsControl(r) && r != '\n' && r != '\r' && r != '\t' {
			return false
		}
	}
	return true
}

// UnsupportedFastForward returns the default safe result for providers that
// have no non-turn context import operation.
func UnsupportedFastForward() ContextChange {
	return ContextChange{Status: ContextUnsupported, Reason: ReasonUnavailable}
}
