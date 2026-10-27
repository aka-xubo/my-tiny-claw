package engine

import (
	"context"
	"fmt"
	"log"

	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

type AgentEngine struct {
	Provider       provider.LLMProvider
	Registry       tools.Registry
	WorkDir        string //工作区
	EnableThinking bool
}

func NewEngine(provider provider.LLMProvider, registry tools.Registry, workDir string, enableThinking bool) *AgentEngine {
	return &AgentEngine{
		Provider:       provider,
		Registry:       registry,
		WorkDir:        workDir,
		EnableThinking: enableThinking,
	}
}

func (e *AgentEngine) Run(ctx context.Context, userPormpt string) error {
	log.Printf("[Engine] 引擎启动，锁定工作区: %s\n", e.WorkDir)
	log.Printf("[Engine] 慢思考模式 (Thinking Phase): %v\n", e.EnableThinking)
	turnCount := 0
	contextHistory := []schema.Message{
		{Role: schema.RoleSystem, Content: "You are my-tiny-claw, an expert coding assistant. You have full access to tools in the workspace."},
		{Role: schema.RoleUser, Content: userPormpt},
	}
	for {
		turnCount++
		log.Printf("第 %d 轮开始", turnCount)
		availableTools := e.Registry.GetAvailableTools()
		//如果开启了深度思考
		//不放入工具
		//强制规划
		if e.EnableThinking {
			log.Println("[Engine][Phase 1] 剥夺工具访问权，强制进入慢思考与规划阶段...")
			thinkingResp, err := e.Provider.Generate(ctx, contextHistory, nil)
			if err != nil {
				return fmt.Errorf("模型回复失败:%s", err.Error())
			}
			if thinkingResp.Content != "" {
				contextHistory = append(contextHistory, *thinkingResp)
			}
		}
		// Phase 2: 行动阶段 (Action) - 恢复工具，顺着规划执行
		log.Println("[Engine][Phase 2] 恢复工具挂载，等待模型采取行动...") // 此时的 contextHistory 中已经包含了上一阶段模型自己的 Thinking Trace。
		// 模型会顺着自己的逻辑，结合恢复的 availableTools 发起精准的工具调用。
		actionResp, err := e.Provider.Generate(ctx, contextHistory, availableTools)
		if err != nil {
			return fmt.Errorf("Action 阶段生成失败: %w", err)
		}
		// 将模型的响应完整追加到上下文历史中
		contextHistory = append(contextHistory, *actionResp)
		if actionResp.Content != "" {
			fmt.Printf("🤖 [对外回复]: %s\n", actionResp.Content)
		}
		if len(actionResp.ToolCalls) == 0 {
			fmt.Println("[Engine] 本次任务完成，退出循环")
			break
		}
		for _, tool := range actionResp.ToolCalls {
			log.Printf("执行工具:%s,参数:%s", tool.ID, tool.Arguments)
			result := e.Registry.Execute(ctx, tool)
			if result.IsError {
				log.Printf(" -> ❌ 工具执行失败:%s", result.Output)
			} else {
				log.Printf(" -> ✅ 工具执行成功 (返回 %d 字节)\n", len(result.Output))
			}
			observationMsg := schema.Message{
				Role:    schema.RoleUser,
				Content: result.Output,
			}
			contextHistory = append(contextHistory, observationMsg)
		}
	}
	return nil
}
