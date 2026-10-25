package agentproduct

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
)

type testAgent struct {
	agent.Agent
	closes atomic.Int32
	err    error
}

func (a *testAgent) Close() error {
	a.closes.Add(1)
	return a.err
}

type testControl struct {
	inspect       func() SessionControlSnapshot
	validate      func() ModelValidation
	change        func() ModelChange
	forward       func() ContextChange
	validateCalls atomic.Int32
	changeCalls   atomic.Int32
	forwardCalls  atomic.Int32
	entered       chan struct{}
	release       chan struct{}
}

func (c *testControl) Inspect(context.Context, session.Session) SessionControlSnapshot {
	if c.entered != nil {
		close(c.entered)
		<-c.release
	}
	if c.inspect != nil {
		return c.inspect()
	}
	return SessionControlSnapshot{Status: ControlReady}
}

func (c *testControl) ValidateModel(context.Context, session.Session, string) ModelValidation {
	c.validateCalls.Add(1)
	if c.validate != nil {
		return c.validate()
	}
	return ModelValidation{Status: ModelSupported, Model: "safe-model"}
}

func (c *testControl) ChangeModel(context.Context, session.Session, string) ModelChange {
	c.changeCalls.Add(1)
	if c.change != nil {
		return c.change()
	}
	return ModelChange{Status: ModelApplied, EffectiveModel: "safe-model"}
}

func (c *testControl) FastForward(context.Context, session.Session, ContextPackage) ContextChange {
	c.forwardCalls.Add(1)
	if c.forward != nil {
		return c.forward()
	}
	return UnsupportedFastForward()
}

func newTestAgent(t *testing.T) *testAgent {
	t.Helper()
	base, err := agent.New(agent.Config{Name: "test"})
	if err != nil {
		t.Fatalf("agent.New: %v", err)
	}
	return &testAgent{Agent: base}
}

func TestProductCloseFencesControlBeforeAgent(t *testing.T) {
	ag := newTestAgent(t)
	control := &testControl{entered: make(chan struct{}), release: make(chan struct{})}
	product, err := New(ag, control)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	inspected := make(chan struct{})
	go func() {
		product.Control.Inspect(context.Background(), nil)
		close(inspected)
	}()
	<-control.entered

	closed := make(chan error, 1)
	go func() { closed <- product.Close() }()
	if got := ag.closes.Load(); got != 0 {
		t.Fatalf("agent closed while control call is active: %d", got)
	}
	close(control.release)
	<-inspected
	if err := <-closed; err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := ag.closes.Load(); got != 1 {
		t.Fatalf("agent close count = %d, want 1", got)
	}
	if got := product.Control.ChangeModel(context.Background(), nil, "other"); got.Status != ModelChangeClosed {
		t.Fatalf("ChangeModel after Close = %q, want %q", got.Status, ModelChangeClosed)
	}
	if got := control.changeCalls.Load(); got != 0 {
		t.Fatalf("closed call reached adapter: %d", got)
	}
	if err := product.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := ag.closes.Load(); got != 1 {
		t.Fatalf("agent close count after second Close = %d, want 1", got)
	}
}

