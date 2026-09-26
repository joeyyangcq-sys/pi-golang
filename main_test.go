package main

import (
	"testing"

	"pi-golang/internal/entity"
)

func TestResolveToolsMode(t *testing.T) {
	tests := []struct {
		name       string
		mode       string
		noTools    bool
		outputFile string
		prompt     string
		want       bool
		wantErr    bool
	}{
		{name: "auto preserves agent capabilities", mode: "auto", prompt: "只返回完整 HTML 页面", want: true},
		{name: "output does not infer permissions", mode: "auto", outputFile: "result.html", prompt: "build anything", want: true},
		{name: "file request keeps tools", mode: "auto", prompt: "读取文件并总结", want: true},
		{name: "explicit enabled wins", mode: "enabled", prompt: "只返回 HTML", want: true},
		{name: "compatibility alias", mode: "auto", noTools: true, prompt: "读取文件", want: false},
		{name: "conflicting flags rejected", mode: "enabled", noTools: true, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveToolsMode(tt.mode, tt.noTools, tt.outputFile, tt.prompt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveToolsMode() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("resolveToolsMode() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveTaskProfile(t *testing.T) {
	tests := []struct {
		name       string
		profile    string
		outputFile string
		prompt     string
		want       entity.TaskProfile
		wantErr    bool
	}{
		{name: "auto preserves mutation boundary", profile: "auto", prompt: "只返回完整 HTML", want: entity.TaskProfileAgentMutation},
		{name: "output does not infer profile", profile: "auto", outputFile: "result.html", want: entity.TaskProfileAgentMutation},
		{name: "file task is mutation", profile: "auto", prompt: "读取文件后写入摘要", want: entity.TaskProfileAgentMutation},
		{name: "explicit readonly", profile: "agent-readonly", want: entity.TaskProfileAgentReadonly},
		{name: "invalid profile", profile: "other", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveTaskProfile(tt.profile, tt.outputFile, tt.prompt)
			if (err != nil) != tt.wantErr {
				t.Fatalf("resolveTaskProfile() error = %v, wantErr %v", err, tt.wantErr)
			}
			if err == nil && got != tt.want {
				t.Fatalf("resolveTaskProfile() = %q, want %q", got, tt.want)
			}
		})
	}
}
