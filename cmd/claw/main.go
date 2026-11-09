// cmd/claw/main.go
package main

import (
	"context"
	"log"
	"os"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/engine"
	"github.com/aka-xubo/my-tiny-claw/internal/observability"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

func main() {
	provider := provider.NewOpenAIProvider(os.Getenv("CODEX_MODEL"))
	workDir, _ := os.Getwd()
	workDir += "/workspace"
	sessionID := "test_trace_001"
	sess := cctx.GlobalSessionMgr.GetOrCreate(sessionID, workDir)
	skillLoader := cctx.NewSkillLoader(workDir)
	registry := tools.NewRegistry()
	registry.Register(tools.NewBashTool(workDir))
	registry.Register(tools.NewWriteFileTool(workDir))
	registry.Register(tools.NewReadSkillTool(skillLoader))
	eng := engine.NewAgentEngine(provider, registry, false, false)
	reporter := engine.NewTerminalReporter()
	prompt := ` 为了加快执行速度，请你在一轮回复中，【同时并行】完成以下两件事：
	 1. 使用 bash 工具执行 'sleep 2 && echo "系统环境检查完毕"'
	 2. 使用 write_file 工具，在当前目录下创建一个 'trace_test.md'，内容写上 "测试并发的写入"。
	  请确保你是分别调用两个不同的工具，不要试图把它们合并成一个命令！ `
	log.Println("\n>>> 🚀 启动trace测试...")
	sess.Append(schema.Message{Role: schema.RoleUser, Content: prompt})
	err := eng.Run(context.Background(), sess, reporter)
	if err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}

func testcost(provider provider.LLMProvider, workDir string) {
	sessionID := "test_observability_001"
	sess := cctx.GlobalSessionMgr.GetOrCreate(sessionID, workDir)
	// 2. 核心拼装：用 Tracker 将真实的大脑包裹起来
	trackedProvider := observability.NewCostTracker(provider, "gpt-6.1-sol", sess)
	registry := tools.NewRegistry()
	registry.Register(tools.NewBashTool(workDir))
	registry.Register(tools.NewReadSkillTool(cctx.NewSkillLoader(workDir)))
	// 3. 将被包裹的 Provider 注入给 Engine (Engine 毫不知情)
	eng := engine.NewAgentEngine(trackedProvider, registry, false, false)
	reporter := engine.NewTerminalReporter()
	prompt := `请用 bash 帮我用 date 命令查一下现在的时间。`
	log.Println("\n>>> 🚀 启动带仪表盘的可观测性测试...")
	sess.Append(schema.Message{Role: schema.RoleUser, Content: prompt})
	err := eng.Run(context.Background(), sess, reporter)
	if err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
	log.Printf("\n================ 财务报表 ================\n")
	log.Printf("会话 ID: %s\n", sess.ID)
	log.Printf("总消耗 Input Tokens: %d\n", sess.TotalPromptTokens)
	log.Printf("总消耗 Output Tokens: %d\n", sess.TotalCompletionTokens)
	log.Printf("总计费用 (CNY): ¥%.6f\n", sess.TotalCostCNY)
	log.Printf("==========================================\n")
}

func testSubAgent(engine *engine.AgentEngine, reporter engine.Reporter, workDir string) {
	// 【防御沙箱】为子智能体准备受限的只读注册表
	readOnlyRegistry := tools.NewRegistry()
	readOnlyRegistry.Register(tools.NewReadFileTool(workDir))
	readOnlyRegistry.Register(tools.NewBashTool(workDir)) // 允许简单的 grep 等搜索操作
	readOnlyRegistry.Register(tools.NewReadSkillTool(cctx.NewSkillLoader(workDir)))

	// 为主智能体准备全功能注册表
	mainRegistry := tools.NewRegistry()
	mainRegistry.Register(tools.NewReadFileTool(workDir))
	mainRegistry.Register(tools.NewWriteFileTool(workDir))
	mainRegistry.Register(tools.NewBashTool(workDir))
	mainRegistry.Register(tools.NewEditFileTool(workDir))
	mainRegistry.Register(tools.NewReadSkillTool(cctx.NewSkillLoader(workDir)))

	// 【核心装配】：将带有 Engine 引用和只读 Registry 的 Subagent 工具注册进主线
	mainRegistry.Register(tools.NewSubagentTool(engine, readOnlyRegistry, reporter))

	sessionID := "test_subagent_001"
	sess := cctx.GlobalSessionMgr.GetOrCreate(sessionID, workDir)

	prompt := `
    我需要你在这个遗留项目里，找到那个“核心密码”。
    为了防止污染主上下文，请你务必派出子智能体（spawn_subagent）去执行探索任务。
    你可以让子智能体使用 bash 去查找当前目录（及其所有子目录）下名为 config.txt 的文件。
    子智能体拿到密码向你汇报后，请你亲自使用 write_file 工具，将密码写在根目录的 answer.txt 里。
    `

	log.Println("\n>>> 🚀 启动多智能体协同测试...")
	sess.Append(schema.Message{Role: schema.RoleUser, Content: prompt})

	err := engine.Run(context.Background(), sess, reporter)
	if err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}