func TestProductCloseReturnsStableError(t *testing.T) {
	sentinel := errors.New("close failed with protected provider detail")
	ag := newTestAgent(t)
	ag.err = sentinel
	product, err := New(ag, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := product.Close(); !errors.Is(err, ErrCloseFailed) || errors.Is(err, sentinel) || strings.Contains(err.Error(), "protected") {
		t.Fatalf("Close error = %v, want bounded sentinel", err)
	}
	if err := product.Close(); !errors.Is(err, ErrCloseFailed) || errors.Is(err, sentinel) {
		t.Fatalf("second Close error = %v, want bounded sentinel", err)
	}
}

func TestProductConcurrentCloseReleasesAgentOnce(t *testing.T) {
	ag := newTestAgent(t)
	product, err := New(ag, &testControl{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var wait sync.WaitGroup
	for range 32 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			if err := product.Close(); err != nil {
				t.Errorf("Close: %v", err)
			}
		}()
	}
	wait.Wait()
	if got := ag.closes.Load(); got != 1 {
		t.Fatalf("agent close count = %d, want 1", got)
	}
}

func TestControlResultsAreBoundedAndAllowlisted(t *testing.T) {
	secret := strings.Repeat("secret", MaxModelLabelBytes)
	control := &testControl{inspect: func() SessionControlSnapshot {
		return SessionControlSnapshot{
			Status: ControlReady,
			Models: []ModelDescriptor{{ID: "model", Label: secret}},
			Reason: Reason("raw provider error: token"),
		}
	}}
	product, err := New(newTestAgent(t), control)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := product.Control.Inspect(context.Background(), nil)
	if got.Status != ControlFailed || got.Reason != ReasonInvalidResult || len(got.Models) != 0 {
		t.Fatalf("Inspect = %#v, want bounded invalid result", got)
	}
}

func TestModelInputIsRejectedBeforeAdapter(t *testing.T) {
	control := &testControl{}
	product, err := New(newTestAgent(t), control)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	for _, model := range []string{"", " ", " model", "model\nother", strings.Repeat("x", MaxModelIDBytes+1)} {
		validation := product.Control.ValidateModel(context.Background(), nil, model)
		if validation.Status != ModelIncompatible || validation.Reason != ReasonInvalidModel {
			t.Fatalf("ValidateModel(%q) = %#v", model, validation)
		}
		change := product.Control.ChangeModel(context.Background(), nil, model)
		if change.Status != ModelChangeIncompatible || change.Reason != ReasonInvalidModel {
			t.Fatalf("ChangeModel(%q) = %#v", model, change)
		}
	}
	if got := control.validateCalls.Load(); got != 0 {
		t.Fatalf("invalid validation reached adapter: %d", got)
	}
	if got := control.changeCalls.Load(); got != 0 {
		t.Fatalf("invalid change reached adapter: %d", got)
	}
}

func TestSnapshotRejectsContradictoryOrUnsafeEvidence(t *testing.T) {
	tests := []SessionControlSnapshot{
		{Status: ControlUnsupported, Models: []ModelDescriptor{{ID: "model"}}},
		{Status: ControlUnsupported, CurrentModel: "model"},
		{Status: ControlUnsupported, FastForwardSupport: true},
		{Status: ControlReady, Reason: ReasonUnavailable},
		{Status: ControlReady, Models: []ModelDescriptor{{ID: "same"}, {ID: "same"}}},
		{Status: ControlReady, Models: []ModelDescriptor{{ID: "one"}}, CurrentModel: "two"},
		{Status: ControlReady, Models: []ModelDescriptor{{ID: "model\nother"}}},
		{Status: ControlReady, Models: []ModelDescriptor{{ID: "model", Label: "unsafe\tlabel"}}},
	}
	for i, snapshot := range tests {
		control := &testControl{inspect: func() SessionControlSnapshot { return snapshot }}
		product, err := New(newTestAgent(t), control)
		if err != nil {
			t.Fatalf("case %d New: %v", i, err)
		}
		got := product.Control.Inspect(context.Background(), nil)
		if got.Status != ControlFailed || got.Reason != ReasonInvalidResult {
			t.Fatalf("case %d Inspect = %#v", i, got)
		}
	}
}

func TestOutcomeInvariantsFailClosed(t *testing.T) {
	replacementAgent := newTestAgent(t)
	replacement, err := New(replacementAgent, nil)
	if err != nil {
		t.Fatalf("replacement New: %v", err)
	}
	tests := []struct {
		name       string
		validation ModelValidation
		change     ModelChange
		context    ContextChange
	}{
		{
			name:       "unknown statuses",
			validation: ModelValidation{Status: ModelValidationStatus("other")},
			change:     ModelChange{Status: ModelChangeStatus("other")},
			context:    ContextChange{Status: ContextChangeStatus("other")},
		},
		{
			name:       "success missing evidence",
			validation: ModelValidation{Status: ModelSupported},
			change:     ModelChange{Status: ModelApplied},
			context:    ContextChange{Status: ContextSucceeded, Reason: ReasonUncertain},
		},
		{
			name:       "failure claims success evidence",
			validation: ModelValidation{Status: ModelFailed, Model: "model"},
			change:     ModelChange{Status: ModelChangeFailed, EffectiveModel: "model", Replacement: replacement},
			context:    ContextChange{Status: ContextFailed, Reason: Reason("raw secret")},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			control := &testControl{
				validate: func() ModelValidation { return test.validation },
				change:   func() ModelChange { return test.change },
				forward:  func() ContextChange { return test.context },
			}
			product, err := New(newTestAgent(t), control)
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			if got := product.Control.ValidateModel(context.Background(), nil, "model"); got.Status != ModelFailed || got.Reason != ReasonInvalidResult {
				t.Fatalf("ValidateModel = %#v", got)
			}
			if got := product.Control.ChangeModel(context.Background(), nil, "model"); got.Status != ModelChangeFailed || got.Reason != ReasonInvalidResult {
				t.Fatalf("ChangeModel = %#v", got)
			}
			pkg, err := NewContextPackage([]ContextMessage{{Role: ContextRoleUser, Text: "visible"}})
			if err != nil {
				t.Fatalf("NewContextPackage: %v", err)
			}
			if got := product.Control.FastForward(context.Background(), nil, pkg); got.Status != ContextFailed || got.Reason != ReasonInvalidResult {
				t.Fatalf("FastForward = %#v", got)
			}
		})
	}
	if got := replacementAgent.closes.Load(); got != 1 {
		t.Fatalf("invalid replacement close count = %d, want 1", got)
	}
}

