package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"pi-golang/internal/entity"
	"pi-golang/internal/infrastructure"
	cliterm "pi-golang/internal/terminal"
	"pi-golang/internal/usecase"
)

// Streams keeps process I/O explicit so command execution can be tested
// without replacing global stdout/stderr. Production main wires these to the
// process streams; protocol drivers can provide their own sinks later.
type Streams struct {
	In  io.Reader
	Out io.Writer
	Err io.Writer
}

func main() {
	os.Exit(realMain(os.Args[1:], Streams{
		In:  os.Stdin,
		Out: os.Stdout,
		Err: os.Stderr,
	}))
}

func realMain(args []string, streams Streams) int {
	streams = withDefaultStreams(streams)
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if len(args) == 0 {
		// 终端中直接运行 `go run .` 时进入持续对话；管道或脚本场景
		// 保留原有的一次性默认行为，避免无界等待 stdin。
		if isInteractiveReader(streams.In) {
			args = []string{"run", "--interactive"}
		} else {
			args = []string{"run"}
		}
	}

	switch args[0] {
	case "version", "-v", "--version":
		fmt.Fprintln(streams.Out, "pi-agent 0.1.0 (minimal)")
		return 0
	case "providers":
		fmt.Fprintln(streams.Out, strings.Join(infrastructure.SupportedProviders(), "\n"))
		return 0
	case "setup":
		return cmdSetup(streams)
	case "help", "-h", "--help":
		fmt.Fprint(streams.Out, strings.TrimSpace(helpText)+"\n")
		return 0
	case "run":
		return cmdRun(ctx, args[1:], streams)
	default:
		fmt.Fprintln(streams.Err, "未知命令:", args[0])
		fmt.Fprintln(streams.Err, "运行 `pi-agent help` 查看用法")
		return 2
	}
}

func withDefaultStreams(streams Streams) Streams {
	if streams.In == nil {
		streams.In = os.Stdin
	}
	if streams.Out == nil {
		streams.Out = os.Stdout
	}
	if streams.Err == nil {
		streams.Err = os.Stderr
	}
	return streams
}

func isInteractiveReader(input io.Reader) bool {
	file, ok := input.(*os.File)
	return ok && isInteractive(file)
}

