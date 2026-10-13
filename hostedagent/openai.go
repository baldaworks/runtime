package hostedagent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"iter"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/genai"
)

const (
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
	defaultOpenAITimeout = 30 * time.Second

	// openAIRoleAssistant is the OpenAI role for model-authored messages.
	openAIRoleAssistant = "assistant"
	// openAIRoleSystem is the OpenAI role for the system instruction.
	openAIRoleSystem = "system"
	// openAIRoleTool is the OpenAI role for tool results sent back to the model.
	openAIRoleTool = "tool"
	// openAIRoleUser is the OpenAI role for end-user messages.
	openAIRoleUser = "user"
	// openAIToolTypeFunction is the only tool type this adapter emits.
	openAIToolTypeFunction = "function"
)

func openAIBaseURL() string {
	if u := os.Getenv("OPENAI_BASE_URL"); u != "" {
		return strings.TrimRight(u, "/")
	}
	return defaultOpenAIBaseURL
}

// OpenAIModel adapts the OpenAI chat completions API to the ADK model
// interface.
type OpenAIModel struct {
	name            string
	apiKey          string
	client          *http.Client
	reasoningEffort string
}

// OpenAIModelOptions controls optional OpenAI-compatible request behavior.
type OpenAIModelOptions struct {
	// Timeout defaults to 30 seconds when zero.
	Timeout time.Duration
	// ReasoningEffort is omitted when empty. Only "none" is supported.
	ReasoningEffort string
}

// openAIToolDefinition is one entry of the request "tools" array.
type openAIToolDefinition struct {
	Type     string          `json:"type"`
	Function json.RawMessage `json:"function"`
}

// openAIFunction carries a declared function and, on the response path, the
// arguments produced by the model.
type openAIFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
}

// openAIToolCall is one entry of a response message "tool_calls" array.
type openAIToolCall struct {
	ID       string         `json:"id"`
	Type     string         `json:"type"`
	Function openAIFunction `json:"function"`
}

type openAIChatRequest struct {
	Model       string                 `json:"model"`
	Messages    []openAIMessage        `json:"messages"`
	Thinking    *openAIThinking        `json:"thinking,omitempty"`
	Temperature float64                `json:"temperature,omitempty"`
	TopP        float64                `json:"top_p,omitempty"`
	MaxTokens   int32                  `json:"max_tokens,omitempty"`
	Stop        []string               `json:"stop,omitempty"`
	Tools       []openAIToolDefinition `json:"tools,omitempty"`

	// toolAliases translates OpenAI-safe function names back to the runtime
	// names expected by ADK. It is request-local and never serialized.
	toolAliases map[string]string
}

type openAIThinking struct {
	Type string `json:"type"`
}

// openAIMessage is one entry of the request "messages" array.
//
// Content is deliberately not omitempty: the OpenAI schema marks content as
// required for role=tool, so an empty tool result must serialize as
// {"role":"tool","tool_call_id":"...","content":""} rather than dropping the
// key and being rejected.
type openAIMessage struct {
	Role       string           `json:"role"`
	Content    string           `json:"content"`
	ToolCallID string           `json:"tool_call_id,omitempty"`
	ToolCalls  []openAIToolCall `json:"tool_calls,omitempty"`
}

type openAIChoice struct {
	Message openAIMessage `json:"message"`
}

type openAIChatResponse struct {
	Choices []openAIChoice `json:"choices"`
}

// NewOpenAIModel creates an ADK-compatible model backed by the OpenAI API.
func NewOpenAIModel(apiKey, modelName string) (*OpenAIModel, error) {
	return NewOpenAIModelWithOptions(apiKey, modelName, OpenAIModelOptions{})
}

// NewOpenAIModelWithTimeout creates a model with a configured HTTP request timeout.
func NewOpenAIModelWithTimeout(apiKey, modelName string, timeout time.Duration) (*OpenAIModel, error) {
	if timeout <= 0 {
		return nil, fmt.Errorf("openai timeout must be positive")
	}
	return NewOpenAIModelWithOptions(apiKey, modelName, OpenAIModelOptions{Timeout: timeout})
}

