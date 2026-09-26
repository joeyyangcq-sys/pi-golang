package infrastructure

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const (
	doctypeHTML = "<!doctype html"
	openHTML    = "<html"
	closeHTML   = "</html>"
)

// SaveHTMLArtifact 将 Agent 的最终文本清洗为一个 HTML 文档并原子写入文件。
//
// LLM 常会把 HTML 包在 Markdown 的 ```html 代码围栏中，或者在文档前后加
// 一句解释。这个边界层只移除这些包裹，不修改文档内部的 HTML、CSS 或 JS。
// 文件先写入同目录临时文件并 Sync，再通过 Rename 替换目标，避免进程中断
// 时留下半个 HTML 文件。生成的网页不是凭据，使用 0644 方便浏览器直接打开。
func SaveHTMLArtifact(path, content string) error {
	normalized, err := NormalizeHTML(content)
	if err != nil {
		return err
	}
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("html artifact: 输出路径不能为空")
	}

	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("html artifact: 创建目录: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".html-*.tmp")
	if err != nil {
		return fmt.Errorf("html artifact: 创建临时文件: %w", err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("html artifact: 设置权限: %w", err)
	}
	if _, err := tmp.WriteString(normalized); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("html artifact: 写入: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("html artifact: 刷新: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("html artifact: 关闭临时文件: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("html artifact: 替换 %s: %w", path, err)
	}
	return nil
}

// NormalizeHTML 从模型回答中提取完整 HTML 文档。
//
// 这里不使用 HTML parser：任务目标是保留模型生成的原始文档，parser 可能
// 重排属性、修复标签或改变内联脚本。校验只要求出现 <html，保证误把普通
// 文本答案写成 .html 时尽早失败。
func NormalizeHTML(content string) (string, error) {
	text := strings.TrimSpace(content)
	if text == "" {
		return "", errors.New("html artifact: Agent 返回为空")
	}
	if strings.HasPrefix(text, "```") {
		text = stripCodeFence(text)
	}
	lower := strings.ToLower(text)
	start := strings.Index(lower, doctypeHTML)
	if start < 0 {
		start = strings.Index(lower, openHTML)
	}
	if start >= 0 {
		text = text[start:]
		lower = strings.ToLower(text)
	}
	end := strings.LastIndex(lower, closeHTML)
	if end >= 0 {
		text = text[:end+len(closeHTML)]
	}
	if !strings.Contains(strings.ToLower(text), openHTML) {
		return "", errors.New("html artifact: Agent 返回不是完整 HTML 文档")
	}
	return strings.TrimSpace(text) + "\n", nil
}

func stripCodeFence(text string) string {
	lines := strings.Split(text, "\n")
	if len(lines) < 2 {
		return text
	}
	if strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
		lines = lines[:len(lines)-1]
	}
	return strings.Join(lines, "\n")
}
