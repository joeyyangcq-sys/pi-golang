package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"

	"pi-golang/internal/entity"
	"pi-golang/internal/infrastructure"
	"pi-golang/internal/usecase"
)

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	args := os.Args[1:]
	if len(args) == 0 {
		args = []string{"run"}
	}

	switch args[0] {
	case "version", "-v", "--version":
		fmt.Println("pi-agent 0.1.0 (minimal)")
		return
	case "providers":
		fmt.Println(strings.Join(infrastructure.SupportedProviders(), "\n"))
		return
	case "setup":
		code := cmdSetup()
		if code != 0 {
			os.Exit(code)
		}
		return
	case "help", "-h", "--help":
		fmt.Print(strings.TrimSpace(helpText) + "\n")
		return
	case "run":
		code := cmdRun(ctx, args[1:])
		if code != 0 {
			os.Exit(code)
		}
		return
	default:
		fmt.Fprintln(os.Stderr, "未知命令:", args[0])
		fmt.Fprintln(os.Stderr, "运行 `pi-agent help` 查看用法")
		os.Exit(2)
	}
}

func cmdRun(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var prompt string
	var debug bool
	var provider string
	var apiKey string
	var baseURL string
	var model string
	var auditFile string
	var auditContent string
	var outputFile string
	var noTools bool
	var toolsMode string
	var taskProfile string
	fs.StringVar(&prompt, "prompt", "", "发送给 agent 的用户 prompt")
	fs.BoolVar(&debug, "debug", false, "打印本次提示词与运行配置（可能包含敏感内容）")
	fs.StringVar(&provider, "provider", "", "本次使用的 provider，覆盖 LLM_PROVIDER")
	fs.StringVar(&apiKey, "api-key", "", "本次使用的 API key，覆盖环境变量")
	fs.StringVar(&baseURL, "base-url", "", "本次 API base URL，覆盖 LLM_BASE_URL")
	fs.StringVar(&model, "model", "", "本次使用的模型，覆盖 LLM_MODEL")
	fs.StringVar(&auditFile, "audit-file", "", "JSONL 审计文件路径，记录每轮 LLM 输入输出")
	fs.StringVar(&auditContent, "audit-content", "", "审计内容模式：redacted（默认）或 full")
	fs.StringVar(&outputFile, "output", "", "把最终回答作为 HTML 原子写入文件")
	fs.BoolVar(&noTools, "no-tools", false, "本次运行不向 LLM 注册工具（适合纯文本/HTML 生成任务）")
	fs.StringVar(&toolsMode, "tools", "auto", "工具模式：auto（默认）、enabled、disabled")
	fs.StringVar(&taskProfile, "task-profile", "auto", "任务 profile：auto、generation、agent-readonly、agent-mutation")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if prompt == "" {
		prompt = "hello"
	}

	cfg, err := infrastructure.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载配置:", err)
		return 1
	}
	cfg = cfg.WithLLMOverrides(provider, apiKey, baseURL, model)
	if cfg.NeedsLLMSetup() {
		if !isInteractive(os.Stdin) {
			printSetupHint(cfg)
			return 1
		}
		cfg, err = setupLLMInteractively(cfg, os.Stdin, os.Stderr)
		if err != nil {
			fmt.Fprintln(os.Stderr, "配置 LLM:", err)
			return 1
		}
	}
	cfg = cfg.WithAuditOverrides(auditFile, auditContent)
	if cfg.Agent.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, cfg.Agent.Timeout)
		defer cancel()
	}
	g, err := infrastructure.BuildWithConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动:", err)
		return 1
	}
	defer func() {
		if closeErr := g.Close(); closeErr != nil {
			fmt.Fprintln(os.Stderr, "关闭审计日志:", closeErr)
		}
	}()
	toolsEnabled, err := resolveToolsMode(toolsMode, noTools, outputFile, prompt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数错误:", err)
		return 2
	}
	profile, err := resolveTaskProfile(taskProfile, outputFile, prompt)
	if err != nil {
		fmt.Fprintln(os.Stderr, "参数错误:", err)
		return 2
	}
	var agentOptions []entity.Option
	if !toolsEnabled {
		agentOptions = append(agentOptions, entity.WithTools(nil))
	}
	agent := g.NewAgent(ctx, agentOptions...)
	if debug {
		// Debug 输出写 stderr，避免与最终 answer 的 stdout 混在一起，便于
		// 脚本只采集回答。提示词可能含业务上下文，生产环境请谨慎开启。
		fmt.Fprintf(os.Stderr, "debug: prompt id=%s version=%s sha256=%s chars=%d\n",
			g.Prompt.ID, g.Prompt.Version, g.Prompt.Hash, len(g.Prompt.Content))
		fmt.Fprintf(os.Stderr, "debug: model=%q provider=%q base_url=%q max_tokens=%d max_iterations=%d timeout=%s tools=%d tools_mode=%s\n",
			agent.Config().Model, g.Config.LLM.Provider, g.Config.LLM.BaseURL, agent.Config().MaxTokens, agent.Config().MaxIterations, agent.Config().Timeout, len(agent.Tools()), toolsMode)
		for _, tool := range agent.Tools() {
			info := tool.Info()
			_, _ = fmt.Fprintf(os.Stderr, "debug: tool name=%q description=%q schema=%s\n",
				info.Name, info.Description, string(info.InputSchema))
		}
		fmt.Fprintln(os.Stderr, "debug: system prompt follows")
		fmt.Fprintln(os.Stderr, g.Prompt.Content)
	}
	g.Logger.Info(ctx, "已构建 agent", "name", agent.Config().Name,
		"has_llm", agent.LLM() != nil, "has_memory", agent.Memory() != nil,
		"plugins", len(agent.Plugins()), "tools", len(agent.Tools()))

	out, err := g.RunUsecase.Execute(ctx, agent, usecase.RunInput{
		UserPrompt:    prompt,
		TaskProfile:   profile,
		ToolsMode:     resolvedToolsMode(toolsMode, toolsEnabled),
		PromptVersion: g.Prompt.Version,
	})
	if err != nil {
		g.Logger.Error(ctx, "运行失败", "err", err)
		_, _ = fmt.Fprintln(os.Stdout, "run failed:", err)
		return 1
	}

	_, _ = fmt.Fprintln(os.Stdout, "===============")
	_, _ = fmt.Fprintln(os.Stdout, "iterations:", out.Iterations, " elapsed:", out.Elapsed.Round(out.Elapsed.Truncate(1).Truncate(100)))
	usageQuality := "missing"
	if out.UsageReported {
		usageQuality = "reported"
	}
	_, _ = fmt.Fprintln(os.Stdout, "usage:", "input", out.Usage.Input,
		"output", out.Usage.Output, "reasoning", out.Usage.Reasoning,
		"cache_read", out.Usage.CacheRead, "cache_write", out.Usage.CacheWrite,
		"total", out.Usage.Total, "quality", usageQuality)
	_, _ = fmt.Fprintln(os.Stdout, "answer:")
	_, _ = fmt.Fprintln(os.Stdout, out.FinalAnswer)
	if strings.TrimSpace(outputFile) != "" {
		if err := infrastructure.SaveHTMLArtifact(outputFile, out.FinalAnswer); err != nil {
			g.Logger.Error(ctx, "保存 HTML 失败", "path", outputFile, "err", err)
			_, _ = fmt.Fprintln(os.Stdout, "html save failed:", err)
			return 1
		}
		g.Logger.Info(ctx, "已保存 HTML", "path", outputFile, "chars", len(out.FinalAnswer))
		_, _ = fmt.Fprintln(os.Stdout, "html:", outputFile)
	}
	return 0
}

