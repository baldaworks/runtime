package agentfactory

import (
	"context"
	"testing"

	"github.com/baldaworks/runtime/v2/agentproduct"
	acpagent "github.com/normahq/go-adk-acpagent/v2"
	"google.golang.org/adk/v2/session"
)

const testModelID = "model-a"

type fakeACPController struct {
	snapshot   acpagent.SessionControlSnapshot
	validation acpagent.ModelValidation
	change     acpagent.ModelChange
	inspects   int
	validates  int
	changes    int
}

func (c *fakeACPController) Inspect(context.Context, session.Session) acpagent.SessionControlSnapshot {
	c.inspects++
	return c.snapshot
}

func (c *fakeACPController) ValidateModel(context.Context, session.Session, string) acpagent.ModelValidation {
	c.validates++
	return c.validation
}

func (c *fakeACPController) ChangeModel(context.Context, session.Session, string) acpagent.ModelChange {
	c.changes++
	return c.change
}

func TestACPInspectMapping(t *testing.T) {
	tests := []struct {
		name   string
		input  acpagent.SessionControlSnapshot
		status agentproduct.ControlStatus
		reason agentproduct.Reason
	}{
		{
			name: "available",
			input: acpagent.SessionControlSnapshot{
				Status:       acpagent.SessionControlAvailable,
				Revision:     "opaque-revision",
				CurrentModel: testModelID,
				Models:       []acpagent.SessionModel{{ID: testModelID, Label: "Model A"}},
			},
			status: agentproduct.ControlReady,
		},
		{name: "unavailable", input: acpagent.SessionControlSnapshot{Status: acpagent.SessionControlUnavailable, Code: "attachment_unavailable"}, status: agentproduct.ControlUnsupported, reason: agentproduct.ReasonUnavailable},
		{name: "busy", input: acpagent.SessionControlSnapshot{Status: acpagent.SessionControlBusy, Code: "busy"}, status: agentproduct.ControlBusy, reason: agentproduct.ReasonActiveWork},
		{name: "failed", input: acpagent.SessionControlSnapshot{Status: acpagent.SessionControlFailed, Code: "invalid_state"}, status: agentproduct.ControlFailed, reason: agentproduct.ReasonBindingMismatch},
		{name: "closed", input: acpagent.SessionControlSnapshot{Status: acpagent.SessionControlClosed, Code: "closed"}, status: agentproduct.ControlClosed, reason: agentproduct.ReasonClosed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			controller := &fakeACPController{snapshot: test.input}
			got := (&acpSessionControl{controller: controller}).Inspect(t.Context(), nil)
			if got.Status != test.status || got.Reason != test.reason {
				t.Fatalf("Inspect = %#v, want status %q reason %q", got, test.status, test.reason)
			}
			if test.status == agentproduct.ControlReady {
				if got.CurrentModel != testModelID || got.ControlRevision != "opaque-revision" || len(got.Models) != 1 || got.Models[0].ID != testModelID || got.FastForwardSupport {
					t.Fatalf("available snapshot = %#v", got)
				}
			}
		})
	}
}

