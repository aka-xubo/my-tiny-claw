// cmd/claw/main.go
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/engine"
	"github.com/aka-xubo/my-tiny-claw/internal/fshelper"
	"github.com/aka-xubo/my-tiny-claw/internal/mcpclient"
	"github.com/aka-xubo/my-tiny-claw/internal/observability"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/sandbox"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

func main() {
	if err := runCLI(); err != nil {
		log.Printf("💥 %v", err)
		os.Exit(1)
	}
}

func runCLI() error {
	if len(os.Args) > 1 && os.Args[1] == "__fs-helper" {
		return fshelper.Serve(os.Stdin, os.Stdout)
	}
	if len(os.Args) > 1 && os.Args[1] == "sandbox" {
		return runSandboxCommand(os.Args[2:])
	}
	promptPtr := flag.String("prompt", "", "要交给 Agent 执行的任务描述")
	workDirPtr := flag.String("dir", ".", "Agent 运行的工作区目录路径 (默认为当前目录)")
	sessionPtr := flag.String("session", "cli_default_session", "指定会话 ID，支持断点续传")
	permissionsPtr := flag.String("permissions", "", "默认权限档（默认读取配置，未配置时为 :workspace）")
	approvalPtr := flag.String("approval", "", "审批策略：untrusted、on-request、never（默认读取配置）")
	thinkingPtr := flag.Bool("thinking", false, "开启慢思考 (Thinking) 阶段，流式模式下会实时展示思考内容")
	streamPtr := flag.Bool("stream", true, "实时流式输出模型回复（-stream=false 关闭）")
	flag.Parse()
	// 解析工作区绝对路径
	workDir, err := filepath.Abs(*workDirPtr)
	if err != nil {
		return fmt.Errorf("解析工作区路径失败: %w", err)
	}
	fmt.Println("==================================================")
	var inputReader *bufio.Reader
	if *promptPtr == "" {
		inputReader = bufio.NewReader(os.Stdin)
		exists, trusted, trustErr := sandbox.ProjectConfigStatus(workDir)
		if trustErr != nil {
			return fmt.Errorf("检查项目配置可信状态失败: %w", trustErr)
		}
		if exists && !trusted {
			content, readErr := os.ReadFile(filepath.Join(workDir, ".claw", "config.toml"))
			if readErr != nil {
				return fmt.Errorf("读取待确认配置失败: %w", readErr)
			}
			fmt.Printf("项目配置内容：\n%s\n", content)
			fmt.Printf("发现未信任的项目配置 %s/.claw/config.toml。信任并加载？[y/N] ", workDir)
			line, readErr := inputReader.ReadString('\n')
			if readErr != nil && !errors.Is(readErr, io.EOF) {
				return readErr
			}
			if strings.EqualFold(strings.TrimSpace(line), "y") {
				if trustErr := sandbox.TrustProject(workDir); trustErr != nil {
					return fmt.Errorf("记录项目可信状态失败: %w", trustErr)
				}
				fmt.Println("项目配置已信任。")
			} else {
				fmt.Println("项目配置未加载。")
			}
		}
	} else if exists, trusted, statusErr := sandbox.ProjectConfigStatus(workDir); statusErr == nil && exists && !trusted {
		fmt.Fprintln(os.Stderr, "警告：项目配置未被信任，非交互模式不会加载该配置。")
	}
	config, err := sandbox.LoadConfig(workDir)
	if err != nil {
		return fmt.Errorf("初始化权限档失败: %w", err)
	}
	permissionName := *permissionsPtr
	if permissionName == "" {
		permissionName = config.DefaultPermissions
	}
	approvalPolicy := *approvalPtr
	if approvalPolicy == "" {
		approvalPolicy = string(config.ApprovalPolicy)
	}
	profile, ok := config.Profiles[permissionName]
	if !ok {
		return fmt.Errorf("未知权限档 %q，可选值：%s", permissionName, strings.Join(sortedProfileNames(config.Profiles), ", "))
	}
	var approver sandbox.Approver = sandbox.DenyApprover{}
	if *promptPtr == "" {
		approver = sandbox.NewTerminalApprover(inputReader, os.Stdout)
	}
	sandboxSession, err := sandbox.NewSession(profile, sandbox.ApprovalPolicy(approvalPolicy), approver, []string{workDir})
	if err != nil {
		return fmt.Errorf("初始化沙箱会话失败: %w", err)
	}
	sandboxSession.SetEnvironmentPolicy(config.EnvExclude, config.EnvKeep)
	defer sandboxSession.Close()
	if terminal, ok := approver.(*sandbox.TerminalApprover); ok {
		defer terminal.Close()
	}
	fmt.Printf("🚀 启动 go-tiny-claw CLI 引擎...\n")
	fmt.Printf("📁 锁定工作区: %s\n", workDir)
	fmt.Println("==================================================")

	// 主进程收到 SIGINT/SIGTERM 时取消 Engine，并由 TaskManager 统一清理
	// 仍在运行的后台会话。
	runCtx, cancelCLI := context.WithCancel(context.Background())
	defer cancelCLI()
	stopSignal := func() {}
	if *promptPtr != "" {
		runCtx, stopSignal = signal.NotifyContext(runCtx, os.Interrupt, syscall.SIGTERM)
	}
	defer stopSignal()
	taskManager := tools.NewTaskManager()
	defer taskManager.Shutdown()

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
	registry.Register(tools.NewReadFileToolWithSandbox(workDir, sandboxSession))
	registry.Register(tools.NewWriteFileToolWithSandbox(workDir, sandboxSession))
	registry.Register(tools.NewManagedBashToolWithSandbox(workDir, taskManager, sandboxSession))
	registry.Register(tools.NewEditFileToolWithSandbox(workDir, sandboxSession))
	registry.Register(tools.NewReadSkillTool(skillLoader))

	// MCP 配置只从用户目录加载；配置不存在时保持现有内置工具行为。
	mcpConfigPath, err := mcpclient.DefaultConfigPath()
	if err != nil {
		return fmt.Errorf("解析 MCP 配置路径失败: %w", err)
	}
	mcpConfig, err := mcpclient.LoadConfig(mcpConfigPath)
	if err != nil {
		return fmt.Errorf("加载 MCP 配置失败: %w", err)
	}
	mcpManager, err := mcpclient.NewManager(mcpConfig, mcpclient.Options{})
	if err != nil {
		return fmt.Errorf("初始化 MCP Manager 失败: %w", err)
	}
	mcpManager.Connect(runCtx)
	for _, tool := range mcpManager.Tools() {
		registry.Register(tool)
	}
	// 文件和 bash 权限由 sandbox.Session 统一审批与执行。
	// 4. 初始化核心引擎 (组装器内部会自动加载 Composer, Compactor, Recovery, Reminders)
	// PlanMode 默认开启；Thinking 由 -thinking 控制
	eng := engine.NewAgentEngine(trackedProvider, registry, true, *thinkingPtr)
	eng.Stream = *streamPtr
	eng.SandboxDescription = sandboxSession.Description()
	// 【追踪装配】：初始化链路追踪 Root Span
	ctx, rootSpan := observability.StartSpan(runCtx, "CLI.TaskRun")
	rootSpan.AddAttribute("Prompt", *promptPtr)
	// 5. 初始化彩色终端输出器
	reporter := engine.NewTerminalReporter()
	// 日志写入前先结束流式输出停在行中间的那一行，避免日志粘在回复末尾
	log.SetOutput(reporter.LogWriter(os.Stderr))
	if *promptPtr == "" {
		rootSpan.EndSpan()
		defer mcpManager.Close()
		if err := runREPL(runCtx, approver.(*sandbox.TerminalApprover), eng, reporter, sess, sandboxSession, config.Profiles, taskManager); err != nil {
			return err
		}
		return nil
	}
	fmt.Printf("\n🎯 收到任务: %s\n\n", *promptPtr)
	// 将用户的 Prompt 压入 Session 记忆
	sess.Append(schema.Message{Role: schema.RoleUser, Content: *promptPtr})
	// 6. 启动 Main Loop！
	err = eng.Run(ctx, sess, reporter)
	stopSignal() // A second interrupt during cleanup uses the default behavior.
	// 先停止后台会话，再关闭 MCP 和导出本次 Trace。
	taskManager.Shutdown()
	closeErr := mcpManager.Close()
	rootSpan.EndSpan()
	_ = observability.ExportTraceToFile(rootSpan, workDir, sess.ID)
	if closeErr != nil {
		log.Printf("关闭 MCP Session 失败: %v", closeErr)
	}
	if err != nil {
		return fmt.Errorf("引擎运行崩溃: %w", err)
	}
	fmt.Println("\n==================================================")
	fmt.Printf("✨ 任务圆满结束。总耗时: %v\n", time.Since(rootSpan.StartTime))
	fmt.Printf("💰 Session 累计消耗: $%.6f | Token: Input %d, Output %d\n", sess.TotalCostCNY, sess.TotalPromptTokens, sess.TotalCompletionTokens)
	fmt.Println("==================================================")
	return nil
}

