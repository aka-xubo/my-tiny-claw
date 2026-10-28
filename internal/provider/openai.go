package provider

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	params := responses.ResponseNewParams{
		Model: sharedResponsesModel(p.model),
		Input: responses.ResponseNewParamsInputUnion{
			OfInputItemList: responseInputItems(msgs),
		},
	}
	if tools := responseTools(availableTools); len(tools) > 0 {
		params.Tools = tools
	}

	resp, err := p.client.Responses.New(ctx, params)
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			detail := strings.TrimSpace(apiErr.Message)
			if detail == "" {
				detail = strings.TrimSpace(apiErr.RawJSON())
			}
			return nil, fmt.Errorf(
				"OpenAI Responses API 请求失败 (%d): %s (type=%s code=%s param=%s): %w",
				apiErr.StatusCode, detail, apiErr.Type, apiErr.Code, apiErr.Param, err,
			)
		}
		return nil, fmt.Errorf("OpenAI Responses API 请求失败: %w", err)
	}

	resultMsg := &schema.Message{
		Role:    schema.RoleAssistant,
		Content: resp.OutputText(),
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

	return resultMsg, nil
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