// NewOpenAIModelWithOptions creates a model with explicit request options.
func NewOpenAIModelWithOptions(apiKey, modelName string, opts OpenAIModelOptions) (*OpenAIModel, error) {
	if err := ValidateOpenAIModelOptions(apiKey, modelName, opts); err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout == 0 {
		timeout = defaultOpenAITimeout
	}
	return &OpenAIModel{
		name:            modelName,
		apiKey:          apiKey,
		client:          &http.Client{Timeout: timeout},
		reasoningEffort: opts.ReasoningEffort,
	}, nil
}

// ValidateOpenAIModelOptions checks construction parameters without starting a
// provider, opening MCP connections, or issuing model requests.
func ValidateOpenAIModelOptions(apiKey, modelName string, opts OpenAIModelOptions) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("api_key is required for openai provider")
	}

	if strings.TrimSpace(modelName) == "" {
		return fmt.Errorf("model is required for openai provider")
	}

	if opts.Timeout < 0 {
		return fmt.Errorf("openai timeout must be positive")
	}
	switch opts.ReasoningEffort {
	case "", "none":
	default:
		return fmt.Errorf("openai reasoning_effort currently supports only none")
	}
	return nil
}

// Name returns the configured OpenAI model identifier.
func (m *OpenAIModel) Name() string {
	return m.name
}

// GenerateContent sends a single non-streaming request to the OpenAI API.
func (m *OpenAIModel) GenerateContent(ctx context.Context, req *model.LLMRequest, _ bool) iter.Seq2[*model.LLMResponse, error] {
	return func(yield func(*model.LLMResponse, error) bool) {
		resp, err := m.generate(ctx, req)
		yield(resp, err)
	}
}

func (m *OpenAIModel) generate(ctx context.Context, req *model.LLMRequest) (*model.LLMResponse, error) {
	payload, err := buildChatRequest(req, m.name)
	if err != nil {
		return nil, err
	}
	if m.reasoningEffort == "none" {
		payload.Thinking = &openAIThinking{Type: "disabled"}
	}

	respBody, err := m.doChatRequest(ctx, payload)
	if err != nil {
		return nil, err
	}

	content, err := parseChatResponseWithAliases(respBody, payload.toolAliases)
	if err != nil {
		return nil, err
	}

	// When the model returns function calls, ADK must run another turn to
	// execute the tools and feed results back. Reporting TurnComplete here
	// would end the run before any tool ever executes.
	hasToolCalls := false
	for _, part := range content.Parts {
		if part.FunctionCall != nil {
			hasToolCalls = true
			break
		}
	}

	return &model.LLMResponse{
		Content:      content,
		TurnComplete: !hasToolCalls,
	}, nil
}

func buildChatRequest(req *model.LLMRequest, modelName string) (openAIChatRequest, error) {
	var tools []openAIToolDefinition
	var aliases map[string]string
	var runtimeToOpenAI map[string]string
	reservedNames := openAIReservedFunctionNames(nil)
	if req != nil && req.Config != nil {
		tools, aliases, runtimeToOpenAI = openAIToolsWithAliases(req.Config)
		reservedNames = openAIReservedFunctionNames(req.Config)
	}
	if aliases == nil {
		aliases = make(map[string]string)
	}
	if runtimeToOpenAI == nil {
		runtimeToOpenAI = make(map[string]string)
	}
	if req != nil {
		addHistoricalToolAliases(req.Contents, aliases, runtimeToOpenAI, reservedNames)
	}

	messages := openAIMessagesFromRequestWithAliases(req, runtimeToOpenAI)
	if len(messages) == 0 {
		return openAIChatRequest{}, fmt.Errorf("openai request missing content")
	}

	payload := openAIChatRequest{
		Model:       modelName,
		Messages:    messages,
		toolAliases: aliases,
	}

	if req != nil && req.Config != nil {
		if req.Config.Temperature != nil {
			payload.Temperature = float64(*req.Config.Temperature)
		}
		if req.Config.TopP != nil {
			payload.TopP = float64(*req.Config.TopP)
		}
		if req.Config.MaxOutputTokens > 0 {
			payload.MaxTokens = req.Config.MaxOutputTokens
		}
		if len(req.Config.StopSequences) > 0 {
			payload.Stop = req.Config.StopSequences
		}

		if len(tools) > 0 {
			payload.Tools = tools
		}
	}

	return payload, nil
}

