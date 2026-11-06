// cmd/claw/main.go
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/engine"
	"github.com/aka-xubo/my-tiny-claw/internal/feishu"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
	"github.com/larksuite/oapi-sdk-go/v3/core/httpserverext"
)

func main() {

	// 通过命令行参数接收用户的 prompt
	/*promptPtr := flag.String("prompt", "", "要交给 Agent 执行的任务描述")
	flag.Parse()
	if *promptPtr == "" {
		fmt.Println("用法: go run cmd/claw/main.go -prompt \"你的任务指令\"")
		os.Exit(1)
	}*/
	llmProvider := provider.NewOpenAIProvider(os.Getenv("CODEX_MODEL"))
	workDir, _ := os.Getwd()
	workDir += "/workspace"
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(workDir))
	registry.Register(tools.NewWriteFileTool(workDir))
	registry.Register(tools.NewBashTool(workDir))
	registry.Register(tools.NewEditFileTool(workDir))

	sessionID := "test_command_intercept_001"
	sess := cctx.GlobalSessionMgr.GetOrCreate(sessionID, workDir)
	sess.Append(schema.Message{Role: schema.RoleUser, Content: ""})
	eng := engine.NewAgentEngine(llmProvider, registry, false, false)
	bot := feishu.NewFeishuBot(eng, sess)

	handler := httpserverext.NewEventHandlerFunc(bot.GetEventDispatcher())

	// 【核心注入】注册安全拦截 Middleware
	registry.Use(func(ctx context.Context, call schema.ToolCall) (bool, string) {
		argsStr := string(call.Arguments)

		// 检查是否命中高危特征库
		if feishu.IsDangerousCommand(call.Name, argsStr) {
			taskID := call.ID // 使用大模型生成的唯一 ToolCallID 作为 TaskID

			// 挂起当前协程，发送消息给飞书，死死等待人类的审批！
			allowed, reason := feishu.GlobalApprovalMgr.WaitForApproval(ctx, taskID, call.Name, argsStr, bot.Reporter())

			if !allowed {
				return false, reason // 拒绝，将理由传回给大模型
			}
			return true, "" // 同意，放行底层工具
		}

		// 没命中黑名单，直接 YOLO 放行
		return true, ""
	})

	// 3. 注册路由并启动 HTTP 服务
	http.HandleFunc("/webhook/event", handler)

	port := ":48080"
	log.Printf("🚀 go-tiny-claw 飞书服务端已启动，正在监听 %s 端口\n", port)

	err := http.ListenAndServe(port, nil)
	if err != nil {
		log.Fatalf("服务器启动失败: %v", err)
	}
}
