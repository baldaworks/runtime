package hostedagent

import (
	"context"
	"fmt"
	"iter"
	"strings"
	"sync"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/model/gemini"
	"google.golang.org/genai"
)

// NewAIStudioModel creates an ADK-compatible Gemini model backed by Google AI
// Studio.
func NewAIStudioModel(ctx context.Context, apiKey, modelName string) (model.LLM, error) {
	if err := ValidateAIStudioModel(modelName); err != nil {
		return nil, err
	}
	_ = ctx // Client construction is deliberately deferred to the execution context.
	return &lazyAIStudioModel{apiKey: apiKey, modelName: modelName}, nil
}

type lazyAIStudioModel struct {
	apiKey    string
	modelName string
	once      sync.Once
	model     model.LLM
	err       error
}

func (m *lazyAIStudioModel) Name() string { return m.modelName }

func (m *lazyAIStudioModel) GenerateContent(ctx context.Context, req *model.LLMRequest, stream bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		m.once.Do(func() {
			var cfg *genai.ClientConfig
			if strings.TrimSpace(m.apiKey) != "" {
				cfg = &genai.ClientConfig{APIKey: m.apiKey}
			}
			m.model, m.err = gemini.NewModel(ctx, m.modelName, cfg)
			if m.err != nil {
				m.err = fmt.Errorf("create gemini model: %w", m.err)
			}
		})
		if m.err != nil {
			yield(nil, m.err)
			return
		}
		m.model.GenerateContent(ctx, req, stream)(yield)
	}
}

// ValidateAIStudioModel checks the required model parameter without creating a
// client. Credentials continue to use the client SDK's environment defaults.
func ValidateAIStudioModel(modelName string) error {
	if strings.TrimSpace(modelName) == "" {
		return fmt.Errorf("model is required for aistudio provider")
	}
	return nil
}