func cmdRun(ctx context.Context, args []string, streams Streams) int {
	streams = withDefaultStreams(streams)
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	fs.SetOutput(streams.Err)
	var prompt string
	var interactive bool
	var debug bool
	var provider string
	var apiKey string
	var baseURL string
	var model string
	var auditFile string
	var auditContent string
	var logFile string
	var outputFile string
	var noTools bool
	var toolsMode string
	var taskProfile string
	var sessionFile string
	var orchestration string
	var maxPlanTasks int
	var protocolAttempts int
	var workerAttempts int
	var maxReplans int
	fs.StringVar(&prompt, "prompt", "", "发送给 agent 的用户 prompt")
	fs.BoolVar(&interactive, "interactive", false, "进入持续多轮对话模式；也可直接运行 `go run .`")
	fs.BoolVar(&debug, "debug", false, "打印本次提示词与运行配置（可能包含敏感内容）")
	fs.StringVar(&provider, "provider", "", "本次使用的 provider，覆盖 LLM_PROVIDER")
	fs.StringVar(&apiKey, "api-key", "", "本次使用的 API key，覆盖环境变量")
	fs.StringVar(&baseURL, "base-url", "", "本次 API base URL，覆盖 LLM_BASE_URL")
	fs.StringVar(&model, "model", "", "本次使用的模型，覆盖 LLM_MODEL")
	fs.StringVar(&auditFile, "audit-file", "", "JSONL 审计文件路径，记录每轮 LLM 输入输出")
	fs.StringVar(&auditContent, "audit-content", "", "审计内容模式：redacted（默认）或 full")
	fs.StringVar(&logFile, "log-file", "", "结构化运行日志文件；交互模式默认不把日志写入终端")
	fs.StringVar(&outputFile, "output", "", "把最终回答作为 HTML 原子写入文件")
	fs.BoolVar(&noTools, "no-tools", false, "本次运行不向 LLM 注册工具（适合纯文本/HTML 生成任务）")
	fs.StringVar(&toolsMode, "tools", "auto", "工具模式：auto（默认）、enabled、disabled")
	fs.StringVar(&taskProfile, "task-profile", "auto", "任务 profile：auto、generation、agent-readonly、agent-mutation")
	fs.StringVar(&sessionFile, "session", "", "会话状态 JSON 文件；重复使用以延续历史并启用跨进程压缩")
	fs.StringVar(&orchestration, "orchestration", "single", "执行模式：single（单会话）或 plan（规划任务图并逐任务使用新会话）")
	fs.IntVar(&maxPlanTasks, "max-plan-tasks", 32, "plan 模式允许的任务总数上限（含验证器追加的修复任务）")
	fs.IntVar(&protocolAttempts, "protocol-attempts", 2, "plan 模式中规划和验证结构化结果的最大尝试次数")
	fs.IntVar(&workerAttempts, "worker-attempts", 1, "plan 模式中每个任务的最大执行次数；大于 1 可能重复副作用")
	fs.IntVar(&maxReplans, "max-replans", 2, "plan 模式验证失败后允许追加修复任务的次数")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	orchestration = strings.ToLower(strings.TrimSpace(orchestration))
	interactiveMode := interactive
	if !interactiveMode && prompt == "" && isInteractiveReader(streams.In) {
		interactiveMode = true
	}
	if interactiveMode {
		if orchestration != "single" {
			fmt.Fprintln(streams.Err, "参数错误: --interactive 只能与 --orchestration=single 一起使用")
			return 2
		}
		if strings.TrimSpace(outputFile) != "" {
			fmt.Fprintln(streams.Err, "参数错误: --output 不能用于多轮对话，请使用单次 --prompt 模式")
			return 2
		}
	} else if prompt == "" {
		prompt = "hello"
	}
	if orchestration != "single" && orchestration != "plan" {
		fmt.Fprintln(streams.Err, "参数错误: --orchestration 必须是 single 或 plan")
		return 2
	}
	if maxPlanTasks < 1 || maxPlanTasks > 100 {
		fmt.Fprintln(streams.Err, "参数错误: --max-plan-tasks 必须在 1 到 100 之间")
		return 2
	}
	if protocolAttempts < 1 || protocolAttempts > 10 {
		fmt.Fprintln(streams.Err, "参数错误: --protocol-attempts 必须在 1 到 10 之间")
		return 2
	}
	if workerAttempts < 1 || workerAttempts > 10 {
		fmt.Fprintln(streams.Err, "参数错误: --worker-attempts 必须在 1 到 10 之间")
		return 2
	}
	if maxReplans < 0 || maxReplans > 10 {
		fmt.Fprintln(streams.Err, "参数错误: --max-replans 必须在 0 到 10 之间")
		return 2
	}
	if orchestration == "plan" && strings.TrimSpace(sessionFile) != "" {
		fmt.Fprintln(streams.Err, "参数错误: plan 模式为每个角色创建独立会话，不能同时使用 --session")
		return 2
	}

	cfg, err := infrastructure.Load()
	if err != nil {
		fmt.Fprintln(streams.Err, "加载配置:", err)
		return 1
	}
	cfg = cfg.WithLLMOverrides(provider, apiKey, baseURL, model)
	if cfg.NeedsLLMSetup() {
		if !isInteractiveReader(streams.In) {
			printSetupHint(cfg, streams.Err)
			return 1
		}
		cfg, err = setupLLMInteractively(cfg, streams.In, streams.Err)
		if err != nil {
			fmt.Fprintln(streams.Err, "配置 LLM:", err)
			return 1
		}
	}
	cfg = cfg.WithAuditOverrides(auditFile, auditContent)
	if cfg.Agent.Timeout > 0 && !interactiveMode {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Agent.Timeout)
		defer cancel()
	}
	var logOutput io.Writer = streams.Err
	if interactiveMode {
		// 交互终端只显示提示词、助手答案和用户可读错误。结构化日志
		// 默认静默，避免 stderr 与正在编辑的输入行互相覆盖。
		logOutput = io.Discard
	}
	var logHandle *os.File
	if strings.TrimSpace(logFile) != "" {
		logHandle, err = os.OpenFile(logFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			fmt.Fprintln(streams.Err, "打开运行日志:", err)
			return 1
		}
		defer func() { _ = logHandle.Close() }()
		logOutput = logHandle
	}
	g, err := infrastructure.BuildWithConfigAndOutput(cfg, logOutput)
	if err != nil {
		fmt.Fprintln(streams.Err, "启动:", err)
		return 1
	}
	defer func() {
		if closeErr := g.Close(); closeErr != nil {
			fmt.Fprintln(streams.Err, "关闭审计日志:", closeErr)
		}
	}()
	toolsEnabled, err := resolveToolsMode(toolsMode, noTools, outputFile, prompt)
	if err != nil {
		fmt.Fprintln(streams.Err, "参数错误:", err)
		return 2
	}
	profile, err := resolveTaskProfile(taskProfile, outputFile, prompt)
	if err != nil {
		fmt.Fprintln(streams.Err, "参数错误:", err)
		return 2
	}
	var agentOptions []entity.Option
	if strings.TrimSpace(sessionFile) != "" {
		session, loadErr := infrastructure.LoadConversationSession(sessionFile)
		if loadErr != nil {
			fmt.Fprintln(streams.Err, "加载会话:", loadErr)
			return 1
		}
		agentOptions = append(agentOptions, entity.WithConversationSession(session))
	}
	if !toolsEnabled {
		agentOptions = append(agentOptions, entity.WithTools(nil))
	}
	agent := g.NewAgent(ctx, agentOptions...)
	if debug {
		// Debug 输出写诊断流；交互模式默认静默，--log-file 可单独保存。
		fmt.Fprintf(logOutput, "debug: prompt id=%s version=%s sha256=%s chars=%d\n",
			g.Prompt.ID, g.Prompt.Version, g.Prompt.Hash, len(g.Prompt.Content))
		fmt.Fprintf(logOutput, "debug: model=%q provider=%q base_url=%q max_tokens=%d max_iterations=%d timeout=%s tools=%d tools_mode=%s\n",
			agent.Config().Model, g.Config.LLM.Provider, g.Config.LLM.BaseURL, agent.Config().MaxTokens, agent.Config().MaxIterations, agent.Config().Timeout, len(agent.Tools()), toolsMode)
		for _, tool := range agent.Tools() {
			info := tool.Info()
			_, _ = fmt.Fprintf(logOutput, "debug: tool name=%q description=%q schema=%s\n",
				info.Name, info.Description, string(info.InputSchema))
		}
		fmt.Fprintln(logOutput, "debug: system prompt follows")
		fmt.Fprintln(logOutput, g.Prompt.Content)
	}
	g.Logger.Info(ctx, "已构建 agent", "name", agent.Config().Name,
		"has_llm", agent.LLM() != nil, "has_memory", agent.Memory() != nil,
		"plugins", len(agent.Plugins()), "tools", len(agent.Tools()))
	if interactiveMode {
		return runInteractive(ctx, g, agent, profile, resolvedToolsMode(toolsMode, toolsEnabled), sessionFile, streams)
	}

	runInput := usecase.RunInput{
		UserPrompt:    prompt,
		TaskProfile:   profile,
		ToolsMode:     resolvedToolsMode(toolsMode, toolsEnabled),
		PromptVersion: g.Prompt.Version,
	}
	var finalAnswer string
	var iterations int
	var elapsed string
	var usage entity.TokenUsage
	var usageReported bool
	var compactions int
	var completed bool
	var stopReason usecase.RunStopReason
	var conversations int
	var agentTurns int
	var planTasks int
	var replans int
	var orchestrationID string
	if orchestration == "single" {
		out, runErr := g.RunUsecase.Execute(ctx, agent, runInput)
		err = runErr
		finalAnswer = out.FinalAnswer
		iterations = out.Iterations
		elapsed = out.Elapsed.Round(out.Elapsed.Truncate(1).Truncate(100)).String()
		usage = out.Usage
		usageReported = out.UsageReported
		compactions = out.Compactions
		stopReason = out.StopReason
		completed = out.StopReason == usecase.RunStopFinalAnswer
		conversations = 1
		agentTurns = 1
	} else {
		allTools := agent.Tools()
		factory := func(role usecase.OrchestrationRole) *entity.Agent {
			roleOptions := append([]entity.Option{}, agentOptions...)
			roleOptions = append(roleOptions, entity.WithTools(orchestrationTools(allTools, role)))
			return g.NewAgent(ctx, roleOptions...)
		}
		planned, runErr := g.RunUsecase.ExecutePlan(ctx, factory, usecase.PlanExecutionInput{
			Run:              runInput,
			MaxTasks:         maxPlanTasks,
			ProtocolAttempts: protocolAttempts,
			WorkerAttempts:   workerAttempts,
			MaxReplans:       maxReplans,
		})
		err = runErr
		finalAnswer = planned.FinalAnswer
		usage = planned.Usage
		compactions = planned.Compactions
		completed = planned.Completed
		conversations = planned.Conversations
		agentTurns = len(planned.Runs)
		planTasks = len(planned.Plan.Tasks)
		replans = planned.Replans
		orchestrationID = planned.OrchestrationID
		elapsed = planned.Elapsed.Round(planned.Elapsed.Truncate(1).Truncate(100)).String()
		for _, run := range planned.Runs {
			iterations += run.Output.Iterations
			usageReported = usageReported || run.Output.UsageReported
			stopReason = run.Output.StopReason
		}
	}
	if orchestration == "single" && strings.TrimSpace(sessionFile) != "" {
		if saveErr := infrastructure.SaveConversationSession(sessionFile, agent.ConversationSession()); saveErr != nil {
			g.Logger.Error(ctx, "保存会话失败", "path", sessionFile, "err", saveErr)
			_, _ = fmt.Fprintln(streams.Err, "session save failed:", saveErr)
			return 1
		}
	}
	if err != nil {
		g.Logger.Error(ctx, "运行失败", "err", err)
		_, _ = fmt.Fprintln(streams.Err, "run failed:", err)
		return 1
	}

	_, _ = fmt.Fprintln(streams.Out, "===============")
	_, _ = fmt.Fprintln(streams.Out, "orchestration:", orchestration, "conversations:", conversations,
		"agent_turns:", agentTurns, "plan_tasks:", planTasks, "replans:", replans, "orchestration_id:", orchestrationID,
		"iterations:", iterations, "elapsed:", elapsed)
	usageQuality := "missing"
	if usageReported {
		usageQuality = "reported"
	}
	_, _ = fmt.Fprintln(streams.Out, "usage:", "input", usage.Input,
		"output", usage.Output, "reasoning", usage.Reasoning,
		"cache_read", usage.CacheRead, "cache_write", usage.CacheWrite,
		"total", usage.Total, "quality", usageQuality)
	_, _ = fmt.Fprintln(streams.Out, "compactions:", compactions)
	_, _ = fmt.Fprintln(streams.Out, "stop_reason:", stopReason, "completed:", completed)
	_, _ = fmt.Fprintln(streams.Out, "answer:")
	_, _ = fmt.Fprintln(streams.Out, finalAnswer)
	if !completed {
		g.Logger.Error(ctx, "任务未完成", "orchestration", orchestration,
			"conversations", conversations, "agent_turns", agentTurns, "plan_tasks", planTasks, "replans", replans,
			"stop_reason", stopReason)
		return 1
	}
	if strings.TrimSpace(outputFile) != "" {
		if err := infrastructure.SaveHTMLArtifact(outputFile, finalAnswer); err != nil {
			g.Logger.Error(ctx, "保存 HTML 失败", "path", outputFile, "err", err)
			_, _ = fmt.Fprintln(streams.Err, "html save failed:", err)
			return 1
		}
		g.Logger.Info(ctx, "已保存 HTML", "path", outputFile, "chars", len(finalAnswer))
		_, _ = fmt.Fprintln(streams.Out, "html:", outputFile)
	}
	return 0
}

