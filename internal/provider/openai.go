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
	"github.com/openai/openai-go/v3/shared"
)

type OpenAIProvider struct {
	client openai.Client
	model  string
}

const defaultBaseURL = "https://coding.gaiaworks.net/v1"

func NewOpenAIProvider(model string) *OpenAIProvider {
	key := strings.TrimSpace(os.Getenv("CODEX_API_KEY"))
	if key == "" {
		panic("请配置CODEX_API_KEY环境变量")
	}
	return newOpenAIProvider(model, key, nil)
}

func newOpenAIProvider(model, key string, httpClient option.HTTPClient) *OpenAIProvider {

	// Chat.Completions.New 会在 BaseURL 后追加 /chat/completions。
	// 因此 BaseURL 必须指向 API 根路径（例如 .../v1），不能填完整接口地址。
	baseURL := strings.TrimSpace(os.Getenv("CODEX_BASE_URL"))
	if baseURL == "" {
		baseURL = strings.TrimSpace(os.Getenv("OPENAI_BASE_URL"))
	}
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")

	model = strings.TrimSpace(model)
	if model == "" {
		model = strings.TrimSpace(os.Getenv("CODEX_MODEL"))
	}
	if model == "" {
		model = "gpt-5.6-sol"
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
	var openaiMsgs []openai.ChatCompletionMessageParamUnion

	// 1. 翻译上下文消息
	for _, msg := range msgs {
		switch msg.Role {
		case schema.RoleSystem:
			openaiMsgs = append(openaiMsgs, openai.SystemMessage(msg.Content))

		case schema.RoleUser:
			if msg.ToolCallID != "" {
				// 注意：v3 新版参数顺序是 (content, toolCallID)
				openaiMsgs = append(openaiMsgs, openai.ToolMessage(msg.Content, msg.ToolCallID))
			} else {
				openaiMsgs = append(openaiMsgs, openai.UserMessage(msg.Content))
			}

		case schema.RoleAssistant:
			astParam := openai.ChatCompletionAssistantMessageParam{}

			if msg.Content != "" {
				astParam.Content = openai.ChatCompletionAssistantMessageParamContentUnion{
					OfString: openai.String(msg.Content),
				}
			}

			// 【重要】如果历史包含 ToolCalls，必须原样放回，以维系大模型的逻辑链
			if len(msg.ToolCalls) > 0 {
				var toolCalls []openai.ChatCompletionMessageToolCallUnionParam
				for _, tc := range msg.ToolCalls {
					// OfFunction 对应 GetFunction()，字段类型严格要求为指针
					toolCalls = append(toolCalls, openai.ChatCompletionMessageToolCallUnionParam{
						OfFunction: &openai.ChatCompletionMessageFunctionToolCallParam{
							ID:   tc.ID,
							Type: "function",
							Function: openai.ChatCompletionMessageFunctionToolCallFunctionParam{
								Name:      tc.Name,
								Arguments: string(tc.Arguments),
							},
						},
					})
				}
				astParam.ToolCalls = toolCalls
			}

			openaiMsgs = append(openaiMsgs, openai.ChatCompletionMessageParamUnion{
				OfAssistant: &astParam,
			})
		}
	}

	// 2. 翻译工具定义 (v3 新 API 特性适配)
	var openaiTools []openai.ChatCompletionToolUnionParam
	for _, toolDef := range availableTools {
		var params shared.FunctionParameters

		// 尝试直接断言，如果不成功则通过 JSON 往返序列化来保证类型匹配
		if m, ok := toolDef.InputSchema.(map[string]interface{}); ok {
			params = shared.FunctionParameters(m)
		} else {
			// fallback：JSON 往返序列化
			b, _ := json.Marshal(toolDef.InputSchema)
			_ = json.Unmarshal(b, &params)
		}

		openaiTools = append(openaiTools, openai.ChatCompletionFunctionTool(
			shared.FunctionDefinitionParam{
				Name:        toolDef.Name,
				Description: openai.String(toolDef.Description),
				Parameters:  params,
			},
		))
	}

	// 3. 构建请求并发送
	params := openai.ChatCompletionNewParams{
		Model:    p.model,
		Messages: openaiMsgs,
	}

	// 【慢思考机制支撑】仅当 availableTools 存在时才挂载 Tools
	if len(openaiTools) > 0 {
		params.Tools = openaiTools
	}

	resp, err := p.client.Chat.Completions.New(ctx, params)
	if err != nil {
		var apiErr *openai.Error
		if errors.As(err, &apiErr) {
			detail := strings.TrimSpace(apiErr.Message)
			if detail == "" {
				detail = strings.TrimSpace(apiErr.RawJSON())
			}
			return nil, fmt.Errorf(
				"OpenAI API 请求失败 (%d): %s (type=%s code=%s param=%s): %w",
				apiErr.StatusCode, detail, apiErr.Type, apiErr.Code, apiErr.Param, err,
			)
		}
		return nil, fmt.Errorf("OpenAI API 请求失败: %w", err)
	}
	if len(resp.Choices) == 0 {
		return nil, fmt.Errorf("API 返回了空的 Choices")
	}

	// 4. 将 API Response 反向翻译为内部 schema.Message
	choice := resp.Choices[0].Message
	resultMsg := &schema.Message{
		Role:    schema.RoleAssistant,
		Content: choice.Content,
	}

	for _, tc := range choice.ToolCalls {
		if tc.Type == "function" {
			resultMsg.ToolCalls = append(resultMsg.ToolCalls, schema.ToolCall{
				ID:        tc.ID,
				Name:      tc.Function.Name,
				Arguments: []byte(tc.Function.Arguments), // 提取 JSON 字符串字节
			})
		}
	}

	return resultMsg, nil
}