func TestACPValidationOutcomeMapping(t *testing.T) {
	tests := []struct {
		input  acpagent.ModelValidationOutcome
		code   string
		status agentproduct.ModelValidationStatus
		reason agentproduct.Reason
	}{
		{acpagent.ModelValidationSupported, "", agentproduct.ModelSupported, agentproduct.ReasonNone},
		{acpagent.ModelValidationUnsupported, "attachment_unavailable", agentproduct.ModelUnsupported, agentproduct.ReasonUnavailable},
		{acpagent.ModelValidationIncompatible, "unknown_model", agentproduct.ModelIncompatible, agentproduct.ReasonInvalidModel},
		{acpagent.ModelValidationBusy, "busy", agentproduct.ModelBusy, agentproduct.ReasonActiveWork},
		{acpagent.ModelValidationFailed, "invalid_state", agentproduct.ModelFailed, agentproduct.ReasonBindingMismatch},
		{acpagent.ModelValidationClosed, "closed", agentproduct.ModelClosed, agentproduct.ReasonClosed},
	}
	for _, test := range tests {
		controller := &fakeACPController{validation: acpagent.ModelValidation{Outcome: test.input, Code: test.code}}
		got := (&acpSessionControl{controller: controller}).ValidateModel(t.Context(), nil, testModelID)
		if got.Status != test.status || got.Reason != test.reason {
			t.Fatalf("ValidateModel(%q) = %#v", test.input, got)
		}
		if test.status == agentproduct.ModelSupported && got.Model != testModelID {
			t.Fatalf("supported model = %q", got.Model)
		}
		if controller.changes != 0 {
			t.Fatalf("validation %q called mutation", test.input)
		}
	}
}

func TestACPModelChangeOutcomeMapping(t *testing.T) {
	tests := []struct {
		input  acpagent.ModelChangeOutcome
		code   string
		status agentproduct.ModelChangeStatus
		reason agentproduct.Reason
	}{
		{acpagent.ModelChangeApplied, "", agentproduct.ModelApplied, agentproduct.ReasonNone},
		{acpagent.ModelChangeUnsupported, "method_unsupported", agentproduct.ModelChangeUnsupported, agentproduct.ReasonUnavailable},
		{acpagent.ModelChangeIncompatible, "unknown_model", agentproduct.ModelChangeIncompatible, agentproduct.ReasonInvalidModel},
		{acpagent.ModelChangeRejected, "request_rejected", agentproduct.ModelRejected, agentproduct.ReasonProviderReject},
		{acpagent.ModelChangeBusy, "busy", agentproduct.ModelChangeBusy, agentproduct.ReasonActiveWork},
		{acpagent.ModelChangeFailed, "canceled", agentproduct.ModelChangeFailed, agentproduct.ReasonPreDispatch},
		{acpagent.ModelChangeAmbiguous, "response_uncertain", agentproduct.ModelAmbiguous, agentproduct.ReasonUncertain},
		{acpagent.ModelChangeClosed, "closed", agentproduct.ModelChangeClosed, agentproduct.ReasonClosed},
	}
	for _, test := range tests {
		controller := &fakeACPController{change: acpagent.ModelChange{Outcome: test.input, Code: test.code, EffectiveModel: testModelID}}
		got := (&acpSessionControl{controller: controller}).ChangeModel(t.Context(), nil, testModelID)
		if got.Status != test.status || got.Reason != test.reason {
			t.Fatalf("ChangeModel(%q) = %#v", test.input, got)
		}
		if test.status == agentproduct.ModelApplied && got.EffectiveModel != testModelID {
			t.Fatalf("applied model = %q", got.EffectiveModel)
		}
		if test.status != agentproduct.ModelApplied && got.EffectiveModel != "" {
			t.Fatalf("non-applied outcome exposed model = %q", got.EffectiveModel)
		}
	}
}

func TestACPControlFastForwardIsUnsupportedWithoutProviderCall(t *testing.T) {
	controller := &fakeACPController{}
	control := &acpSessionControl{controller: controller}
	got := control.FastForward(t.Context(), nil, agentproduct.ContextPackage{})
	if got.Status != agentproduct.ContextUnsupported || got.Reason != agentproduct.ReasonUnavailable {
		t.Fatalf("FastForward = %#v", got)
	}
	if controller.inspects != 0 || controller.validates != 0 || controller.changes != 0 {
		t.Fatal("unsupported FastForward called ACP")
	}
}

func TestACPReasonMappingNeverExposesRawCode(t *testing.T) {
	if got := mapACPReason("credential=secret raw provider body"); got != agentproduct.ReasonPreDispatch {
		t.Fatalf("unknown code mapped to %q", got)
	}
}