// runInteractive keeps one Agent alive and sends every non-command input to
// the same conversation session. The usecase already appends user and
// assistant messages, so the CLI only needs to provide the outer read/print
// loop.
func runInteractive(
	ctx context.Context,
	g *infrastructure.Graph,
	agent *entity.Agent,
	profile entity.TaskProfile,
	toolsMode string,
	sessionFile string,
	streams Streams,
) int {
	streams = withDefaultStreams(streams)
	_, _ = fmt.Fprintln(streams.Out, "进入多轮对话模式。输入 /help 查看命令，输入 /exit 或 Ctrl-D 退出。")
	var reader *bufio.Reader
	var editor *cliterm.LineEditor
	if terminal, ok := streams.In.(*os.File); ok && isInteractive(terminal) {
		var editorErr error
		editor, editorErr = cliterm.NewLineEditor(terminal, streams.Out, "pi> ")
		if editorErr != nil {
			fmt.Fprintln(streams.Err, "启用行编辑失败，退回整行输入:", editorErr)
		}
	}
	if editor == nil {
		reader = bufio.NewReader(streams.In)
	}

	for {
		var line string
		var readErr error
		if editor != nil {
			line, readErr = editor.ReadLine()
			if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, cliterm.ErrInterrupt) {
				fmt.Fprintln(streams.Err, "行编辑不可用，退回整行输入:", readErr)
				editor = nil
				reader = bufio.NewReader(streams.In)
				continue
			}
		} else {
			_, _ = fmt.Fprint(streams.Out, "pi> ")
			line, readErr = reader.ReadString('\n')
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			if errors.Is(readErr, cliterm.ErrInterrupt) {
				continue
			}
			fmt.Fprintln(streams.Err, "读取输入:", readErr)
			return 1
		}
		prompt := strings.TrimSpace(line)
		if prompt == "" {
			if errors.Is(readErr, io.EOF) {
				if editor == nil {
					_, _ = fmt.Fprintln(streams.Out)
				}
				return 0
			}
			continue
		}

		switch strings.ToLower(prompt) {
		case "/exit", "/quit":
			_, _ = fmt.Fprintln(streams.Out, "再见。")
			return 0
		case "/help":
			_, _ = fmt.Fprintln(streams.Out, "命令：/help 查看帮助，/reset 清空当前对话，/exit 退出。")
			if errors.Is(readErr, io.EOF) {
				return 0
			}
			continue
		case "/reset":
			agent.ResetConversationSession()
			if strings.TrimSpace(sessionFile) != "" {
				if err := infrastructure.SaveConversationSession(sessionFile, agent.ConversationSession()); err != nil {
					fmt.Fprintln(streams.Err, "保存会话:", err)
					return 1
				}
			}
			_, _ = fmt.Fprintln(streams.Out, "当前对话已清空。")
			if errors.Is(readErr, io.EOF) {
				return 0
			}
			continue
		}

		runCtx := ctx
		cancel := func() {}
		if timeout := g.Config.Agent.Timeout; timeout > 0 {
			runCtx, cancel = context.WithTimeout(ctx, timeout)
		}
		out, err := g.RunUsecase.Execute(runCtx, agent, usecase.RunInput{
			UserPrompt:    prompt,
			TaskProfile:   profile,
			ToolsMode:     toolsMode,
			PromptVersion: g.Prompt.Version,
		})
		cancel()

		if strings.TrimSpace(sessionFile) != "" {
			if saveErr := infrastructure.SaveConversationSession(sessionFile, agent.ConversationSession()); saveErr != nil {
				fmt.Fprintln(streams.Err, "保存会话:", saveErr)
				return 1
			}
		}
		if err != nil {
			fmt.Fprintln(streams.Err, "运行失败:", err)
			if ctx.Err() != nil {
				return 1
			}
		} else {
			_, _ = fmt.Fprintln(streams.Out, "assistant:")
			_, _ = fmt.Fprintln(streams.Out, out.FinalAnswer)
			_, _ = fmt.Fprintf(streams.Out, "[iterations=%d elapsed=%s]\n", out.Iterations, out.Elapsed.Round(time.Millisecond))
		}

		if errors.Is(readErr, io.EOF) {
			return 0
		}
	}
}

