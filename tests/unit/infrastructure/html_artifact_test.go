package infrastructure_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pi-golang/internal/infrastructure"
)

func TestNormalizeHTML_RemovesFenceAndSurroundingText(t *testing.T) {
	input := "下面是页面：\n```html\n<!doctype html><html><body>ok</body></html>\n```\n结束。"

	got, err := infrastructure.NormalizeHTML(input)
	if err != nil {
		t.Fatalf("NormalizeHTML() error = %v", err)
	}
	if !strings.HasPrefix(strings.ToLower(got), "<!doctype html") {
		t.Fatalf("NormalizeHTML() = %q, want doctype prefix", got)
	}
	if strings.Contains(got, "```") || strings.Contains(got, "结束") {
		t.Fatalf("NormalizeHTML() 未移除包裹文本: %q", got)
	}
}

func TestNormalizeHTML_RejectsPlainText(t *testing.T) {
	if _, err := infrastructure.NormalizeHTML("这不是 HTML"); err == nil {
		t.Fatal("NormalizeHTML() error = nil, want invalid HTML error")
	}
}

func TestSaveHTMLArtifact_WritesNormalizedDocument(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "page.html")
	if err := infrastructure.SaveHTMLArtifact(path, "```html\n<html><body>ok</body></html>\n```"); err != nil {
		t.Fatalf("SaveHTMLArtifact() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if got := string(data); got != "<html><body>ok</body></html>\n" {
		t.Fatalf("saved HTML = %q", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("saved HTML permissions = %o, want 644", got)
	}
}
