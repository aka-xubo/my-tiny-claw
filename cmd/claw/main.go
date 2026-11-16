// cmd/claw/main.go
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/engine"
	"github.com/aka-xubo/my-tiny-claw/internal/mcpclient"
	"github.com/aka-xubo/my-tiny-claw/internal/observability"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

func main() {
	promptPtr := flag.String("prompt", "", "要交给 Agent 执行的任务描述")
	workDirPtr := flag.String("dir", ".", "Agent 运行的工作区目录路径 (默认为当前目录)")
	sessionPtr := flag.String("session", "cli_default_session", "指定会话 ID，支持断点续传")
	flag.Parse()
	if *promptPtr == "" {
		fmt.Println("用法: go-tiny-claw -prompt \"你的任务描述\" [-dir /path/to/workdir] [-session session_id]")
		os.Exit(1)
	}
	// 解析工作区绝对路径
	workDir, err := filepath.Abs(*workDirPtr)
	if err != nil {
		log.Fatalf("解析工作区路径失败: %v", err)
	}
	fmt.Println("==================================================")
	fmt.Printf("🚀 启动 go-tiny-claw CLI 引擎...\n")
	fmt.Printf("📁 锁定工作区: %s\n", workDir)
	fmt.Println("==================================================")

	// 2. 初始化核心基础服务
	var realProvider provider.LLMProvider
	modelName := "gpt-6.1-sol"
	realProvider = provider.NewOpenAIProvider(os.Getenv("CODEX_MODEL"))
	skillLoader := cctx.NewSkillLoader(workDir)
	// 获取持久化Session
	sess := cctx.GlobalSessionMgr.GetOrCreate(*sessionPtr, workDir)
	//【监控装配】：用 Cost Tracker 将真实大脑包裹起来
	trackedProvider := observability.NewCostTracker(realProvider, modelName, sess)
	// 3. 初始化工具与执行层
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(workDir))
	registry.Register(tools.NewWriteFileTool(workDir))
	registry.Register(tools.NewBashTool(workDir))
	registry.Register(tools.NewEditFileTool(workDir))
	registry.Register(tools.NewReadSkillTool(skillLoader))

	// MCP 配置只从用户目录加载；配置不存在时保持现有内置工具行为。
	mcpConfigPath, err := mcpclient.DefaultConfigPath()
	if err != nil {
		log.Fatalf("解析 MCP 配置路径失败: %v", err)
	}
	mcpConfig, err := mcpclient.LoadConfig(mcpConfigPath)
	if err != nil {
		log.Fatalf("加载 MCP 配置失败: %v", err)
	}
	mcpManager, err := mcpclient.NewManager(mcpConfig, mcpclient.Options{})
	if err != nil {
		log.Fatalf("初始化 MCP Manager 失败: %v", err)
	}
	mcpManager.Connect(context.Background())
	for _, tool := range mcpManager.Tools() {
		registry.Register(tool)
	}
	// 在 CLI 模式下，默认开启 YOLO 模式（全权信任本地执行），
	// 因此这里暂时不挂载 Feishu 审批 Middleware。
	// 4. 初始化核心引擎 (组装器内部会自动加载 Composer, Compactor, Recovery, Reminders)
	// 开启 EnableThinking = true
	eng := engine.NewAgentEngine(trackedProvider, registry, true, false)
	// 【追踪装配】：初始化链路追踪 Root Span
	ctx, rootSpan := observability.StartSpan(context.Background(), "CLI.TaskRun")
	rootSpan.AddAttribute("Prompt", *promptPtr)
	// 5. 初始化彩色终端输出器
	reporter := engine.NewTerminalReporter()
	fmt.Printf("\n🎯 收到任务: %s\n\n", *promptPtr)
	// 将用户的 Prompt 压入 Session 记忆
	sess.Append(schema.Message{Role: schema.RoleUser, Content: *promptPtr})
	// 6. 启动 Main Loop！
	err = eng.Run(ctx, sess, reporter)
	closeErr := mcpManager.Close()
	rootSpan.EndSpan()
	_ = observability.ExportTraceToFile(rootSpan, workDir, sess.ID)
	if closeErr != nil {
		log.Printf("关闭 MCP Session 失败: %v", closeErr)
	}
	if err != nil {
		log.Printf("\n💥 引擎运行崩溃: %v", err)
		os.Exit(1)
	}
	fmt.Println("\n==================================================")
	fmt.Printf("✨ 任务圆满结束。总耗时: %v\n", time.Since(rootSpan.StartTime))
	fmt.Printf("💰 Session 累计消耗: $%.6f | Token: Input %d, Output %d\n", sess.TotalCostCNY, sess.TotalPromptTokens, sess.TotalCompletionTokens)
	fmt.Println("==================================================")
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