// openAIToolsFromConfig converts genai tool configuration into OpenAI tool
// definitions. Names OpenAI cannot accept receive reversible aliases.
func openAIToolsFromConfig(cfg *genai.GenerateContentConfig) []openAIToolDefinition {
	tools, _, _ := openAIToolsWithAliases(cfg)
	return tools
}

// openAIToolsWithAliases converts declarations and returns mappings in both
// directions. OpenAI receives only valid names; ADK receives the original MCP
// name when it dispatches a returned function call.
func openAIToolsWithAliases(cfg *genai.GenerateContentConfig) ([]openAIToolDefinition, map[string]string, map[string]string) {
	if cfg == nil || len(cfg.Tools) == 0 {
		return nil, nil, nil
	}

	aliases := make(map[string]string)
	runtimeToOpenAI := make(map[string]string)
	seenRuntimeNames := make(map[string]struct{})
	reservedNames := openAIReservedFunctionNames(cfg)
	newAlias := func(runtimeName string) string {
		// The alias must be stable across turns. Tool declarations can be
		// reordered or a subset can be sent on a later turn, while the history
		// still contains calls from earlier turns.
		return stableOpenAIFunctionAlias(runtimeName, aliases, reservedNames)
	}

	var defs []openAIToolDefinition
	for _, t := range cfg.Tools {
		if t == nil {
			continue
		}
		for _, fd := range t.FunctionDeclarations {
			if fd == nil {
				continue
			}

			runtimeName := fd.Name
			if strings.TrimSpace(runtimeName) == "" {
				continue
			}
			if _, seen := seenRuntimeNames[runtimeName]; seen {
				continue
			}
			seenRuntimeNames[runtimeName] = struct{}{}
			openAIName := runtimeName
			if !isValidOpenAIFuncName(runtimeName) {
				openAIName = newAlias(runtimeName)
				aliases[openAIName] = runtimeName
				runtimeToOpenAI[runtimeName] = openAIName
			}

			params := []byte("{}")
			if fd.Parameters != nil {
				if p, err := marshalJSONSchema(fd.Parameters); err == nil {
					params = p
				}
			} else if fd.ParametersJsonSchema != nil {
				if p, err := json.Marshal(fd.ParametersJsonSchema); err == nil {
					params = p
				}
			}

			fn := openAIFunction{
				Name:        openAIName,
				Description: fd.Description,
				Parameters:  params,
			}
			fnJSON, err := json.Marshal(fn)
			if err != nil {
				continue
			}
			defs = append(defs, openAIToolDefinition{
				Type:     "function",
				Function: fnJSON,
			})
		}
	}
	return defs, aliases, runtimeToOpenAI
}

func openAIReservedFunctionNames(cfg *genai.GenerateContentConfig) map[string]struct{} {
	reserved := make(map[string]struct{})
	if cfg == nil {
		return reserved
	}
	for _, tool := range cfg.Tools {
		if tool == nil {
			continue
		}
		for _, declaration := range tool.FunctionDeclarations {
			if declaration != nil && isValidOpenAIFuncName(declaration.Name) {
				reserved[declaration.Name] = struct{}{}
			}
		}
	}
	return reserved
}

func addHistoricalToolAliases(contents []*genai.Content, aliases, runtimeToOpenAI map[string]string, reservedNames map[string]struct{}) {
	for _, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part != nil && part.FunctionCall != nil && isValidOpenAIFuncName(part.FunctionCall.Name) {
				reservedNames[part.FunctionCall.Name] = struct{}{}
			}
		}
	}

	for _, content := range contents {
		if content == nil {
			continue
		}
		for _, part := range content.Parts {
			if part == nil || part.FunctionCall == nil {
				continue
			}
			runtimeName := part.FunctionCall.Name
			if isValidOpenAIFuncName(runtimeName) {
				continue
			}
			if !isValidOpenAIFuncName(runtimeName) {
				alias := stableOpenAIFunctionAlias(runtimeName, aliases, reservedNames)
				aliases[alias] = runtimeName
				runtimeToOpenAI[runtimeName] = alias
			}
		}
	}
}

