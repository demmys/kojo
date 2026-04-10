package lmsproxy

import "encoding/json"

// --- Anthropic Messages API (incoming) ---

// AnthropicRequest is the POST /v1/messages request body.
type AnthropicRequest struct {
	Model     string             `json:"model"`
	MaxTokens int                `json:"max_tokens,omitempty"`
	System    json.RawMessage    `json:"system,omitempty"` // string or []SystemBlock
	Messages  []AnthropicMessage `json:"messages"`
	Tools     []AnthropicTool    `json:"tools,omitempty"`
	Stream    bool               `json:"stream"`
	// Claude-specific fields we accept but ignore.
	Thinking json.RawMessage `json:"thinking,omitempty"`
	Metadata json.RawMessage `json:"metadata,omitempty"`
}

// AnthropicMessage is a single message in the messages array.
type AnthropicMessage struct {
	Role    string          `json:"role"` // "user" | "assistant"
	Content json.RawMessage `json:"content"`
}

// AnthropicContentBlock represents a typed content block inside a message.
type AnthropicContentBlock struct {
	Type      string          `json:"type"` // "text" | "tool_use" | "tool_result" | "thinking"
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`           // tool_use id
	Name      string          `json:"name,omitempty"`         // tool_use function name
	Input     json.RawMessage `json:"input,omitempty"`        // tool_use arguments
	ToolUseID string          `json:"tool_use_id,omitempty"`  // tool_result reference
	Content   json.RawMessage `json:"content,omitempty"`      // tool_result content
	IsError   bool            `json:"is_error,omitempty"`     // tool_result error flag
}

// AnthropicTool defines a tool the model can call.
type AnthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// --- OpenAI Responses API (outgoing) ---

// OAIRequest is the POST /v1/responses request body.
type OAIRequest struct {
	Model              string         `json:"model"`
	Input              json.RawMessage `json:"input"` // string or []InputItem
	Instructions       string         `json:"instructions,omitempty"`
	PreviousResponseID string         `json:"previous_response_id,omitempty"`
	MaxOutputTokens    int            `json:"max_output_tokens,omitempty"`
	Tools              []OAITool      `json:"tools,omitempty"`
	Stream             bool           `json:"stream"`
	Store              bool           `json:"store"`
}

// OAIInputItem is one element in the input array.
type OAIInputItem struct {
	Type    string           `json:"type"` // "message" | "function_call" | "function_call_output"
	Role    string           `json:"role,omitempty"`
	Content []OAIContentPart `json:"content,omitempty"`
	// function_call fields
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	// function_call / function_call_output fields
	CallID string `json:"call_id,omitempty"`
	Output string `json:"output,omitempty"`
}

// OAIContentPart is a typed part inside a message's content array.
type OAIContentPart struct {
	Type string `json:"type"` // "input_text" | "output_text"
	Text string `json:"text"`
}

// OAITool defines a function-type tool for the Responses API.
// Unlike Chat Completions, Responses API uses a flat structure without a
// nested "function" wrapper.
type OAITool struct {
	Type        string          `json:"type"` // "function"
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
}

// --- OpenAI Responses SSE event payloads ---

// OAIResponseCreated is the payload for response.created.
type OAIResponseCreated struct {
	Type     string            `json:"type"`
	Response OAIResponseObject `json:"response"`
}

// OAIResponseObject is the top-level response envelope.
type OAIResponseObject struct {
	ID     string    `json:"id"`
	Model  string    `json:"model"`
	Status string    `json:"status,omitempty"`
	Usage  *OAIUsage `json:"usage,omitempty"`
}

// OAIUsage tracks token counts.
type OAIUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// OAIOutputItemAdded is the payload for response.output_item.added.
type OAIOutputItemAdded struct {
	Type        string        `json:"type"`
	OutputIndex int           `json:"output_index"`
	Item        OAIOutputItem `json:"item"`
}

// OAIOutputItem represents an output item (message or function_call).
type OAIOutputItem struct {
	Type   string `json:"type"` // "message" | "function_call"
	ID     string `json:"id"`
	Name   string `json:"name,omitempty"`   // function_call name
	CallID string `json:"call_id,omitempty"`
}

// OAIContentPartAdded is the payload for response.content_part.added.
type OAIContentPartAdded struct {
	Type         string `json:"type"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Part         struct {
		Type string `json:"type"` // "output_text"
		Text string `json:"text"`
	} `json:"part"`
}

// OAIOutputTextDelta is the payload for response.output_text.delta.
type OAIOutputTextDelta struct {
	Type         string `json:"type"`
	OutputIndex  int    `json:"output_index"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

// OAIFuncCallArgsDelta is the payload for response.function_call_arguments.delta.
type OAIFuncCallArgsDelta struct {
	Type        string `json:"type"`
	OutputIndex int    `json:"output_index"`
	ItemID      string `json:"item_id"`
	Delta       string `json:"delta"`
}

// OAIFuncCallArgsDone is the payload for response.function_call_arguments.done.
type OAIFuncCallArgsDone struct {
	Type        string `json:"type"`
	OutputIndex int    `json:"output_index"`
	ItemID      string `json:"item_id"`
	Name        string `json:"name"`
	CallID      string `json:"call_id"`
	Arguments   string `json:"arguments"`
}

// OAIOutputItemDone is the payload for response.output_item.done.
type OAIOutputItemDone struct {
	Type        string        `json:"type"`
	OutputIndex int           `json:"output_index"`
	Item        OAIOutputItem `json:"item"`
}

// OAIResponseCompleted is the payload for response.completed.
type OAIResponseCompleted struct {
	Type     string            `json:"type"`
	Response OAIResponseObject `json:"response"`
}
