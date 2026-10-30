// cmd/claw/main.go
package main

import (
	"context"
	"log"
	"os"

	"github.com/aka-xubo/my-tiny-claw/internal/engine"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

func main() {

	workDir, _ := os.Getwd()
	llmProvider := provider.NewOpenAIProvider(os.Getenv("CODEX_MODEL"))
	registry := tools.NewRegistry()
	registry.Register(tools.NewReadFileTool(workDir))
	registry.Register(tools.NewWriteFileTool(workDir))
	registry.Register(tools.NewBashTool(workDir))
	eng := engine.NewEngine(llmProvider, registry, workDir, false)
	prompt := ` 请帮我执行以下操作：
1. 用 bash 查看一下我当前电脑的 Go 版本。
2. 帮我写一个简单的 helloworld.go 文件，输出 "Hello, my-tiny-claw!"。
3. 用 bash 编译并运行这个 go 文件，确认它能正常工作。 `
	err := eng.Run(context.Background(), prompt)
	if err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}