// cmdSetup 显式运行首次配置向导。run 命令在发现配置不完整时也会自动
// 进入同一个向导；单独的 setup 命令方便用户在更换 provider 或模型时重配。
func cmdSetup(streams Streams) int {
	streams = withDefaultStreams(streams)
	if !isInteractiveReader(streams.In) {
		fmt.Fprintln(streams.Err, "setup 需要交互式终端；请直接在终端运行 `go run . setup`")
		return 2
	}
	cfg, err := infrastructure.Load()
	if err != nil {
		fmt.Fprintln(streams.Err, "加载配置:", err)
		return 1
	}
	if _, err := setupLLMInteractively(cfg, streams.In, streams.Err); err != nil {
		fmt.Fprintln(streams.Err, "配置 LLM:", err)
		return 1
	}
	return 0
}

// setupLLMInteractively 读取最小 LLM 配置并持久化。
//
// API key 不会显示在提示符、日志或命令行参数中；在支持 stty 的终端中输入
// 时关闭回显。配置文件由 infrastructure.SaveLLMConfig 以 0600 权限原子写入。
// 这是跨平台的本地文件方案；生产环境可把同一个配置接缝替换成 Keychain
// 或 Secret Service，而不必把 secret 放进环境变量或 shell history。
func setupLLMInteractively(cfg infrastructure.Config, input io.Reader, output io.Writer) (infrastructure.Config, error) {
	reader := bufio.NewReader(input)
	terminal, _ := input.(*os.File)
	path, err := infrastructure.UserConfigPath()
	if err != nil {
		return cfg, err
	}

	_, _ = fmt.Fprintln(output, "未检测到完整的 LLM 配置，开始首次设置。")
	_, _ = fmt.Fprintf(output, "配置将保存到：%s\n", path)
	_, _ = fmt.Fprintln(output, "常用 provider：lmstudio、ollama、vllm、openai、anthropic、gemini、custom")

	previousProvider := strings.ToLower(strings.TrimSpace(cfg.LLM.Provider))
	provider, err := promptLine(reader, output, "Provider", cfg.LLM.Provider)
	if err != nil {
		return cfg, err
	}
	provider = strings.ToLower(provider)
	if provider == "" {
		return cfg, fmt.Errorf("provider 不能为空")
	}

	baseURL := cfg.LLM.BaseURL
	if provider != previousProvider {
		baseURL = ""
	}
	if defaultURL := defaultLocalBaseURL(provider); defaultURL != "" && baseURL == "" {
		baseURL = defaultURL
	}
	baseURL, err = promptLine(reader, output, "Base URL（custom 必填）", baseURL)
	if err != nil {
		return cfg, err
	}
	if provider == "custom" && baseURL == "" {
		return cfg, fmt.Errorf("custom provider 必须填写 Base URL，例如 http://127.0.0.1:8000/v1")
	}

	model, err := promptLine(reader, output, "Model", cfg.LLM.Model)
	if err != nil {
		return cfg, err
	}
	if model == "" {
		return cfg, fmt.Errorf("model 不能为空；可先通过 provider 的 /v1/models 查看模型名")
	}

	apiKey := cfg.LLM.APIKey
	keyLabel := "API key（可选；LM Studio 开启鉴权时填写）"
	if providerNeedsAPIKey(provider) {
		keyLabel = "API key（输入时不回显）"
	}
	enteredKey, readErr := promptSecretLine(reader, terminal, output, keyLabel, apiKey != "")
	if readErr != nil {
		return cfg, readErr
	}
	if enteredKey != "" {
		apiKey = enteredKey
	}
	if providerNeedsAPIKey(provider) && apiKey == "" {
		return cfg, fmt.Errorf("%s provider 需要 API key", provider)
	}

	cfg.LLM = infrastructure.LLMConfig{
		Provider: provider,
		APIKey:   apiKey,
		BaseURL:  baseURL,
		Model:    model,
	}
	if err := infrastructure.SaveLLMConfig(cfg.LLM); err != nil {
		return cfg, err
	}
	_, _ = fmt.Fprintln(output, "LLM 配置已保存。下次可直接运行 `go run .`。")
	return cfg, nil
}

