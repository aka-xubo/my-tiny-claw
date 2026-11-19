package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"

	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
	"github.com/openai/openai-go/v3/packages/param"
	"github.com/openai/openai-go/v3/responses"
)

type OpenAIProvider struct {
	client openai.Client
	model  string
}

const defaultBaseURL = "https://coding.gaiaworks.net/openai/v1"

var _ StreamingProvider = (*OpenAIProvider)(nil)

func NewOpenAIProvider(model string) *OpenAIProvider {
	key := strings.TrimSpace(os.Getenv("CODEX_API_KEY"))
	if key == "" {
		panic("请配置CODEX_API_KEY环境变量")
	}
	return newOpenAIProvider(model, key, nil)
}

func newOpenAIProvider(model, key string, httpClient option.HTTPClient) *OpenAIProvider {
	// Responses.New 会在 BaseURL 后追加 /responses，因此 BaseURL 必须指向
	// 中转站的 API 根路径（例如 https://coding.gaiaworks.net/openai/v1）。
	baseURL := strings.TrimSpace(os.Getenv("CODEX_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	// 兼容之前版本给出的旧地址，避免用户 shell 中已有的环境变量继续
	// 把请求发到 /v1/responses，而 curl 使用的是 /openai/v1/responses。
	if baseURL == "https://coding.gaiaworks.net/v1" {
		baseURL = defaultBaseURL
	}

	model = strings.TrimSpace(model)
	if model == "" {
		model = strings.TrimSpace(os.Getenv("CODEX_MODEL"))
	}
	if model == "" {
		model = "gpt-6.1-sol"
	}

	clientOptions := []option.RequestOption{
		option.WithAPIKey(key),
		option.WithBaseURL(baseURL),
	}
	if httpClient != nil {
		clientOptions = append(clientOptions, option.WithHTTPClient(httpClient))
	}

	return &OpenAIProvider{
		client: openai.NewClient(clientOptions...),
		model:  model,
	}
}

func (p *OpenAIProvider) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	resp, err := p.client.Responses.New(ctx, p.buildParams(msgs, availableTools))
	if err != nil {
		return nil, wrapAPIError(err)
	}
	return messageFromResponse(resp), nil
}

// GenerateStream 使用 Responses API 的 SSE 流式接口。文本增量实时交给 onDelta，
// ToolCalls 和 Usage 统一从 response.completed 事件携带的完整 Response 中解析，
// 保证与 Generate 的结果一致。
//
// 如果流在收到任何事件之前就失败（例如中转站不支持 SSE、连接被拒绝），
// 自动退回一次非流式 Generate；一旦已经收到事件，就不再重试，直接返回错误。
func (p *OpenAIProvider) GenerateStream(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition, onDelta StreamHandler) (*schema.Message, error) {
	msg, receivedEvents, err := p.generateStream(ctx, msgs, availableTools, onDelta)
	if err == nil || receivedEvents || ctx.Err() != nil {
		return msg, err
	}
	log.Printf("[Provider] ⚠️ 流式请求未收到任何事件 (%v)，退回非流式请求\n", err)
	msg, err = p.Generate(ctx, msgs, availableTools)
	if err != nil {
		return nil, err
	}
	// 非流式结果一次性交给 onDelta，调用方的展示逻辑保持一致。
	if onDelta != nil && msg.Content != "" {
		onDelta(msg.Content)
	}
	return msg, nil
}

func (p *OpenAIProvider) generateStream(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition, onDelta StreamHandler) (msg *schema.Message, receivedEvents bool, err error) {
	stream := p.client.Responses.NewStreaming(ctx, p.buildParams(msgs, availableTools))
	defer stream.Close()

	for stream.Next() {
		receivedEvents = true
		event := stream.Current()
		switch event.Type {
		case "response.output_text.delta":
			if onDelta != nil && event.Delta != "" {
				onDelta(event.Delta)
			}
		case "response.completed":
			completed := event.AsResponseCompleted()
			return messageFromResponse(&completed.Response), true, nil
		case "response.failed":
			failed := event.AsResponseFailed()
			return nil, true, fmt.Errorf("OpenAI Responses API 流式响应失败: %s (code=%s)",
				failed.Response.Error.Message, failed.Response.Error.Code)
		case "response.incomplete":
			incomplete := event.AsResponseIncomplete()
			return nil, true, fmt.Errorf("OpenAI Responses API 流式响应未完成: %s",
				incomplete.Response.IncompleteDetails.Reason)
		case "error":
			errEvent := event.AsError()
			return nil, true, fmt.Errorf("OpenAI Responses API 流式错误: %s (code=%s)", errEvent.Message, errEvent.Code)
		}
	}
	if err := stream.Err(); err != nil {
		return nil, receivedEvents, wrapAPIError(err)
	}
	return nil, receivedEvents, errors.New("OpenAI Responses API 流式响应在 response.completed 之前结束")
}

func (p *OpenAIProvider) buildParams(msgs []schema.Message, availableTools []schema.ToolDefinition) responses.ResponseNewParams {
	params := responses.ResponseNewParams{
		Model: sharedResponsesModel(p.model),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responseInputItems(msgs),
		},
	}
	if tools := responseTools(availableTools); len(tools) > 0 {
		params.Tools = tools
	}
	return params
}