// cmdSetup 显式运行首次配置向导。run 命令在发现配置不完整时也会自动
// 进入同一个向导；单独的 setup 命令方便用户在更换 provider 或模型时重配。
func cmdSetup() int {
	if !isInteractive(os.Stdin) {
		fmt.Fprintln(os.Stderr, "setup 需要交互式终端；请直接在终端运行 `go run . setup`")
		return 2
	}
	cfg, err := infrastructure.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载配置:", err)
		return 1
	}
	if _, err := setupLLMInteractively(cfg, os.Stdin, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "配置 LLM:", err)
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
func setupLLMInteractively(cfg infrastructure.Config, input *os.File, output io.Writer) (infrastructure.Config, error) {
	reader := bufio.NewReader(input)
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
	enteredKey, readErr := promptSecretLine(reader, output, keyLabel, apiKey != "")
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

func promptSecretLine(reader *bufio.Reader, output io.Writer, label string, hasExisting bool) (string, error) {
	if hasExisting {
		_, _ = fmt.Fprintf(output, "%s [已保存，回车保留]: ", label)
	} else {
		_, _ = fmt.Fprintf(output, "%s: ", label)
	}
	if isInteractive(os.Stdin) {
		// 直接传参数，不经过 shell；stty 只负责当前终端的回显开关。
		echoOff := exec.Command("stty", "-echo")
		echoOff.Stdin = os.Stdin
		if err := echoOff.Run(); err == nil {
			defer func() {
				echoOn := exec.Command("stty", "echo")
				echoOn.Stdin = os.Stdin
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

func printSetupHint(cfg infrastructure.Config) {
	path, _ := infrastructure.UserConfigPath()
	fmt.Fprintln(os.Stderr, "当前 LLM 配置不完整，未启动网络请求。")
	fmt.Fprintf(os.Stderr, "配置文件位置：%s\n", path)
	fmt.Fprintln(os.Stderr, "请在交互式终端运行：go run . setup")
	fmt.Fprintln(os.Stderr, "或显式提供：--provider、--model，以及远程 provider 的 --api-key。")
	if cfg.LLM.Provider == "lmstudio" {
		fmt.Fprintln(os.Stderr, "LM Studio 示例：go run . run --provider lmstudio --base-url http://127.0.0.1:1234/v1 --model <模型名> --prompt '你好'")
	}
}

const helpText = `
pi-agent — 精简 Clean Architecture Go AI Agent 骨架

Usage:
  pi-agent run [flags] [--prompt "hello"]  运行一次 agent 会话（默认命令）
  pi-agent setup                         交互式配置本地或远程 LLM
  pi-agent providers                       列出内置 provider
  pi-agent version                 打印版本并退出
  pi-agent help                    显示本帮助

环境变量:
  LLM_PROVIDER         provider 名称（默认: openai）
  LLM_API_KEY          所选 provider 的 API key           (默认: 空)
  LLM_BASE_URL         覆盖端点 base URL                  (默认: provider 默认)
  LLM_MODEL            使用的模型 id                       (默认: provider 默认)
  LM_API_TOKEN         LM Studio 开启认证时的 API token
  AGENT_NAME           agent 名称                         (默认: pi-agent)
  AGENT_SYSTEM_PROMPT  最先注入的系统提示                   (默认: 空)
  AGENT_TEMPERATURE    0..2 采样温度                        (默认: 0.7)
  AGENT_MAX_TOKENS     单次模型输出 token 上限               (默认: provider)
  AGENT_MAX_ITERATIONS 每次运行最大工具调用循环数           (默认: 5)
  AGENT_TIMEOUT        整次 Agent 预算，例如 10m、1h              (默认: context)
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
  --provider, --model, --api-key, --base-url  仅覆盖本次运行的 LLM 配置。
	  --audit-file, --audit-content               仅覆盖本次运行的审计配置。
	  --output path                                将最终回答清洗后保存为 HTML 文件。
	  --tools auto|enabled|disabled                 auto 会为 HTML/纯文本生成关闭工具；可显式覆盖。
	  --task-profile auto|generation|agent-readonly|agent-mutation
	                                               generation 可在畸形 tool call 时安全降级；写入任务不会自动重试。
	  --no-tools                                   --tools=disabled 的兼容别名。
  API key 优先级：--api-key > LLM_API_KEY > provider 专属环境变量。
`