func promptLine(reader *bufio.Reader, output io.Writer, label, defaultValue string) (string, error) {
	if defaultValue != "" {
		_, _ = fmt.Fprintf(output, "%s [%s]: ", label, defaultValue)
	} else {
		_, _ = fmt.Fprintf(output, "%s: ", label)
	}
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	line = strings.TrimSpace(line)
	if line == "" {
		return defaultValue, nil
	}
	return line, nil
}

func promptSecretLine(reader *bufio.Reader, terminal *os.File, output io.Writer, label string, hasExisting bool) (string, error) {
	if hasExisting {
		_, _ = fmt.Fprintf(output, "%s [已保存，回车保留]: ", label)
	} else {
		_, _ = fmt.Fprintf(output, "%s: ", label)
	}
	if terminal != nil && isInteractive(terminal) {
		// 直接传参数，不经过 shell；stty 只负责当前终端的回显开关。
		echoOff := exec.Command("stty", "-echo")
		echoOff.Stdin = terminal
		if err := echoOff.Run(); err == nil {
			defer func() {
				echoOn := exec.Command("stty", "echo")
				echoOn.Stdin = terminal
				_ = echoOn.Run()
				_, _ = fmt.Fprintln(output)
			}()
		}
	}
	line, err := reader.ReadString('\n')
	if err != nil && len(line) == 0 {
		return "", err
	}
	line = strings.TrimSpace(line)
	return line, nil
}

