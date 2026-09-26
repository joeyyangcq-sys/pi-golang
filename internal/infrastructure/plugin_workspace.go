package infrastructure

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"pi-golang/internal/entity"
)

const (
	defaultReadBytes   = 128 << 10
	maxReadBytes       = 1 << 20
	defaultListEntries = 200
	maxListEntries     = 1000
	maxWriteBytes      = 1 << 20
)

// WorkspacePlugin 提供最小但可用于真实 coding-agent 任务的文件工具。
//
// 它有意不提供 shell/exec：第一阶段先让 Agent 能可靠地查看目录、读取文件
// 和写入文件，同时把文件系统信任边界固定在启动时的工作区根目录。后续如果
// 增加命令工具，应单独设计命令白名单、超时、输出上限和审批策略，不能把
// os/exec 直接暴露给模型。
type WorkspacePlugin struct {
	workspace workspaceRoot
}

var (
	_ entity.Plugin            = (*WorkspacePlugin)(nil)
	_ entity.WithRegisterTools = (*WorkspacePlugin)(nil)
)

// NewWorkspacePlugin 创建限定在 root 下工作的文件工具插件。
func NewWorkspacePlugin(root string) (*WorkspacePlugin, error) {
	workspace, err := newWorkspaceRoot(root)
	if err != nil {
		return nil, err
	}
	return &WorkspacePlugin{workspace: workspace}, nil
}

// ID 返回稳定插件标识。
func (*WorkspacePlugin) ID() entity.PluginID { return "pi/workspace" }

// RegisterTools 返回 read_file、write_file、list_files 三个工具。
func (p *WorkspacePlugin) RegisterTools() []entity.Tool {
	return []entity.Tool{
		readFileTool{workspace: p.workspace},
		writeFileTool{workspace: p.workspace},
		listFilesTool{workspace: p.workspace},
	}
}

type workspaceRoot struct {
	path string
}

func newWorkspaceRoot(root string) (workspaceRoot, error) {
	if strings.TrimSpace(root) == "" {
		return workspaceRoot{}, errors.New("workspace: 根目录不能为空")
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return workspaceRoot{}, fmt.Errorf("workspace: 解析根目录: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return workspaceRoot{}, fmt.Errorf("workspace: 解析根目录链接: %w", err)
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return workspaceRoot{}, fmt.Errorf("workspace: 检查根目录: %w", err)
	}
	if !info.IsDir() {
		return workspaceRoot{}, fmt.Errorf("workspace: 根路径不是目录: %s", root)
	}
	return workspaceRoot{path: resolved}, nil
}

func (w workspaceRoot) resolveExisting(userPath string) (string, error) {
	candidate, err := w.lexicalPath(userPath)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(candidate)
	if err != nil {
		return "", fmt.Errorf("workspace: 路径不存在或无法解析: %w", err)
	}
	if !w.contains(resolved) {
		return "", errors.New("workspace: 路径不能越出工作区")
	}
	return resolved, nil
}

func (w workspaceRoot) resolveForWrite(userPath string) (string, error) {
	candidate, err := w.lexicalPath(userPath)
	if err != nil {
		return "", err
	}
	parent := filepath.Dir(candidate)
	resolvedParent, err := w.resolveExistingAncestor(parent)
	if err != nil {
		return "", err
	}
	path := filepath.Join(resolvedParent, filepath.Base(candidate))
	if info, statErr := os.Lstat(path); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			resolved, linkErr := filepath.EvalSymlinks(path)
			if linkErr != nil || !w.contains(resolved) {
				return "", errors.New("workspace: 不允许写入指向工作区外的符号链接")
			}
			return resolved, nil
		}
	}
	if !w.contains(path) {
		return "", errors.New("workspace: 路径不能越出工作区")
	}
	return path, nil
}

func (w workspaceRoot) lexicalPath(userPath string) (string, error) {
	userPath = strings.TrimSpace(userPath)
	if userPath == "" || filepath.IsAbs(userPath) || !filepath.IsLocal(userPath) {
		return "", errors.New("workspace: path 必须是工作区内的相对路径")
	}
	candidate := filepath.Join(w.path, filepath.Clean(userPath))
	if !w.contains(candidate) {
		return "", errors.New("workspace: path 不能包含工作区外的路径")
	}
	return candidate, nil
}

func (w workspaceRoot) resolveExistingAncestor(path string) (string, error) {
	current := path
	for {
		if _, err := os.Stat(current); err == nil {
			resolved, resolveErr := filepath.EvalSymlinks(current)
			if resolveErr != nil || !w.contains(resolved) {
				return "", errors.New("workspace: 父目录不能越出工作区")
			}
			missing, relErr := filepath.Rel(current, path)
			if relErr != nil {
				return "", fmt.Errorf("workspace: 解析父目录: %w", relErr)
			}
			return filepath.Join(resolved, missing), nil
		}
		if current == w.path || filepath.Dir(current) == current {
			return "", errors.New("workspace: 找不到写入文件的父目录")
		}
		current = filepath.Dir(current)
	}
}

func (w workspaceRoot) contains(path string) bool {
	rel, err := filepath.Rel(w.path, path)
	if err != nil {
		return false
	}
	return rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))
}

type readFileTool struct{ workspace workspaceRoot }

