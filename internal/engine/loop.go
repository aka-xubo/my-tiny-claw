package engine

import (
	"context"
	"fmt"
	"log"
	"strings"
	"sync"

	cctx "github.com/aka-xubo/my-tiny-claw/internal/context"
	"github.com/aka-xubo/my-tiny-claw/internal/observability"
	"github.com/aka-xubo/my-tiny-claw/internal/provider"
	"github.com/aka-xubo/my-tiny-claw/internal/schema"
	"github.com/aka-xubo/my-tiny-claw/internal/tools"
)

type AgentEngine struct {
	provider       provider.LLMProvider
	registry       tools.Registry
	EnableThinking bool
	PlanMode       bool // 【新增】暴露给外部的计划模式开关
	compactor      *cctx.Compactor
	recovery       *cctx.RecoveryManager
	injector       *ReminderInjector // 【新增】提醒注入器
}

func NewAgentEngine(p provider.LLMProvider, r tools.Registry, planMode bool, enableThinking bool) *AgentEngine {
	return &AgentEngine{
		provider:       p,
		registry:       r,
		EnableThinking: enableThinking,
		PlanMode:       planMode,
		// 【初始化压缩器】：为了便于今天的极端测试，将水位线阈值设积极（例如 3000 字符），
		// 并保护最近的 6 条消息（大约两轮 Turn 的交互）
		compactor: cctx.NewCompactor(20000, 6),
		recovery:  cctx.NewRecoveryManager(),
		injector:  NewReminderInjector(),
	}
}