func runREPL(runCtx context.Context, terminalApprover *sandbox.TerminalApprover, eng *engine.AgentEngine, reporter engine.Reporter, sess *cctx.Session, sandboxSession *sandbox.Session, profiles map[string]*sandbox.Profile, taskManager *tools.TaskManager) error {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signals)
	fmt.Println("进入交互模式。输入 /permissions、/approval 或 /exit。")
	for {
		fmt.Print("claw> ")
		promptCtx, cancelPrompt := context.WithCancel(runCtx)
		type inputResult struct {
			line string
			err  error
		}
		next := make(chan inputResult, 1)
		go func() { line, err := terminalApprover.NextLine(promptCtx); next <- inputResult{line, err} }()
		var input inputResult
		select {
		case input = <-next:
		case <-signals:
			cancelPrompt()
			return nil
		case <-runCtx.Done():
			cancelPrompt()
			return runCtx.Err()
		}
		cancelPrompt()
		line, err := input.line, input.err
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		switch {
		case line == "/exit":
			return nil
		case line == "/permissions":
			current := sandboxSession.Profile()
			for name, profile := range profiles {
				mark := " "
				if current != nil && current.Name == profile.Name {
					mark = "*"
				}
				fmt.Printf("%s %s — %s\n", mark, name, profile.Description)
			}
			continue
		case strings.HasPrefix(line, "/permissions "):
			name := strings.TrimSpace(strings.TrimPrefix(line, "/permissions "))
			profile, ok := profiles[name]
			if !ok {
				fmt.Printf("未知权限档: %s\n", name)
				continue
			}
			items, listErr := taskManager.List(sess.ID, sess.WorkDir)
			if listErr != nil {
				return listErr
			}
			for _, item := range items {
				if item.Status == tools.TaskRunning && item.Profile != profile.Name {
					fmt.Printf("后台会话 %s（%s）继续使用 %s。输入 stop 停止，或 keep 保留 > ", item.SessionID, item.Command, item.Profile)
					for {
						answer, err := terminalApprover.NextLine(runCtx)
						if err != nil {
							return err
						}
						answer = strings.TrimSpace(answer)
						if answer == "keep" {
							break
						}
						if answer == "stop" {
							if err := taskManager.Stop(runCtx, item.SessionID, sess.ID, sess.WorkDir); err != nil {
								return err
							}
							break
						}
						fmt.Print("请输入 stop 或 keep > ")
					}
				}
			}
			if err := sandboxSession.SetProfile(profile); err != nil {
				fmt.Printf("切换权限档失败: %v\n", err)
			} else {
				fmt.Printf("当前权限档：%s\n", name)
			}
			continue
		case strings.HasPrefix(line, "/approval "):
			policy := sandbox.ApprovalPolicy(strings.TrimSpace(strings.TrimPrefix(line, "/approval ")))
			if err := sandboxSession.SetPolicy(policy); err != nil {
				fmt.Printf("切换审批策略失败: %v\n", err)
			} else {
				fmt.Printf("当前审批策略：%s\n", policy)
			}
			continue
		}
		sess.Append(schema.Message{Role: schema.RoleUser, Content: line})
		eng.SandboxDescription = sandboxSession.Description()
		ctx, span := observability.StartSpan(runCtx, "CLI.REPLTurn")
		span.AddAttribute("Prompt", line)
		terminalApprover.BeginTurn()
		turnCtx, cancelTurn := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() { done <- eng.Run(turnCtx, sess, reporter) }()
		var turnErr error
		select {
		case turnErr = <-done:
		case sig := <-signals:
			cancelTurn()
			terminalApprover.CancelTurnApprovals()
			select {
			case turnErr = <-done:
			case <-signals:
				terminalApprover.EndTurn()
				span.EndSpan()
				return nil
			}
			if sig == syscall.SIGTERM {
				terminalApprover.EndTurn()
				span.EndSpan()
				return nil
			}
		case <-runCtx.Done():
			cancelTurn()
			<-done
			terminalApprover.EndTurn()
			span.EndSpan()
			return runCtx.Err()
		}
		cancelTurn()
		terminalApprover.EndTurn()
		if turnCtx.Err() != nil && turnErr != nil {
			fmt.Println("本轮已取消或执行失败：", turnErr)
		}

		span.EndSpan()
	}
}