func (readFileTool) Info() entity.Info {
	return entity.Info{
		Name: "read_file",
		Description: "读取工作区内的文本文件。先用 list_files 定位路径；path 必须是相对路径。" +
			"默认最多返回 128 KiB，超长内容会被截断并标记。不要用它读取密钥、" +
			"凭据或不相关的大文件。",
		InputSchema: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"工作区内相对文件路径，例如 internal/usecase/run.go"},
    "max_bytes":{"type":"integer","minimum":1,"maximum":1048576,"description":"最多读取字节数，默认 131072"}
  },
  "required":["path"],
  "additionalProperties":false
}`),
	}
}

func (t readFileTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Path     string `json:"path"`
		MaxBytes int    `json:"max_bytes"`
	}
	if err := entity.DecodeArguments(request, &args); err != nil {
		return toolError(err)
	}
	if err := ctx.Err(); err != nil {
		return toolError(err)
	}
	path, err := t.workspace.resolveExisting(args.Path)
	if err != nil {
		return toolError(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return toolError(err)
	}
	if info.IsDir() {
		return toolError(errors.New("read_file: path 是目录，请使用 list_files"))
	}
	limit := args.MaxBytes
	if limit == 0 {
		limit = defaultReadBytes
	}
	if limit < 1 || limit > maxReadBytes {
		return toolError(fmt.Errorf("read_file: max_bytes 必须在 1 到 %d 之间", maxReadBytes))
	}
	file, err := os.Open(path)
	if err != nil {
		return toolError(err)
	}
	defer func() { _ = file.Close() }()
	data, err := io.ReadAll(io.LimitReader(file, int64(limit)+1))
	if err != nil {
		return toolError(err)
	}
	if len(data) > limit {
		return entity.Result{Content: string(data[:limit]) + "\n\n[内容已截断，文件超过读取上限]"}
	}
	return entity.Result{Content: string(data)}
}

type writeFileTool struct{ workspace workspaceRoot }

func (writeFileTool) Info() entity.Info {
	return entity.Info{
		Name: "write_file",
		Description: "将完整文本写入工作区内的文件。path 必须是相对路径；工具会创建已存在的父目录，" +
			"并以原子替换方式写入。写入前先读取相关文件并确认目标，禁止写入凭据、构建产物" +
			"或工作区外路径；单次最多 1 MiB。",
		InputSchema: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"工作区内相对文件路径"},
    "content":{"type":"string","description":"要写入的完整文件内容"}
  },
  "required":["path","content"],
  "additionalProperties":false
}`),
	}
}

func (t writeFileTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := entity.DecodeArguments(request, &args); err != nil {
		return toolError(err)
	}
	if err := ctx.Err(); err != nil {
		return toolError(err)
	}
	if len(args.Content) > maxWriteBytes {
		return toolError(fmt.Errorf("write_file: content 超过 %d 字节上限", maxWriteBytes))
	}
	path, err := t.workspace.resolveForWrite(args.Path)
	if err != nil {
		return toolError(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return toolError(err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-write-*.tmp")
	if err != nil {
		return toolError(err)
	}
	tmpPath := tmp.Name()
	defer func() { _ = os.Remove(tmpPath) }()
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		return toolError(err)
	}
	if _, err = tmp.WriteString(args.Content); err != nil {
		_ = tmp.Close()
		return toolError(err)
	}
	if err == nil {
		err = tmp.Sync()
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return toolError(err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return toolError(err)
	}
	return entity.Result{Content: fmt.Sprintf("已写入 %s（%d 字节）", args.Path, len(args.Content))}
}

type listFilesTool struct{ workspace workspaceRoot }

func (listFilesTool) Info() entity.Info {
	return entity.Info{
		Name: "list_files",
		Description: "列出工作区内目录的直接子项，不递归读取内容。先用它探索目录，再用 read_file 读取具体文件；" +
			"path 省略时使用工作区根目录。",
		InputSchema: json.RawMessage(`{
  "type":"object",
  "properties":{
    "path":{"type":"string","description":"工作区内相对目录路径，默认 ."},
    "max_entries":{"type":"integer","minimum":1,"maximum":1000,"description":"最多返回条目数，默认 200"}
  },
  "additionalProperties":false
}`),
	}
}

func (t listFilesTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Path       string `json:"path"`
		MaxEntries int    `json:"max_entries"`
	}
	if err := entity.DecodeArguments(request, &args); err != nil {
		return toolError(err)
	}
	if err := ctx.Err(); err != nil {
		return toolError(err)
	}
	if args.Path == "" {
		args.Path = "."
	}
	path, err := t.workspace.resolveExisting(args.Path)
	if err != nil {
		return toolError(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return toolError(err)
	}
	if !info.IsDir() {
		return toolError(errors.New("list_files: path 不是目录"))
	}
	limit := args.MaxEntries
	if limit == 0 {
		limit = defaultListEntries
	}
	if limit < 1 || limit > maxListEntries {
		return toolError(fmt.Errorf("list_files: max_entries 必须在 1 到 %d 之间", maxListEntries))
	}
	directory, err := os.Open(path)
	if err != nil {
		return toolError(err)
	}
	defer func() { _ = directory.Close() }()
	entries, err := directory.ReadDir(limit + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return toolError(err)
	}
	truncated := len(entries) > limit
	if truncated {
		entries = entries[:limit]
	}
	var builder strings.Builder
	for _, entry := range entries {
		if entry.IsDir() {
			fmt.Fprintf(&builder, "dir  %s/\n", entry.Name())
		} else {
			fmt.Fprintf(&builder, "file %s\n", entry.Name())
		}
	}
	if truncated {
		fmt.Fprintf(&builder, "\n[仅显示前 %d 项，目录还有更多项]", limit)
	}
	return entity.Result{Content: strings.TrimRight(builder.String(), "\n")}
}

func toolError(err error) entity.Result {
	return entity.Result{Content: err.Error(), IsError: true}
}