func stableOpenAIFunctionAlias(runtimeName string, aliases map[string]string, reservedNames map[string]struct{}) string {
	for salt := 0; ; salt++ {
		seed := runtimeName
		if salt > 0 {
			seed = fmt.Sprintf("%s#%d", runtimeName, salt)
		}
		digest := sha256.Sum256([]byte(seed))
		candidate := fmt.Sprintf("runtime_tool_%x", digest[:12])
		_, reserved := reservedNames[candidate]
		if !reserved {
			if mappedRuntimeName, exists := aliases[candidate]; !exists || mappedRuntimeName == runtimeName {
				return candidate
			}
		}
	}
}

// genaiSchemaTypes maps the genai schema type constants onto the JSON Schema
// type keywords OpenAI expects. genai spells them in upper case ("OBJECT"),
// JSON Schema and the OpenAI API use lower case ("object"), and sending the
// upper-case form is rejected.
var genaiSchemaTypes = map[string]string{
	"TYPE_UNSPECIFIED": "",
	"OBJECT":           "object",
	"ARRAY":            "array",
	"STRING":           "string",
	"NUMBER":           "number",
	"INTEGER":          "integer",
	"BOOLEAN":          "boolean",
}

// marshalJSONSchema converts a genai.Schema into the JSON Schema object OpenAI
// expects and marshals it.
//
// A direct json.Marshal of genai.Schema would keep genai's own enum spelling
// ("type":"OBJECT"), which the OpenAI API rejects. Marshal first so every
// schema field supported by genai survives, then normalize only the type fields
// that belong to schema nodes.
func marshalJSONSchema(schema *genai.Schema) ([]byte, error) {
	encoded, err := json.Marshal(schema)
	if err != nil {
		return nil, err
	}

	var value map[string]any
	if err := json.Unmarshal(encoded, &value); err != nil {
		return nil, err
	}
	normalizeGenaiSchemaTypes(value)
	return json.Marshal(value)
}

// normalizeGenaiSchemaTypes normalizes a serialized schema in place. It walks
// only fields whose values are themselves schemas, so an arbitrary "type" key
// inside Default or Example data is left untouched.
func normalizeGenaiSchemaTypes(schema map[string]any) {
	if typeName, ok := schema["type"].(string); ok {
		if normalized, found := genaiSchemaTypes[strings.ToUpper(strings.TrimSpace(typeName))]; found {
			if normalized == "" {
				delete(schema, "type")
			} else {
				schema["type"] = normalized
			}
		}
	}

	if properties, ok := schema["properties"].(map[string]any); ok {
		for _, property := range properties {
			if nested, ok := property.(map[string]any); ok {
				normalizeGenaiSchemaTypes(nested)
			}
		}
	}
	if items, ok := schema["items"].(map[string]any); ok {
		normalizeGenaiSchemaTypes(items)
	}
	if anyOf, ok := schema["anyOf"].([]any); ok {
		for _, member := range anyOf {
			if nested, ok := member.(map[string]any); ok {
				normalizeGenaiSchemaTypes(nested)
			}
		}
	}
}

// validOpenAIFuncNameRe matches the OpenAI API requirement for function names.
var validOpenAIFuncNameRe = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// isValidOpenAIFuncName checks whether a function name is valid per the OpenAI
// API spec.
func isValidOpenAIFuncName(name string) bool {
	return validOpenAIFuncNameRe.MatchString(name)
}

func (m *OpenAIModel) doChatRequest(ctx context.Context, payload openAIChatRequest) ([]byte, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, openAIBaseURL()+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("create openai request: %w", err)
	}

	httpReq.Header.Set("Authorization", "Bearer "+m.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := m.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("send openai request: %w", err)
	}
	defer func() {
		_ = resp.Body.Close()
	}()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read openai response: %w", err)
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil, fmt.Errorf("openai request failed: %s", strings.TrimSpace(string(respBody)))
	}

	return respBody, nil
}

