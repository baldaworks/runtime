package agentproduct

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

// ErrCloseFailed reports that an owned resource failed to close. Product does
// not expose the underlying provider error because it may contain protected
// configuration or remote details.
var ErrCloseFailed = errors.New("agent product close failed")

// Product owns one immutable executable Agent and its optional session control.
// Agent and Control are fixed at construction time by convention. Close is the
// sole ownership entrypoint; callers must not close Agent directly.
type Product struct {
	Agent   agent.Agent
	Control SessionControl

	admission  *controlAdmission
	closeAgent func() error
	closeOnce  sync.Once
	closeErr   error
}

// New constructs a Product and takes ownership of ag. When ag implements
// io.Closer, Product.Close closes it after control admission is fenced.
func New(ag agent.Agent, control SessionControl) (*Product, error) {
	if ag == nil {
		return nil, fmt.Errorf("agentproduct: agent is required")
	}
	p := &Product{Agent: ag}
	if control != nil {
		p.admission = &controlAdmission{next: control, owner: p}
		p.Control = p.admission
	}
	if closer, ok := ag.(io.Closer); ok {
		p.closeAgent = closer.Close
	}
	return p, nil
}

// Close fences new control calls, waits for admitted calls, and then releases
// Agent-owned resources exactly once. Cleanup failures return ErrCloseFailed;
// provider error details are intentionally not exposed.
func (p *Product) Close() error {
	if p == nil {
		return nil
	}
	p.closeOnce.Do(func() {
		if p.admission != nil {
			p.admission.close()
		}
		if p.closeAgent != nil {
			if err := p.closeAgent(); err != nil {
				p.closeErr = ErrCloseFailed
			}
		}
	})
	return p.closeErr
}

type controlAdmission struct {
	mu     sync.RWMutex
	next   SessionControl
	owner  *Product
	closed bool
}

func (c *controlAdmission) Inspect(ctx context.Context, sess session.Session) SessionControlSnapshot {
	if !c.acquire() {
		return SessionControlSnapshot{Status: ControlClosed, Reason: ReasonClosed}
	}
	defer c.mu.RUnlock()
	return normalizeSnapshot(c.next.Inspect(ctx, sess))
}

func (c *controlAdmission) ValidateModel(ctx context.Context, sess session.Session, model string) ModelValidation {
	if !c.acquire() {
		return ModelValidation{Status: ModelClosed, Reason: ReasonClosed}
	}
	defer c.mu.RUnlock()
	if !validModelID(model) {
		return ModelValidation{Status: ModelIncompatible, Reason: ReasonInvalidModel}
	}
	return normalizeValidation(c.next.ValidateModel(ctx, sess, model), model)
}

func (c *controlAdmission) ChangeModel(ctx context.Context, sess session.Session, model string) ModelChange {
	if !c.acquire() {
		return ModelChange{Status: ModelChangeClosed, Reason: ReasonClosed}
	}
	defer c.mu.RUnlock()
	if !validModelID(model) {
		return ModelChange{Status: ModelChangeIncompatible, Reason: ReasonInvalidModel}
	}
	return normalizeModelChange(c.next.ChangeModel(ctx, sess, model), model, c.owner)
}

func (c *controlAdmission) FastForward(ctx context.Context, sess session.Session, pkg ContextPackage) ContextChange {
	if !c.acquire() {
		return ContextChange{Status: ContextClosed, Reason: ReasonClosed}
	}
	defer c.mu.RUnlock()
	if !pkg.Valid() {
		return ContextChange{Status: ContextIncompatible, Reason: ReasonInvalidContext}
	}
	return normalizeContextChange(c.next.FastForward(ctx, sess, pkg))
}

func (c *controlAdmission) acquire() bool {
	c.mu.RLock()
	if c.closed {
		c.mu.RUnlock()
		return false
	}
	return true
}

func (c *controlAdmission) close() {
	c.mu.Lock()
	c.closed = true
	c.mu.Unlock()
}