func TestSuccessfulModelOutcomeMustMatchRequest(t *testing.T) {
	candidateAgent := newTestAgent(t)
	candidate, err := New(candidateAgent, nil)
	if err != nil {
		t.Fatalf("candidate New: %v", err)
	}
	control := &testControl{
		validate: func() ModelValidation {
			return ModelValidation{Status: ModelSupported, Model: "model-b"}
		},
		change: func() ModelChange {
			return ModelChange{Status: ModelReplacement, EffectiveModel: "model-b", Replacement: candidate}
		},
	}
	product, err := New(newTestAgent(t), control)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := product.Control.ValidateModel(context.Background(), nil, "model-a"); got.Status != ModelFailed || got.Reason != ReasonInvalidResult {
		t.Fatalf("ValidateModel mismatch = %#v", got)
	}
	if got := product.Control.ChangeModel(context.Background(), nil, "model-a"); got.Status != ModelChangeFailed || got.Reason != ReasonInvalidResult {
		t.Fatalf("ChangeModel mismatch = %#v", got)
	}
	if got := candidateAgent.closes.Load(); got != 1 {
		t.Fatalf("mismatched replacement close count = %d, want 1", got)
	}
}

func TestAllowlistedOutcomeVocabularyIsPreserved(t *testing.T) {
	validations := []ModelValidationStatus{
		ModelSupported, ModelUnsupported, ModelIncompatible, ModelBusy, ModelFailed, ModelClosed,
	}
	for _, status := range validations {
		result := ModelValidation{Status: status}
		if status == ModelSupported {
			result.Model = "model"
		}
		control := &testControl{validate: func() ModelValidation { return result }}
		product, err := New(newTestAgent(t), control)
		if err != nil {
			t.Fatalf("validation %q New: %v", status, err)
		}
		if got := product.Control.ValidateModel(context.Background(), nil, "model"); got.Status != status {
			t.Fatalf("validation status = %q, want %q", got.Status, status)
		}
	}

	changes := []ModelChangeStatus{
		ModelApplied, ModelReplacement, ModelChangeUnsupported, ModelChangeIncompatible, ModelRejected,
		ModelChangeBusy, ModelChangeFailed, ModelAmbiguous, ModelChangeClosed,
	}
	for _, status := range changes {
		result := ModelChange{Status: status}
		if status == ModelApplied || status == ModelReplacement {
			result.EffectiveModel = "model"
		}
		if status == ModelReplacement {
			candidate, err := New(newTestAgent(t), nil)
			if err != nil {
				t.Fatalf("candidate New: %v", err)
			}
			result.Replacement = candidate
		}
		control := &testControl{change: func() ModelChange { return result }}
		product, err := New(newTestAgent(t), control)
		if err != nil {
			t.Fatalf("change %q New: %v", status, err)
		}
		got := product.Control.ChangeModel(context.Background(), nil, "model")
		if got.Status != status {
			t.Fatalf("change status = %q, want %q", got.Status, status)
		}
		if got.Replacement != nil {
			_ = got.Replacement.Close()
		}
	}

	contexts := []ContextChangeStatus{
		ContextReady, ContextSucceeded, ContextUnsupported, ContextIncompatible, ContextBusy, ContextFailed,
		ContextAmbiguous, ContextClosed,
	}
	pkg, err := NewContextPackage([]ContextMessage{{Role: ContextRoleUser, Text: "visible"}})
	if err != nil {
		t.Fatalf("NewContextPackage: %v", err)
	}
	for _, status := range contexts {
		control := &testControl{forward: func() ContextChange { return ContextChange{Status: status} }}
		product, err := New(newTestAgent(t), control)
		if err != nil {
			t.Fatalf("context %q New: %v", status, err)
		}
		if got := product.Control.FastForward(context.Background(), nil, pkg); got.Status != status {
			t.Fatalf("context status = %q, want %q", got.Status, status)
		}
	}
}