func (e *AgentEngine) Run(ctx context.Context, session *cctx.Session, reporter Reporter) error {
	log.Printf("[Engine] 唤醒会话 [%s]，锁定工作区: %s\n", session.ID, session.WorkDir)

	// 【埋点 1】：开启 Root Span，记录整个任务的生命周期
	ctx, rootSpan := observability.StartSpan(ctx, "Agent.Run")
	rootSpan.AddAttribute("SessionID", session.ID)
	rootSpan.AddAttribute("WorkDir", session.WorkDir)
	defer func() {
		rootSpan.EndSpan()
		_ = observability.ExportTraceToFile(rootSpan, session.WorkDir, session.ID)
		log.Printf("📊 [Tracing] 本次任务的执行回放链路已保存至工作区的 .claw/traces 目录下\n")
	}()
	composer := cctx.NewPromptComposer(session.WorkDir, e.PlanMode)
	systemMsg := composer.Build()
	turnCount := 0
	for {
		turnCount++
		// 【埋点 2】：记录单次 Turn 循环
		turnCtx, turnSpan := observability.StartSpan(ctx, fmt.Sprintf("Turn-%d", turnCount))
		defer turnSpan.EndSpan() // 利用 defer，哪怕遇到了 break 或 error 也会计算耗时
		availableTools := e.registry.GetAvailableTools()

		// 1. 从 Session 提取出近期的 Working Memory (例如最近 20 条，给压缩器留下充足的判断空间)
		workingMemory := session.GetWorkingMemory(20)

		var contextHistory []schema.Message
		contextHistory = append(contextHistory, systemMsg)
		contextHistory = append(contextHistory, workingMemory...)

		// 2. 【核心注入点】: 在向 Provider 发起推理前，过一遍内存压缩器！
		// 无论你带出了多少上下文，如果字符总数超标，早期日志将被掩码化，超大日志将被掐头去尾
		compactedContext := e.compactor.Compact(contextHistory)
		// 记录发给模型的实际上下文大小，非常有助于排查幻觉
		turnSpan.AddAttribute("context_message_count", len(compactedContext))

		// 3. 后续的 Provider.Generate 全面使用被保护过的新鲜上下文 (compactedContext)
		// ================= Phase 1: Thinking =================
		var currentTurnThinkingContent string
		if e.EnableThinking {
			if reporter != nil {
				reporter.OnThinking(turnCtx)
			}
			// 【埋点 3】：记录 Thinking 调用 thinkCtx,
			thinkCtx, thinkSpan := observability.StartSpan(turnCtx, "LLM.Thinking")
			thinkResp, err := e.provider.Generate(thinkCtx, compactedContext, nil)
			thinkSpan.EndSpan() // 结束思考跨度
			if err != nil {
				return fmt.Errorf("Thinking 阶段失败: %w", err)
			}
			if thinkResp.Content != "" {
				//session.Append(*thinkResp)
				currentTurnThinkingContent = thinkResp.Content
				compactedContext = append(compactedContext, *thinkResp)
			}
		}

		// ================= Phase 2: Action =================
		actCtx, actSpan := observability.StartSpan(turnCtx, "LLM.Action")
		actionResp, err := e.provider.Generate(actCtx, compactedContext, availableTools)
		actSpan.EndSpan()
		if err != nil {
			return fmt.Errorf("Action 阶段失败: %w", err)
		}
		finalAssistantMsg := schema.Message{
			Role:      schema.RoleAssistant,
			Content:   strings.TrimSpace(currentTurnThinkingContent + "\n" + actionResp.Content),
			ToolCalls: actionResp.ToolCalls,
		}

		// 写入 Session（硬盘/全量内存）的永远是全量的真实响应，不受 Compact 影响！
		// Compact 只作用于本轮发给大模型的那个临时 Context。
		session.Append(finalAssistantMsg)
		compactedContext = append(compactedContext, *actionResp)

		if actionResp.Content != "" && reporter != nil {
			reporter.OnMessage(ctx, actionResp.Content)
		}

		// ... (执行工具与并发逻辑，与上一讲完全一致) ...
		if len(actionResp.ToolCalls) == 0 {
			break
		}

		loadSkillTurn := hasReadSkillCall(actionResp.ToolCalls)
		observationMsgs := make([]schema.Message, len(actionResp.ToolCalls))
		var wg sync.WaitGroup

		// 用于收集本轮执行的最后一个工具，供 Reminder 探测器分析
		// 如果并发调用了多个工具，可以逐个分析或仅分析报错的那个。这里简化为取第一个
		var lastToolCall schema.ToolCall
		var lastToolResult schema.ToolResult

		for i, toolCall := range actionResp.ToolCalls {
			wg.Add(1)
			go func(idx int, call schema.ToolCall) {
				defer wg.Done()
				if reporter != nil {
					reporter.OnToolCall(ctx, call.Name, string(call.Arguments))
				}

				var result schema.ToolResult
				if loadSkillTurn && call.Name != "read_skill" {
					result = schema.ToolResult{
						ToolCallID: call.ID,
						Output:     "本轮正在加载技能正文，其他执行型工具尚未执行。请在下一轮阅读 read_skill 的结果后重新发起该工具调用。",
						IsError:    true,
					}
				} else {
					result = e.registry.Execute(turnCtx, call)
				}
				finalOutput := result.Output
				if result.IsError {
					// 发生错误，交由 RecoveryManager 诊断并注入 救援指南
					finalOutput = e.recovery.AnalyzeAndInject(call.Name, result.Output)
					log.Printf(" -> [Go-%d] ❌ 注入救援指南: %s\n", idx, finalOutput)
				} else {
					log.Printf(" -> [Go-%d] ✅ 工具执行成功 (返回 %d 字节)\n", idx, len(result.Output))
				}
				if reporter != nil {
					displayOutput := finalOutput
					if len(displayOutput) > 200 {
						displayOutput = displayOutput[:200] + "... (已截断)"
					}
					reporter.OnToolResult(ctx, call.Name, displayOutput, result.IsError)
				}
				observationMsgs[idx] = schema.Message{
					Role:       schema.RoleUser,
					Content:    finalOutput,
					ToolCallID: call.ID,
					Kind:       messageKindForTool(call.Name),
				}
				// 捕获状态供外部探测器使用
				if idx == 0 {
					lastToolCall = call
					lastToolResult = result
				}
			}(i, toolCall)
		}
		wg.Wait()

		// 将全量观测结果持久化到 Session 中
		session.Append(observationMsgs...)

		//核心防线：在准备进入下一轮之前，进行死循环探测！
		reminderMsg := e.injector.CheckAndInject(lastToolCall, lastToolResult)
		if reminderMsg != nil {
			// 如果触发了干预规则，将这条严厉的提醒作为 User 消息，强制追加到 Session 的最末尾！
			// 大模型在下一轮被唤醒时，第一眼就会看到这句话，从而打破局部执念。
			session.Append(*reminderMsg)
		}
	}

	return nil
}