func isInteractive(file *os.File) bool {
	info, err := file.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice == 0 {
		return false
	}
	// /dev/null 也是字符设备，但不是可交互终端。直接调用 tty（不经过
	// shell）验证文件描述符，避免把管道或重定向误判成 setup 终端。
	cmd := exec.Command("tty")
	cmd.Stdin = file
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	return cmd.Run() == nil
}

func providerNeedsAPIKey(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "ollama", "lmstudio", "vllm", "custom":
		return false
	case "openai", "anthropic", "gemini", "google", "openrouter", "groq",
		"mistral", "xai", "deepseek", "cerebras", "zai", "kimi", "minimax":
		return true
	default:
		return false
	}
}

func defaultLocalBaseURL(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "lmstudio":
		return "http://127.0.0.1:1234/v1"
	case "ollama":
		return "http://127.0.0.1:11434/v1"
	case "vllm":
		return "http://127.0.0.1:8000/v1"
	default:
		return ""
	}
}

func resolveToolsMode(mode string, noTools bool, _ string, _ string) (bool, error) {
	mode = strings.ToLower(strings.TrimSpace(mode))
	if noTools {
		if mode != "" && mode != "auto" && mode != "disabled" {
			return false, fmt.Errorf("--no-tools 不能与 --tools=%s 同时使用", mode)
		}
		return false, nil
	}
	switch mode {
	case "", "auto":
		// 能力必须由调用方显式收窄；auto 保留通用 Agent 的工具集合。
		// output/prompt 只描述任务，不足以安全推断其权限需求。
		return true, nil
	case "enabled":
		return true, nil
	case "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("--tools 必须是 auto、enabled 或 disabled")
	}
}

