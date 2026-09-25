package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"

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
	fs.StringVar(&prompt, "prompt", "", "发送给 agent 的用户 prompt")
	fs.BoolVar(&debug, "debug", false, "打印本次提示词与运行配置（可能包含敏感内容）")
	fs.StringVar(&provider, "provider", "", "本次使用的 provider，覆盖 LLM_PROVIDER")
	fs.StringVar(&apiKey, "api-key", "", "本次使用的 API key，覆盖环境变量")
	fs.StringVar(&baseURL, "base-url", "", "本次 API base URL，覆盖 LLM_BASE_URL")
	fs.StringVar(&model, "model", "", "本次使用的模型，覆盖 LLM_MODEL")
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
	g, err := infrastructure.BuildWithConfig(cfg)
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动:", err)
		return 1
	}
	agent := g.NewAgent(ctx)
	if debug {
		// Debug 输出写 stderr，避免与最终 answer 的 stdout 混在一起，便于
		// 脚本只采集回答。提示词可能含业务上下文，生产环境请谨慎开启。
		fmt.Fprintf(os.Stderr, "debug: prompt id=%s version=%s sha256=%s chars=%d\n",
			g.Prompt.ID, g.Prompt.Version, g.Prompt.Hash, len(g.Prompt.Content))
		fmt.Fprintf(os.Stderr, "debug: model=%q provider=%q base_url=%q max_iterations=%d tools=%d\n",
			agent.Config().Model, g.Config.LLM.Provider, g.Config.LLM.BaseURL, agent.Config().MaxIterations, len(agent.Tools()))
		fmt.Fprintln(os.Stderr, "debug: system prompt follows")
		fmt.Fprintln(os.Stderr, g.Prompt.Content)
	}
	g.Logger.Info(ctx, "已构建 agent", "name", agent.Config().Name,
		"has_llm", agent.LLM() != nil, "has_memory", agent.Memory() != nil,
		"plugins", len(agent.Plugins()), "tools", len(agent.Tools()))

	out, err := g.RunUsecase.Execute(ctx, agent, usecase.RunInput{UserPrompt: prompt})
	if err != nil {
		g.Logger.Error(ctx, "运行失败", "err", err)
		_, _ = fmt.Fprintln(os.Stdout, "run failed:", err)
		return 1
	}

	_, _ = fmt.Fprintln(os.Stdout, "===============")
	_, _ = fmt.Fprintln(os.Stdout, "iterations:", out.Iterations, " elapsed:", out.Elapsed.Round(out.Elapsed.Truncate(1).Truncate(100)))
	_, _ = fmt.Fprintln(os.Stdout, "answer:")
	_, _ = fmt.Fprintln(os.Stdout, out.FinalAnswer)
	return 0
}

const helpText = `
pi-agent — 精简 Clean Architecture Go AI Agent 骨架

Usage:
  pi-agent run [flags] [--prompt "hello"]  运行一次 agent 会话（默认命令）
  pi-agent providers                       列出内置 provider
  pi-agent version                 打印版本并退出
  pi-agent help                    显示本帮助

环境变量:
  LLM_PROVIDER         provider 名称（默认: openai）
  LLM_API_KEY          所选 provider 的 API key           (默认: 空)
  LLM_BASE_URL         覆盖端点 base URL                  (默认: provider 默认)
  LLM_MODEL            使用的模型 id                       (默认: provider 默认)
  AGENT_NAME           agent 名称                         (默认: pi-agent)
  AGENT_SYSTEM_PROMPT  最先注入的系统提示                   (默认: 空)
  AGENT_TEMPERATURE    0..2 采样温度                        (默认: 0.7)
  AGENT_MAX_ITERATIONS 每次运行最大工具调用循环数           (默认: 5)
  LOG_LEVEL            debug | info | warn | error        (默认: info)

调试:
  run --debug 会向 stderr 输出实际使用的 PromptArtifact 元数据和系统提示词。
  设置 LOG_LEVEL=debug 可查看每轮 Agent/LLM 循环的结构化日志。

run flags:
  --provider, --model, --api-key, --base-url  仅覆盖本次运行的 LLM 配置。
  API key 优先级：--api-key > LLM_API_KEY > provider 专属环境变量。
`