func hasReadSkillCall(calls []schema.ToolCall) bool {
	for _, call := range calls {
		if call.Name == "read_skill" {
			return true
		}
	}
	return false
}

func messageKindForTool(toolName string) string {
	if toolName == "read_skill" {
		return schema.MessageKindSkillBody
	}
	return ""
}

// internal/engine/loop.go (续加在末尾)

// RunSub 是专为 Subagent 拉起的一次性受限循环。
// 它不依赖外部 Session，打完就跑。
// Reporter：为了让用户在终端看到子智能体的工作轨迹，将主线程的 Reporter 透传进来，并打上特殊标记。
func (e *AgentEngine) RunSub(ctx context.Context, taskPrompt string, readOnlyRegistry tools.Registry, reporter any) (string, error) {

	// 【核心优化】：在 System Prompt 中严厉警告它必须使用工具！
	contextHistory := []schema.Message{
		{
			Role: schema.RoleSystem,
			Content: `你是一个专门负责深度探索的探路者 (Explorer Subagent)。
你的任务是根据主架构师的指令，在当前工作区内仔细阅读代码、查阅日志，搜集足够的信息。

【核心纪律】
1. 你必须、且只能依靠内置工具（如 bash 的 find/grep，或 read_file）去寻找答案。绝对不允许凭空捏造或猜测！
2. 如果你没有找到确切的答案，你必须继续使用工具深入搜索。
3. 当且仅当你找到了确切的线索后，停止调用工具，直接输出一段纯文本作为你的终极汇报。主架构师会根据你的汇报来做下一步决策。`,
		},
		{
			Role:    schema.RoleUser,
			Content: taskPrompt,
		},
	}

	// 限制子智能体最多只能跑 10 个 Turn，防止它自己卡死
	const maxSubTurns = 10
	turnCount := 0

	for {
		turnCount++
		if turnCount > maxSubTurns {
			return "", fmt.Errorf("子智能体探索过于深入，超过 %d 轮被强制召回，请主 Agent 给它更明确的指令", maxSubTurns)
		}

		// 【驾驭底线】：子智能体仅能获取传入的只读工具注册表
		availableTools := readOnlyRegistry.GetAvailableTools()

		compactedContext := e.compactor.Compact(contextHistory)

		// 子任务要求急速响应，强制关闭主体的慢思考，直接预测行动
		actionResp, err := e.provider.Generate(ctx, compactedContext, availableTools)
		if err != nil {
			return "", fmt.Errorf("子智能体推理失败: %w", err)
		}

		contextHistory = append(contextHistory, *actionResp)

		// 【核心退出条件】：子智能体一旦不调用工具了，说明它做好了总结汇报
		if len(actionResp.ToolCalls) == 0 {
			// 直接将它的这段汇报内容剥离出来返回给上层
			return actionResp.Content, nil
		}

		// 执行只读工具的并发循环
		observationMsgs := make([]schema.Message, len(actionResp.ToolCalls))
		var wg sync.WaitGroup

		for i, toolCall := range actionResp.ToolCalls {
			wg.Add(1)
			go func(idx int, call schema.ToolCall) {
				defer wg.Done()

				// 【可视化的关键】：让终端用户看到 Subagent 正在干嘛
				var r Reporter
				if reporter != nil {
					r = reporter.(Reporter)
					r.OnToolCall(ctx, fmt.Sprintf("[Subagent] %s", call.Name), string(call.Arguments))
				}

				result := readOnlyRegistry.Execute(ctx, call)

				finalOutput := result.Output
				if result.IsError {
					finalOutput = e.recovery.AnalyzeAndInject(call.Name, result.Output)
				}

				if reporter != nil {
					display := finalOutput
					if len(display) > 200 {
						display = display[:200] + "... (已截断)"
					}
					r.OnToolResult(ctx, fmt.Sprintf("[Subagent] %s", call.Name), display, result.IsError)
				}

				observationMsgs[idx] = schema.Message{
					Role:       schema.RoleUser,
					Content:    finalOutput,
					ToolCallID: call.ID,
					Kind:       messageKindForTool(call.Name),
				}
			}(i, toolCall)
		}

		wg.Wait()
		contextHistory = append(contextHistory, observationMsgs...)
	}
}
