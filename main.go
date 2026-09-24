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
	fs.StringVar(&prompt, "prompt", "", "发送给 agent 的用户 prompt")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if prompt == "" {
		prompt = "hello"
	}

	g, err := infrastructure.Build()
	if err != nil {
		fmt.Fprintln(os.Stderr, "启动:", err)
		return 1
	}
	agent := g.NewAgent(ctx)
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
  pi-agent run [-prompt "hello"]   运行一次 agent 会话（默认命令）
  pi-agent version                 打印版本并退出
  pi-agent help                    显示本帮助

环境变量:
  LLM_PROVIDER         openai | openrouter | anthropic   (默认: openai)
  LLM_API_KEY          所选 provider 的 API key           (默认: 空)
  LLM_BASE_URL         覆盖端点 base URL                  (默认: provider 默认)
  LLM_MODEL            使用的模型 id                       (默认: provider 默认)
  AGENT_NAME           agent 名称                         (默认: pi-agent)
  AGENT_SYSTEM_PROMPT  最先注入的系统提示                   (默认: 空)
  AGENT_TEMPERATURE    0..2 采样温度                        (默认: 0.7)
  AGENT_MAX_ITERATIONS 每次运行最大工具调用循环数           (默认: 5)
  LOG_LEVEL            debug | info | warn | error        (默认: info)
`
