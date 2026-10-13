package hostedagent

import (
	"context"
	"fmt"
	"strings"

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

	var cfg *genai.ClientConfig
	if strings.TrimSpace(apiKey) != "" {
		cfg = &genai.ClientConfig{APIKey: apiKey}
	}

	llmModel, err := gemini.NewModel(ctx, modelName, cfg)
	if err != nil {
		return nil, fmt.Errorf("create gemini model: %w", err)
	}

	return llmModel, nil
}

// ValidateAIStudioModel checks the required model parameter without creating a
// client. Credentials continue to use the client SDK's environment defaults.
func ValidateAIStudioModel(modelName string) error {
	if strings.TrimSpace(modelName) == "" {
		return fmt.Errorf("model is required for aistudio provider")
	}
	return nil
}
