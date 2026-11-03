// cmd/claw/main.go
package main

import (
	"context"
	"log"
	"os"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/engine"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

func main() {

	llmProvider := provider.NewOpenAIProvider(os.Getenv("CODEX_MODEL"))
	workDir, _ := os.Getwd()
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(workDir))
	registry.Register(tools.NewWriteFileTool(workDir))
	registry.Register(tools.NewBashTool(workDir))

	sessionID := "test_oom_protection_001"
	sess := cctx.GlobalSessionMgr.GetOrCreate(sessionID, workDir)

	// 发起一个会导致读取大文件的恶意任务
	prompt := `
    请帮我执行以下三个步骤：
    1. 使用 bash 执行 echo "开始排查日志"
    2. 使用 read_file 工具读取当前目录下的巨大文件 mock_log.txt
    3. 使用 bash 执行 date 命令获取当前时间，并告诉我任务全部完成。
    `
	sess.Append(schema.Message{Role: schema.RoleUser, Content: prompt})
	// 实例化引擎 (关闭思考模式以提速)
	eng := engine.NewAgentEngine(llmProvider, registry, false)
	reporter := engine.NewTerminalReporter()
	err := eng.Run(context.Background(), sess, reporter)
	if err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}