func resolveTaskProfile(profile string, _ string, _ string) (entity.TaskProfile, error) {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "", "auto":
		// 未声明时保守保留完整 Agent profile；生成任务可显式选择
		// --task-profile generation，并同时使用 --tools=disabled。
		return entity.TaskProfileAgentMutation, nil
	case string(entity.TaskProfileGeneration):
		return entity.TaskProfileGeneration, nil
	case string(entity.TaskProfileAgentReadonly):
		return entity.TaskProfileAgentReadonly, nil
	case string(entity.TaskProfileAgentMutation):
		return entity.TaskProfileAgentMutation, nil
	default:
		return "", fmt.Errorf("--task-profile 必须是 auto、generation、agent-readonly 或 agent-mutation")
	}
}

func resolvedToolsMode(mode string, enabled bool) string {
	if !enabled {
		return "disabled"
	}
	mode = strings.ToLower(strings.TrimSpace(mode))
	if mode == "" || mode == "auto" {
		return "enabled"
	}
	return mode
}

// orchestrationTools gives each fresh conversation the tools its role needs.
// Workers keep the caller's complete tool set. Planning is read-only, while
// verification may also run commands to validate the integrated workspace.
func orchestrationTools(all []entity.Tool, role usecase.OrchestrationRole) []entity.Tool {
	if role == usecase.OrchestrationWorker {
		return append([]entity.Tool(nil), all...)
	}
	filtered := make([]entity.Tool, 0, len(all))
	for _, tool := range all {
		access := tool.Info().Access
		if access == entity.ToolAccessRead ||
			(role == usecase.OrchestrationVerifier && access == entity.ToolAccessExecute) {
			filtered = append(filtered, tool)
		}
	}
	return filtered
}

func printSetupHint(cfg infrastructure.Config, output io.Writer) {
	path, _ := infrastructure.UserConfigPath()
	fmt.Fprintln(output, "当前 LLM 配置不完整，未启动网络请求。")
	fmt.Fprintf(output, "配置文件位置：%s\n", path)
	fmt.Fprintln(output, "请在交互式终端运行：go run . setup")
	fmt.Fprintln(output, "或显式提供：--provider、--model，以及远程 provider 的 --api-key。")
	if cfg.LLM.Provider == "lmstudio" {
		fmt.Fprintln(output, "LM Studio 示例：go run . run --provider lmstudio --base-url http://127.0.0.1:1234/v1 --model <模型名> --prompt '你好'")
	}
}