func normalizeSnapshot(got SessionControlSnapshot) SessionControlSnapshot {
	if !validControlStatus(got.Status) || !validReason(got.Reason) || len(got.Models) > MaxModels ||
		!validOptionalModelID(got.CurrentModel) || !validOptionalControlRevision(got.ControlRevision) ||
		got.Status == ControlReady && got.Reason != ReasonNone ||
		got.Status != ControlReady && (len(got.Models) != 0 || got.CurrentModel != "" ||
			got.ControlRevision != "" || got.FastForwardSupport) {
		return SessionControlSnapshot{Status: ControlFailed, Reason: ReasonInvalidResult}
	}
	models := make([]ModelDescriptor, len(got.Models))
	ids := make(map[string]struct{}, len(got.Models))
	for i, model := range got.Models {
		if !validModelID(model.ID) || !validOptionalLabel(model.Label) {
			return SessionControlSnapshot{Status: ControlFailed, Reason: ReasonInvalidResult}
		}
		if _, duplicate := ids[model.ID]; duplicate {
			return SessionControlSnapshot{Status: ControlFailed, Reason: ReasonInvalidResult}
		}
		ids[model.ID] = struct{}{}
		models[i] = model
	}
	if got.CurrentModel != "" {
		if _, advertised := ids[got.CurrentModel]; !advertised {
			return SessionControlSnapshot{Status: ControlFailed, Reason: ReasonInvalidResult}
		}
	}
	got.Models = models
	return got
}

func normalizeValidation(got ModelValidation, requested string) ModelValidation {
	if !validValidationStatus(got.Status) || !validReason(got.Reason) || !validOptionalModelID(got.Model) ||
		got.Status == ModelSupported && (got.Model != requested || got.Reason != ReasonNone) ||
		got.Status != ModelSupported && got.Model != "" {
		return ModelValidation{Status: ModelFailed, Reason: ReasonInvalidResult}
	}
	return got
}

func normalizeModelChange(got ModelChange, requested string, owner *Product) ModelChange {
	if !validModelChangeStatus(got.Status) || !validReason(got.Reason) || !validOptionalModelID(got.EffectiveModel) ||
		(got.Status == ModelReplacement) != (got.Replacement != nil) || got.Replacement == owner ||
		(got.Replacement != nil && got.Replacement.Agent == nil) ||
		(got.Status == ModelApplied || got.Status == ModelReplacement) && (got.EffectiveModel != requested || got.Reason != ReasonNone) ||
		got.Status != ModelApplied && got.Status != ModelReplacement && got.EffectiveModel != "" {
		if got.Replacement != nil && got.Replacement != owner {
			_ = got.Replacement.Close()
		}
		return ModelChange{Status: ModelChangeFailed, Reason: ReasonInvalidResult}
	}
	return got
}

func normalizeContextChange(got ContextChange) ContextChange {
	if !validContextChangeStatus(got.Status) || !validReason(got.Reason) ||
		(got.Status == ContextReady || got.Status == ContextSucceeded) && got.Reason != ReasonNone {
		return ContextChange{Status: ContextFailed, Reason: ReasonInvalidResult}
	}
	return got
}

func validModelID(value string) bool {
	return structuralText(value, MaxModelIDBytes)
}

func validOptionalModelID(value string) bool { return value == "" || validModelID(value) }

func validOptionalControlRevision(value string) bool {
	return value == "" || structuralText(value, MaxControlRevisionBytes)
}

func validOptionalLabel(value string) bool {
	return value == "" || structuralText(value, MaxModelLabelBytes)
}

func structuralText(value string, maxBytes int) bool {
	if value == "" || len(value) > maxBytes || strings.TrimSpace(value) != value || !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func validControlStatus(value ControlStatus) bool {
	switch value {
	case ControlReady, ControlUnsupported, ControlBusy, ControlFailed, ControlClosed:
		return true
	default:
		return false
	}
}

func validValidationStatus(value ModelValidationStatus) bool {
	switch value {
	case ModelSupported, ModelUnsupported, ModelIncompatible, ModelBusy, ModelFailed, ModelClosed:
		return true
	default:
		return false
	}
}

func validModelChangeStatus(value ModelChangeStatus) bool {
	switch value {
	case ModelApplied, ModelReplacement, ModelChangeUnsupported, ModelChangeIncompatible, ModelRejected,
		ModelChangeBusy, ModelChangeFailed, ModelAmbiguous, ModelChangeClosed:
		return true
	default:
		return false
	}
}

func validContextChangeStatus(value ContextChangeStatus) bool {
	switch value {
	case ContextReady, ContextSucceeded, ContextUnsupported, ContextIncompatible, ContextBusy, ContextFailed,
		ContextAmbiguous, ContextClosed:
		return true
	default:
		return false
	}
}

func validReason(value Reason) bool {
	switch value {
	case ReasonNone, ReasonUnavailable, ReasonInvalidSession, ReasonBindingMismatch, ReasonInvalidModel,
		ReasonInvalidContext, ReasonActiveWork, ReasonPreDispatch, ReasonProviderReject, ReasonUncertain,
		ReasonInvalidResult, ReasonClosed:
		return true
	default:
		return false
	}
}
