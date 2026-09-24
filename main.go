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
		fmt.Fprintln(os.Stderr, "unknown command:", args[0])
		fmt.Fprintln(os.Stderr, "run `pi-agent help` for usage")
		os.Exit(2)
	}
}

func cmdRun(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("run", flag.ContinueOnError)
	var prompt string
	fs.StringVar(&prompt, "prompt", "", "the user prompt to send to the agent")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if prompt == "" {
		prompt = "hello"
	}

	g, err := infrastructure.Build()
	if err != nil {
		fmt.Fprintln(os.Stderr, "startup:", err)
		return 1
	}
	agent := g.NewAgent(ctx)
	g.Logger.Info(ctx, "built agent", "name", agent.Config().Name,
		"has_llm", agent.LLM() != nil, "has_memory", agent.Memory() != nil)

	out, err := g.RunUsecase.Execute(ctx, agent, usecase.RunInput{UserPrompt: prompt})
	if err != nil {
		g.Logger.Error(ctx, "run failed", "err", err)
		fmt.Fprintln(os.Stdout, "run failed:", err)
		return 1
	}

	fmt.Fprintln(os.Stdout, "===============")
	fmt.Fprintln(os.Stdout, "iterations:", out.Iterations, " elapsed:", out.Elapsed.Round(out.Elapsed.Truncate(1).Truncate(100)))
	fmt.Fprintln(os.Stdout, "answer:")
	fmt.Fprintln(os.Stdout, out.FinalAnswer)
	return 0
}

const helpText = `
pi-agent — minimal Clean Architecture Go AI Agent skeleton

Usage:
  pi-agent run [-prompt "hello"]   Run one agent turn (default command)
  pi-agent version                 Print version and exit
  pi-agent help                    Show this help

Environment variables:
  LLM_PROVIDER         openai | openrouter | anthropic   (default: openai)
  LLM_API_KEY          API key for the chosen provider   (default: empty)
  LLM_BASE_URL         Override endpoint base URL        (default: provider default)
  LLM_MODEL            Model id to use                   (default: provider default)
  AGENT_NAME           Name of the agent                 (default: pi-agent)
  AGENT_SYSTEM_PROMPT  System prompt injected first      (default: empty)
  AGENT_TEMPERATURE    0..2 sampling temperature         (default: 0.7)
  AGENT_MAX_ITERATIONS Max tool-call loops per run       (default: 5)
  LOG_LEVEL            debug | info | warn | error       (default: info)
`