func parseChatResponse(respBody []byte) (*genai.Content, error) {
	return parseChatResponseWithAliases(respBody, nil)
}

func parseChatResponseWithAliases(respBody []byte, aliases map[string]string) (*genai.Content, error) {
	var parsed openAIChatResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return nil, fmt.Errorf("parse openai response: %w", err)
	}

	if len(parsed.Choices) == 0 {
		return nil, fmt.Errorf("openai response missing choices")
	}

	msg := parsed.Choices[0].Message

	// A response may carry text, tool calls, or both.
	var parts []*genai.Part

	if strings.TrimSpace(msg.Content) != "" {
		parts = append(parts, genai.NewPartFromText(msg.Content))
	}

	for _, tc := range msg.ToolCalls {
		if tc.Type != openAIToolTypeFunction {
			continue
		}

		// OpenAI-compatible providers return arguments as a JSON string.
		// Some (DeepSeek) return it doubly encoded: the RawMessage holds a
		// JSON string whose value is the JSON object. Decode both forms, but
		// reject malformed JSON rather than dispatching an incomplete call.
		args, err := decodeFunctionArguments(tc.Function.Arguments)
		if err != nil {
			return nil, fmt.Errorf("decode tool call %q arguments: %w", tc.ID, err)
		}

		name := tc.Function.Name
		if runtimeName, ok := aliases[name]; ok {
			name = runtimeName
		}
		parts = append(parts, &genai.Part{
			FunctionCall: &genai.FunctionCall{
				ID:   tc.ID,
				Name: name,
				Args: args,
			},
		})
	}

	if len(parts) == 0 {
		return nil, fmt.Errorf("openai response missing content and tool_calls")
	}

	return &genai.Content{
		Parts: parts,
		Role:  genai.RoleModel,
	}, nil
}

func openAIMessagesFromRequest(req *model.LLMRequest) []openAIMessage {
	return openAIMessagesFromRequestWithAliases(req, nil)
}

func openAIMessagesFromRequestWithAliases(req *model.LLMRequest, runtimeToOpenAI map[string]string) []openAIMessage {
	if req == nil {
		return nil
	}

	messages := make([]openAIMessage, 0, len(req.Contents)+1)

	if req.Config != nil && req.Config.SystemInstruction != nil {
		text := contentText(req.Config.SystemInstruction)
		if strings.TrimSpace(text) != "" {
			messages = append(messages, openAIMessage{
				Role:    openAIRoleSystem,
				Content: text,
			})
		}
	}

	for _, content := range req.Contents {
		text, toolCalls, toolResponses := contentToOpenAIWithAliases(content, runtimeToOpenAI)
		if text == "" && len(toolCalls) == 0 && len(toolResponses) == 0 {
			continue
		}

		role := openAIRole(content.Role)
		if len(toolCalls) > 0 {
			if role != openAIRoleAssistant {
				role = openAIRoleAssistant
			}
			messages = append(messages, openAIMessage{
				Role:      role,
				Content:   text,
				ToolCalls: toolCalls,
			})
		}
		if len(toolResponses) > 0 {
			messages = append(messages, toolResponses...)
		}
		if text != "" && len(toolCalls) == 0 {
			messages = append(messages, openAIMessage{
				Role:    role,
				Content: text,
			})
		}
	}

	return messages
}

// contentToOpenAI extracts text, tool calls, and tool responses from a
// genai.Content so the full tool round trip survives in the message history.
func contentToOpenAI(content *genai.Content) (string, []openAIToolCall, []openAIMessage) {
	return contentToOpenAIWithAliases(content, nil)
}