func wrapAPIError(err error) error {
	var apiErr *openai.Error
	if errors.As(err, &apiErr) {
		detail := strings.TrimSpace(apiErr.Message)
		if detail == "" {
			detail = strings.TrimSpace(apiErr.RawJSON())
		}
		return fmt.Errorf(
			"OpenAI Responses API 请求失败 (%d): %s (type=%s code=%s param=%s): %w",
			apiErr.StatusCode, detail, apiErr.Type, apiErr.Code, apiErr.Param, err,
		)
	}
	return fmt.Errorf("OpenAI Responses API 请求失败: %w", err)
}

func messageFromResponse(resp *responses.Response) *schema.Message {
	resultMsg := &schema.Message{
		Role:    schema.RoleAssistant,
		Content: resp.OutputText(),
	}
	if resp.Usage.InputTokens > 0 || resp.Usage.OutputTokens > 0 {
		resultMsg.Usage = &schema.Usage{
			PromptTokens:     int(resp.Usage.InputTokens),
			CompletionTokens: int(resp.Usage.OutputTokens),
		}
	}
	for _, output := range resp.Output {
		if output.Type != "function_call" {
			continue
		}
		call := output.AsFunctionCall()
		resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
			// Responses API 的工具结果通过 call_id 关联，而不是 output item 的 id。
			ID:        call.CallID,
			Name:      call.Name,
			Arguments: []byte(call.Arguments),
		})
	}
	return resultMsg
}

// sharedResponsesModel is kept as a small conversion point because the SDK
// aliases ResponsesModel to string today, while keeping provider code independent
// of that generated type detail.
func sharedResponsesModel(model string) string {
	return model
}

func responseInputItems(msgs []schema.Message) responses.ResponseInputParam {
	input := make(responses.ResponseInputParam, 0, len(msgs))
	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleSystem))
		case schema.RoleUser:
			if msg.ToolCallID != "" {
				item := responses.ResponseInputItemParamOfFunctionCallOutput(msg.Content)
				item.OfFunctionCallOutput.CallID = param.NewOpt(msg.ToolCallID)
				input = append(input, item)
			} else {
				input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleUser))
			}
		case schema.RoleAssistant:
			if msg.Content != "" {
				input = append(input, responses.ResponseInputItemParamOfMessage(msg.Content, responses.EasyInputMessageRoleAssistant))
			}
			for _, call := range msg.ToolCalls {
				input = append(input, responses.ResponseInputItemParamOfFunctionCall(string(call.Arguments), call.ID, call.Name))
			}
		}
	}
	return input
}

func responseTools(toolDefs []schema.ToolDefinition) []responses.ToolUnionParam {
	tools := make([]responses.ToolUnionParam, 0, len(toolDefs))
	for _, toolDef := range toolDefs {
		tools = append(tools, responses.ToolUnionParam{
			OfFunction: &responses.FunctionToolParam{
				Name:        toolDef.Name,
				Description: param.NewOpt(toolDef.Description),
				Parameters:  inputSchemaMap(toolDef.InputSchema),
				Strict:      param.NewOpt(false),
			},
		})
	}
	return tools
}

func inputSchemaMap(schemaValue interface{}) map[string]any {
	if m, ok := schemaValue.(map[string]interface{}); ok {
		return m
	}
	b, err := json.Marshal(schemaValue)
	if err != nil {
		return nil
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return nil
	}
	return m
}