const helpText = `
pi-agent — 精简 Clean Architecture Go AI Agent 骨架

Usage:
  pi-agent run [flags] [--prompt "hello"]  运行一次 agent 会话；终端中无 prompt 时进入多轮模式
  pi-agent run --interactive              显式进入多轮对话模式
  pi-agent setup                         交互式配置本地或远程 LLM
  pi-agent providers                       列出内置 provider
  pi-agent version                 打印版本并退出
  pi-agent help                    显示本帮助

环境变量:
  LLM_PROVIDER         provider 名称（默认: openai）
  LLM_API_KEY          所选 provider 的 API key           (默认: 空)
  LLM_BASE_URL         覆盖端点 base URL                  (默认: provider 默认)
  LLM_MODEL            使用的模型 id                       (默认: provider 默认)
  LLM_USER_CONTENT_FORMAT text | parts 用户消息格式          (默认: text)
  LM_API_TOKEN         LM Studio 开启认证时的 API token
  AGENT_NAME           agent 名称                         (默认: pi-agent)
  AGENT_SYSTEM_PROMPT  最先注入的系统提示                   (默认: 空)
  AGENT_TEMPERATURE    0..2 采样温度                        (默认: 0.7)
  AGENT_MAX_TOKENS     单次模型输出 token 上限               (默认: provider)
  AGENT_MAX_ITERATIONS 每次运行最大工具调用循环数           (默认: 5)
  AGENT_TIMEOUT        整次 Agent 预算，例如 10m、1h              (默认: context)
  AGENT_CONTEXT_WINDOW 模型上下文窗口 token 数；0 时不压缩       (默认: 0)
  AGENT_CONTEXT_RESERVE_TOKENS 为输出和协议预留的 token 数       (默认: 16384)
  AGENT_CONTEXT_KEEP_RECENT_TOKENS 压缩后保留原文的 token 数     (默认: 20000)
  AGENT_CONTEXT_SUMMARY_MAX_TOKENS 压缩摘要的输出 token 上限     (默认: 2048)
  AGENT_CONTEXT_TOOL_RESULT_MAX_CHARS 摘要输入中单条工具结果上限 (默认: 2000)
  AGENT_INCLUDE_WORKING_DIRECTORY 在系统提示中加入 Pi 同形 cwd section (默认: false)
  LOG_LEVEL            debug | info | warn | error        (默认: info)
  AUDIT_LOG_FILE        JSONL 审计文件路径；为空时不启用
  AUDIT_CONTENT_MODE    redacted | full（默认: redacted）
  PI_AGENT_CONFIG_FILE  覆盖用户配置文件路径（默认使用 os.UserConfigDir）

调试:
  run --debug 会向 stderr 输出实际使用的 PromptArtifact 元数据和系统提示词。
  设置 LOG_LEVEL=debug 可查看每轮 Agent/LLM 循环的结构化日志。
  首次运行或配置不完整时会进入交互式设置；也可单独运行 pi-agent setup。
  API key 不会回显，配置文件使用 0600 权限保存。非交互终端请使用环境变量
  或显式 flags，程序不会阻塞等待输入。

run flags:
  --interactive                             持续读取多轮输入；/help、/reset、/exit 可用。
  --provider, --model, --api-key, --base-url  仅覆盖本次运行的 LLM 配置。
  --audit-file, --audit-content               仅覆盖本次运行的审计配置。
  --log-file path                             将结构化运行日志单独写入文件；交互模式默认静默。
  --output path                                将最终回答清洗后保存为 HTML 文件。
  --tools auto|enabled|disabled                auto 保留通用 Agent 工具；可显式收窄为 disabled。
  --task-profile auto|generation|agent-readonly|agent-mutation
                                               generation 可在畸形 tool call 时安全降级；写入任务不会自动重试。
  --session path                               single 模式跨进程保存会话历史和摘要状态。
  --orchestration single|plan                  plan 先生成任务 DAG，每个任务使用独立新会话，最后独立验收。
  --max-plan-tasks n                           任务总数安全上限；不会预先决定实际会话数。
  --protocol-attempts n                        规划和验收结构化输出的尝试上限。
  --worker-attempts n                          每个任务的执行次数上限；默认 1，避免重复副作用。
  --max-replans n                              验收失败后追加修复任务的次数上限。
  --no-tools                                   --tools=disabled 的兼容别名。
  API key 优先级：--api-key > LLM_API_KEY > provider 专属环境变量。
`