func runSandboxCommand(args []string) error {
	flagArgs := args
	commandArgs := []string{}
	for i, arg := range args {
		if arg == "--" {
			flagArgs = args[:i]
			commandArgs = args[i+1:]
			break
		}
	}
	flags := flag.NewFlagSet("claw sandbox", flag.ContinueOnError)
	flags.SetOutput(os.Stderr)
	permissions := flags.String("permissions", "", "权限档名称（默认读取配置，未配置时为 :workspace）")
	workDir := flags.String("C", ".", "命令工作目录")
	printPolicy := flags.Bool("print-policy", false, "打印生成的 Seatbelt 规则")
	if err := flags.Parse(flagArgs); err != nil {
		return err
	}
	if len(commandArgs) == 0 && len(flags.Args()) > 0 {
		commandArgs = flags.Args()
	}

	absWorkDir, err := filepath.Abs(*workDir)
	if err != nil {
		return fmt.Errorf("解析工作目录失败: %w", err)
	}
	config, err := sandbox.LoadConfig(absWorkDir)
	if err != nil {
		return err
	}
	permissionName := *permissions
	if permissionName == "" {
		permissionName = config.DefaultPermissions
	}
	profile, ok := config.Profiles[permissionName]
	if !ok {
		return fmt.Errorf("未知权限档 %q，可选值：%s", permissionName, strings.Join(sortedProfileNames(config.Profiles), ", "))
	}
	if *printPolicy {
		if profile.FullAccess {
			fmt.Println("full access")
		} else {
			policy, err := sandbox.GenerateSBPL(profile)
			if err != nil {
				return err
			}
			fmt.Print(policy)
		}
		if len(commandArgs) == 0 {
			return nil
		}
	}
	if len(commandArgs) == 0 {
		return fmt.Errorf("缺少命令，用法：claw sandbox [-permissions NAME] [-C DIR] [--print-policy] -- CMD [ARGS...]")
	}
	commandPath, err := exec.LookPath(commandArgs[0])
	if err != nil {
		return fmt.Errorf("找不到命令 %q: %w", commandArgs[0], err)
	}
	commandArgs[0], err = filepath.Abs(commandPath)
	if err != nil {
		return fmt.Errorf("解析命令路径失败: %w", err)
	}
	session, err := sandbox.NewSession(profile, config.ApprovalPolicy, sandbox.DenyApprover{}, []string{absWorkDir})
	if err != nil {
		return err
	}
	defer session.Close()
	session.SetEnvironmentPolicy(config.EnvExclude, config.EnvKeep)
	cmd, cleanup, err := session.Spawn(context.Background(), commandArgs, absWorkDir, profile)
	if err != nil {
		return err
	}
	defer cleanup()
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return fmt.Errorf("命令退出，状态码 %d", exitErr.ExitCode())
		}
		return err
	}
	return nil
}

func sortedProfileNames(profiles map[string]*sandbox.Profile) []string {
	names := make([]string, 0, len(profiles))
	for name := range profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
	profiles, err := sandbox.BuiltinProfiles(workDir)
	if err != nil {
		log.Printf("初始化子 Agent 权限档失败: %v", err)
		return
	}
	readOnlySession, err := sandbox.NewSession(profiles[":read-only"], sandbox.ApprovalNever, sandbox.DenyApprover{}, []string{workDir})
	if err != nil {
		log.Printf("初始化子 Agent 沙箱失败: %v", err)
		return
	}
	defer readOnlySession.Close()
	// 【防御沙箱】为子智能体准备受限的只读注册表
	readOnlyRegistry := tools.NewRegistry()
	readOnlyRegistry.Register(tools.NewReadFileToolWithSandbox(workDir, readOnlySession))
	readOnlyRegistry.Register(tools.NewReadOnlyBashToolWithSandbox(workDir, readOnlySession)) // 允许简单的 grep 等搜索操作
	readOnlyRegistry.Use(tools.NewReadOnlyBashMiddleware())
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

	err = engine.Run(context.Background(), sess, reporter)
	if err != nil {
		log.Fatalf("引擎运行崩溃: %v", err)
	}
}
