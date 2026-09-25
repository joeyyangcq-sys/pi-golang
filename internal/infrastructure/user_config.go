package infrastructure

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// persistedLLMConfig 是写入本机用户配置文件的最小字段集合。
//
// 不把日志、审计或 Agent 运行状态写入这里，避免配置文件变成不可控的
// 大型数据文件。API key 也不会出现在普通日志中；文件本身使用 0600 权限。
type persistedLLMConfig struct {
	Provider string `json:"provider,omitempty"`
	APIKey   string `json:"api_key,omitempty"`
	BaseURL  string `json:"base_url,omitempty"`
	Model    string `json:"model,omitempty"`
}

// UserConfigPath 返回 pi-agent 的本机配置文件路径。
//
// 默认使用 os.UserConfigDir：macOS 通常是 ~/Library/Application Support，
// Linux 通常是 $XDG_CONFIG_HOME 或 ~/.config。PI_AGENT_CONFIG_FILE 可用于
// 测试、容器和显式部署；它只改变路径，不会改变权限策略。
func UserConfigPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv("PI_AGENT_CONFIG_FILE")); path != "" {
		return filepath.Clean(path), nil
	}
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("config: 找不到用户配置目录: %w", err)
	}
	return filepath.Join(dir, "pi-agent", "config.json"), nil
}

func loadPersistedLLMConfig() (LLMConfig, error) {
	path, err := UserConfigPath()
	if err != nil {
		return LLMConfig{}, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return LLMConfig{}, nil
	}
	if err != nil {
		return LLMConfig{}, fmt.Errorf("config: 读取 %s: %w", path, err)
	}
	var saved persistedLLMConfig
	if err := json.Unmarshal(data, &saved); err != nil {
		return LLMConfig{}, fmt.Errorf("config: 解析 %s: %w", path, err)
	}
	return LLMConfig{
		Provider: strings.ToLower(strings.TrimSpace(saved.Provider)),
		APIKey:   strings.TrimSpace(saved.APIKey),
		BaseURL:  strings.TrimSpace(saved.BaseURL),
		Model:    strings.TrimSpace(saved.Model),
	}, nil
}

// SaveLLMConfig 将 provider、endpoint 和凭据原子写入用户配置文件。
//
// 目录使用 0700，文件使用 0600；写入先落到同目录临时文件并 Sync，再通过
// rename 替换目标，避免程序中断留下半截 JSON。此处仍属于“本机文件凭据”
// 方案：生产环境如果已有 macOS Keychain、Linux Secret Service 或企业密钥
// 管理器，应优先由上层接入专门的 secret store，而不是共享配置文件。
func SaveLLMConfig(llm LLMConfig) error {
	path, err := UserConfigPath()
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("config: 创建目录 %s: %w", dir, err)
	}

	payload, err := json.MarshalIndent(persistedLLMConfig{
		Provider: strings.ToLower(strings.TrimSpace(llm.Provider)),
		APIKey:   strings.TrimSpace(llm.APIKey),
		BaseURL:  strings.TrimSpace(llm.BaseURL),
		Model:    strings.TrimSpace(llm.Model),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("config: 编码: %w", err)
	}

	tmp, err := os.CreateTemp(dir, ".config-*.tmp")
	if err != nil {
		return fmt.Errorf("config: 创建临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: 设置权限: %w", err)
	}
	if _, err := tmp.Write(append(payload, '\n')); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: 写入: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("config: 刷新: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("config: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("config: 替换 %s: %w", path, err)
	}
	return nil
}