func TestContextPackageBoundsAndCopy(t *testing.T) {
	messages := []ContextMessage{{Role: ContextRoleUser, Text: "hello"}, {Role: ContextRoleAssistant, Text: "hi"}}
	pkg, err := NewContextPackage(messages)
	if err != nil {
		t.Fatalf("NewContextPackage: %v", err)
	}
	messages[0].Text = "changed"
	if got := pkg.Messages()[0].Text; got != "hello" {
		t.Fatalf("stored message = %q, want copied input", got)
	}
	returned := pkg.Messages()
	returned[0].Text = "changed again"
	if got := pkg.Messages()[0].Text; got != "hello" {
		t.Fatalf("stored message = %q, want copied output", got)
	}
	if !pkg.Valid() || pkg.ByteLen() != len("hellohi") {
		t.Fatalf("package validity/length = %v/%d", pkg.Valid(), pkg.ByteLen())
	}

	bad := []ContextMessage{{Role: ContextRole("system"), Text: "hidden"}}
	if _, err := NewContextPackage(bad); !errors.Is(err, ErrInvalidContextPackage) {
		t.Fatalf("system context error = %v", err)
	}
	tooLarge := []ContextMessage{{Role: ContextRoleUser, Text: strings.Repeat("x", MaxContextMessageBytes+1)}}
	if _, err := NewContextPackage(tooLarge); !errors.Is(err, ErrInvalidContextPackage) {
		t.Fatalf("oversized context error = %v", err)
	}
}

func TestFastForwardRejectsUnconstructedPackage(t *testing.T) {
	control := &testControl{}
	product, err := New(newTestAgent(t), control)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	got := product.Control.FastForward(context.Background(), nil, ContextPackage{})
	if got.Status != ContextIncompatible || got.Reason != ReasonInvalidContext {
		t.Fatalf("FastForward = %#v", got)
	}
	if calls := control.forwardCalls.Load(); calls != 0 {
		t.Fatalf("invalid package reached adapter: %d", calls)
	}
}

func TestDirectAgentCloseDoesNotOwnProductLifecycle(t *testing.T) {
	ag := newTestAgent(t)
	control := &testControl{}
	product, err := New(ag, control)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := ag.Close(); err != nil {
		t.Fatalf("direct Agent.Close: %v", err)
	}
	if got := product.Control.ValidateModel(context.Background(), nil, "safe-model"); got.Status != ModelSupported {
		t.Fatalf("control status after direct Agent.Close = %q; direct close bypassed Product lifecycle", got.Status)
	}
	if err := product.Close(); err != nil {
		t.Fatalf("Product.Close: %v", err)
	}
	if got := ag.closes.Load(); got != 2 {
		t.Fatalf("agent close count = %d, want 2 to make misuse observable", got)
	}
}
