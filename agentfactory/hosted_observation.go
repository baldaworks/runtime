package agentfactory

import (
	"context"
	"iter"
	"sync"

	"google.golang.org/adk/v2/model"
)

// hostedModelObservation records only a successful model execution by the
// exact constructed Product. It is intentionally process-local.
type hostedModelObservation struct {
	mu    sync.RWMutex
	model string
}

func (o *hostedModelObservation) record(modelName string) {
	if o == nil {
		return
	}
	o.mu.Lock()
	o.model = modelName
	o.mu.Unlock()
}

func (o *hostedModelObservation) executed(modelName string) bool {
	if o == nil {
		return false
	}
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.model == modelName
}

type observedHostedModel struct {
	model.LLM
	modelName   string
	observation *hostedModelObservation
}

func observeHostedModel(llm model.LLM, modelName string, observation *hostedModelObservation) model.LLM {
	if llm == nil || observation == nil {
		return llm
	}
	return &observedHostedModel{LLM: llm, modelName: modelName, observation: observation}
}

func (m *observedHostedModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		observed := false
		failed := false
		stopped := false
		m.LLM.GenerateContent(ctx, req, stream)(func(response *model.LLMResponse, err error) bool {
			if err != nil {
				failed = true
			}
			if response != nil && response.ErrorCode != "" {
				failed = true
			}
			if response != nil {
				observed = true
			}
			keepGoing := yield(response, err)
			if !keepGoing {
				stopped = true
			}
			return keepGoing
		})
		if observed && !failed && !stopped {
			m.observation.record(m.modelName)
		}
	}
}