func contentToOpenAIWithAliases(content *genai.Content, runtimeToOpenAI map[string]string) (string, []openAIToolCall, []openAIMessage) {
	if content == nil {
		return "", nil, nil
	}

	var textParts []string
	var calls []openAIToolCall
	var toolResponses []openAIMessage

	for _, part := range content.Parts {
		if part == nil {
			continue
		}
		if part.Text != "" {
			textParts = append(textParts, part.Text)
		}
		if part.FunctionCall != nil {
			// The OpenAI API requires arguments as a JSON string, not an
			// object, so marshal the object and then re-marshal it as a
			// string value.
			argsBytes, err := json.Marshal(part.FunctionCall.Args)
			if err != nil {
				argsBytes = []byte("{}")
			}
			argsStrBytes, err := json.Marshal(string(argsBytes))
			if err != nil {
				argsStrBytes = []byte(`"{}"`)
			}
			toolCallID := part.FunctionCall.ID
			if toolCallID == "" {
				toolCallID = part.FunctionCall.Name
			}
			name := part.FunctionCall.Name
			if alias, ok := runtimeToOpenAI[name]; ok {
				name = alias
			}
			calls = append(calls, openAIToolCall{
				ID:   toolCallID,
				Type: openAIToolTypeFunction,
				Function: openAIFunction{
					Name:      name,
					Arguments: argsStrBytes,
				},
			})
		}
		if part.FunctionResponse != nil {
			respStr := functionResponseText(part.FunctionResponse.Response)
			toolCallID := part.FunctionResponse.ID
			if toolCallID == "" {
				toolCallID = part.FunctionResponse.Name
			}
			toolResponses = append(toolResponses, openAIMessage{
				Role:       openAIRoleTool,
				ToolCallID: toolCallID,
				Content:    respStr,
			})
		}
	}

	return strings.Join(textParts, ""), calls, toolResponses
}

// functionResponseText converts the ADK response object to the string content
// required by an OpenAI tool message. Prefer the standard output key, while
// accepting the common result/response aliases used by existing tools.
func functionResponseText(response map[string]any) string {
	if response == nil {
		return ""
	}
	if _, hasOutput := response["output"]; hasOutput {
		if _, hasError := response["error"]; hasError {
			return jsonValueText(response)
		}
	}

	for _, key := range []string{"output", "result", "response", "text", "content", "error"} {
		value, ok := response[key]
		if !ok {
			continue
		}
		if key == "error" {
			return "error: " + jsonValueText(value)
		}
		return jsonValueText(value)
	}

	return jsonValueText(response)
}

func jsonValueText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

// openAIArgumentsMaxDepth bounds how many times a JSON string encoding of the
// arguments is peeled. Providers that double-encode wrap once; the extra levels
// cover a string that itself contains an encoded string without letting a
// crafted payload drive an unbounded loop.
const openAIArgumentsMaxDepth = 4

// decodeFunctionArguments turns the raw "arguments" field of a tool call into
// the object genai.FunctionCall.Args expects.
//
// Providers disagree on the shape: some send a JSON object, some send a JSON
// string holding that object, and some encode it twice. The value is peeled
// repeatedly until it stops being a JSON string, then coerced into a map.
func decodeFunctionArguments(raw json.RawMessage) (map[string]any, error) {
	if len(raw) == 0 {
		return nil, nil
	}

	value := raw
	var decoded any
	for depth := 0; depth < openAIArgumentsMaxDepth; depth++ {
		if err := json.Unmarshal(value, &decoded); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		// A JSON string may itself hold more JSON: unwrap and try again.
		if asString, ok := decoded.(string); ok {
			trimmed := strings.TrimSpace(asString)
			if trimmed == "" {
				break
			}
			// A scalar string is valid argument data too. Only peel it when
			// its contents are themselves valid JSON.
			var nested any
			if err := json.Unmarshal([]byte(trimmed), &nested); err != nil {
				break
			}
			value = json.RawMessage(trimmed)
			continue
		}
		break
	}

	switch typed := decoded.(type) {
	case map[string]any:
		return typed, nil
	default:
		return nil, fmt.Errorf("arguments must decode to a JSON object, got %T", typed)
	}
}

func contentText(content *genai.Content) string {
	if content == nil {
		return ""
	}

	var sb strings.Builder

	for _, part := range content.Parts {
		if part == nil || part.Text == "" {
			continue
		}

		sb.WriteString(part.Text)
	}

	return sb.String()
}

func openAIRole(role string) string {
	switch strings.ToLower(strings.TrimSpace(role)) {
	case openAIRoleAssistant, "model":
		return openAIRoleAssistant
	case openAIRoleSystem:
		return openAIRoleSystem
	default:
		return openAIRoleUser
	}
}
