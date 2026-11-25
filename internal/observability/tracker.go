// internal/observability/tracker.go
package observability

import (
	"context"
	"log"
	"time"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
)

// PricingModel 定义了不同大模型的计费标准 (单位: 美元/1M Tokens)
// 为了演示，这里硬编码了当前市面上几个主流模型的官方大致定价。
var PricingModel = map[string]struct {
	InputPrice  float64
	OutputPrice float64
}{
	"gpt-6.1-sol": {InputPrice: 0.15, OutputPrice: 0.15}, // 这里假定的大模型价格(每百万Token，tk)
}

// CostTracker 是一个包装了真实 LLMProvider 的装饰器中间件
type CostTracker struct {
	nextProvider provider.LLMProvider
	modelName    string
	session      *cctx.Session // 当前所属的会话 (用于累加总成本)
}

// NewCostTracker 构造函数：接收一个现有的 Provider，返回一个被监控的 Provider
func NewCostTracker(next provider.LLMProvider, modelName string, session *cctx.Session) *CostTracker {
	return &CostTracker{
		nextProvider: next,
		modelName:    modelName,
		session:      session,
	}
}

// Generate 实现了 LLMProvider 接口！这意味着它可以被无缝注入到 Main Loop 中。
func (t *CostTracker) Generate(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition) (*schema.Message, error) {
	startTime := time.Now()
	respMsg, err := t.nextProvider.Generate(ctx, msgs, availableTools)
	return t.record(respMsg, err, time.Since(startTime))
}

// GenerateStream 让 CostTracker 同时满足 provider.StreamingProvider。
// 底层支持流式就透传；不支持则退回 Generate，并把完整内容一次性交给 onDelta。
func (t *CostTracker) GenerateStream(ctx context.Context, msgs []schema.Message, availableTools []schema.ToolDefinition, onDelta provider.StreamHandler) (*schema.Message, error) {
	startTime := time.Now()
	streaming, ok := t.nextProvider.(provider.StreamingProvider)
	if !ok {
		respMsg, err := t.nextProvider.Generate(ctx, msgs, availableTools)
		if err == nil && respMsg != nil && respMsg.Content != "" && onDelta != nil {
			onDelta(respMsg.Content)
		}
		return t.record(respMsg, err, time.Since(startTime))
	}
	respMsg, err := streaming.GenerateStream(ctx, msgs, availableTools, onDelta)
	return t.record(respMsg, err, time.Since(startTime))
}

func (t *CostTracker) record(respMsg *schema.Message, err error, latency time.Duration) (*schema.Message, error) {
	// 如果报错了，只打印报错时间，不计费
	if err != nil {
		log.Printf("[Tracker] ❌ API 调用失败，耗时: %v\n", latency)
		return respMsg, err
	}

	// 解析 Token 并计算成本
	if respMsg.Usage != nil {
		promptTokens := respMsg.Usage.PromptTokens
		completionTokens := respMsg.Usage.CompletionTokens

		var cost float64
		if price, exists := PricingModel[t.modelName]; exists {
			// 计算美元花费 = (输入Tokens * 输入单价 + 输出Tokens * 输出单价) / 1000000
			cost = (float64(promptTokens)*price.InputPrice + float64(completionTokens)*price.OutputPrice) / 1000000.0
		}

		log.Printf("[Tracker] 📊 API 调用完成 | 耗时: %v | 输入: %d tk | 输出: %d tk | 花费: ¥%.6f\n",
			latency, promptTokens, completionTokens, cost)

		// 将账单累加到当前的 Session 中，供人类后续随时查询
		if t.session != nil {
			t.session.RecordUsage(promptTokens, completionTokens, cost)
			log.Printf("[Tracker] 💰 当前会话 (%s) 累计花费: ¥%.6f\n", t.session.ID, t.session.TotalCostCNY)
		}
	} else {
		log.Printf("[Tracker] ⚠️ API 调用完成，但未返回 Usage 数据 | 耗时: %v\n", latency)
	}

	return respMsg, nil
}

var _ provider.StreamingProvider = (*CostTracker)(nil)
