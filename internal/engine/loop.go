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
	provider provider.LLMProvider
	registry tools.Registry
	workDir  string //工作区
}

func NewEngine(provider provider.LLMProvider, registry tools.Registry, workDir string) *AgentEngine {
	return &AgentEngine{
		provider: provider,
		registry: registry,
		workDir:  workDir,
	}
}

func (e *AgentEngine) Run(ctx context.Context, userPormpt string) error {
	turnCount := 0
	contextHistory := []schema.Message{
		{Role: schema.RoleSystem, Content: "You are go-tiny-claw, an expert coding assistant. You have full access to tools in the workspace."},
		{Role: schema.RoleUser, Content: userPormpt},
	}
	for {
		turnCount++
		log.Printf("第 %d 轮开始", turnCount)
		availableTools := e.registry.GetAvailableTools()
		log.Println("modle is thinking...")
		msg, err := e.provider.Generate(ctx, contextHistory, availableTools)
		// 将模型的响应完整追加到上下文历史中
		contextHistory = append(contextHistory, *msg)
		if err != nil {
			return fmt.Errorf("模型回复失败:%s", err.Error())
		}
		if msg.Content != "" {
			fmt.Printf("模型：%s", msg.Content)
		}
		if len(msg.ToolCalls) == 0 {
			fmt.Println("[Engine] 本次任务完成，退出循环")
			break
		}
		for _, tool := range msg.ToolCalls {
			log.Printf("执行工具:%s,参数:%s", tool.ID, tool.Arguments)
			result := e.registry.Execute(ctx, tool)
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
