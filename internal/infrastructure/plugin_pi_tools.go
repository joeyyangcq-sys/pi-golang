package infrastructure

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"pi-golang/internal/entity"
)

const (
	piToolMaxOutputBytes = 50 << 10
	piToolMaxOutputLines = 2000
	piToolDefaultTimeout = 120 * time.Second
	piToolMaxTimeout     = 20 * time.Minute
)

type piReadTool struct{ workspace workspaceRoot }

func (piReadTool) Info() entity.Info {
	return entity.Info{
		Name:        "read",
		Access:      entity.ToolAccessRead,
		Description: "Read the contents of a file. Supports text files. Output is truncated to 2000 lines or 50KB (whichever is hit first). Use offset/limit for large files. When you need the full file, continue with offset until complete.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path"],"properties":{"path":{"type":"string","description":"Path to the file to read (relative or absolute)"},"offset":{"type":"number","description":"Line number to start reading from (1-indexed)"},"limit":{"type":"number","description":"Maximum number of lines to read"}}}`),
	}
}

func (t piReadTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Path   string `json:"path"`
		Offset int    `json:"offset"`
		Limit  int    `json:"limit"`
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
		return toolError(errors.New("read: path is a directory; use ls"))
	}
	offset := args.Offset
	if offset == 0 {
		offset = 1
	}
	limit := args.Limit
	if limit == 0 || limit > piToolMaxOutputLines {
		limit = piToolMaxOutputLines
	}
	if offset < 1 || limit < 1 {
		return toolError(errors.New("read: offset and limit must be positive"))
	}
	file, err := os.Open(path)
	if err != nil {
		return toolError(err)
	}
	defer func() { _ = file.Close() }()

	var out strings.Builder
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), 1<<20)
	lineNo := 0
	emitted := 0
	truncated := false
	for scanner.Scan() {
		lineNo++
		if lineNo < offset {
			continue
		}
		line := scanner.Text()
		if !utf8.ValidString(line) {
			return toolError(errors.New("read: binary files are not supported"))
		}
		formatted := fmt.Sprintf("%d: %s\n", lineNo, line)
		if emitted >= limit || out.Len()+len(formatted) > piToolMaxOutputBytes {
			truncated = true
			break
		}
		out.WriteString(formatted)
		emitted++
	}
	if err := scanner.Err(); err != nil {
		return toolError(err)
	}
	if truncated {
		fmt.Fprintf(&out, "[Output truncated; continue with offset=%d]", lineNo)
	}
	return entity.Result{Content: strings.TrimRight(out.String(), "\n")}
}

type piWriteTool struct{ workspace workspaceRoot }

func (piWriteTool) Info() entity.Info {
	return entity.Info{
		Name:        "write",
		Access:      entity.ToolAccessMutate,
		Description: "Write content to a file. Creates the file if it doesn't exist, overwrites if it does. Automatically creates parent directories.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path","content"],"properties":{"path":{"type":"string","description":"Path to the file to write (relative or absolute)"},"content":{"type":"string","description":"Content to write to the file"}}}`),
	}
}

func (t piWriteTool) Call(ctx context.Context, request entity.Request) entity.Result {
	return writeFileTool{workspace: t.workspace}.Call(ctx, request)
}

type piEditTool struct{ workspace workspaceRoot }

func (piEditTool) Info() entity.Info {
	return entity.Info{
		Name:        "edit",
		Access:      entity.ToolAccessMutate,
		Description: "Edit a single file using exact text replacement. Every edits[].oldText must match a unique, non-overlapping region of the original file. If two changes affect the same block or nearby lines, merge them into one edit instead of emitting overlapping edits. Do not include large unchanged regions just to connect distant changes.",
		InputSchema: json.RawMessage(`{"type":"object","required":["path","edits"],"properties":{"path":{"type":"string","description":"Path to the file to edit (relative or absolute)"},"edits":{"type":"array","items":{"type":"object","required":["oldText","newText"],"properties":{"oldText":{"type":"string","description":"Exact text for one targeted replacement. It must be unique in the original file and must not overlap with any other edits[].oldText in the same call."},"newText":{"type":"string","description":"Replacement text for this targeted edit."}}},"description":"One or more targeted replacements. Each edit is matched against the original file, not incrementally. Do not include overlapping or nested edits. If two changes touch the same block or nearby lines, merge them into one edit instead."}}}`),
	}
}

func (t piEditTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Path  string `json:"path"`
		Edits []struct {
			OldText string `json:"oldText"`
			NewText string `json:"newText"`
		} `json:"edits"`
	}
	if err := entity.DecodeArguments(request, &args); err != nil {
		return toolError(err)
	}
	if err := ctx.Err(); err != nil {
		return toolError(err)
	}
	if len(args.Edits) == 0 {
		return toolError(errors.New("edit: edits must not be empty"))
	}
	path, err := t.workspace.resolveExisting(args.Path)
	if err != nil {
		return toolError(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return toolError(err)
	}
	if len(data) > maxWriteBytes {
		return toolError(fmt.Errorf("edit: file exceeds %d byte limit", maxWriteBytes))
	}
	original := string(data)
	type replacement struct {
		start int
		end   int
		text  string
	}
	replacements := make([]replacement, 0, len(args.Edits))
	for index, edit := range args.Edits {
		if edit.OldText == "" {
			return toolError(fmt.Errorf("edit: edits[%d].oldText must not be empty", index))
		}
		if strings.Count(original, edit.OldText) != 1 {
			return toolError(fmt.Errorf("edit: edits[%d].oldText must match exactly once", index))
		}
		start := strings.Index(original, edit.OldText)
		replacements = append(replacements, replacement{start: start, end: start + len(edit.OldText), text: edit.NewText})
	}
	sort.Slice(replacements, func(i, j int) bool { return replacements[i].start < replacements[j].start })
	for index := 1; index < len(replacements); index++ {
		if replacements[index].start < replacements[index-1].end {
			return toolError(errors.New("edit: replacement regions overlap"))
		}
	}
	updated := original
	for index := len(replacements) - 1; index >= 0; index-- {
		replacement := replacements[index]
		updated = updated[:replacement.start] + replacement.text + updated[replacement.end:]
	}
	encoded, err := json.Marshal(struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}{Path: args.Path, Content: updated})
	if err != nil {
		return toolError(err)
	}
	result := writeFileTool{workspace: t.workspace}.Call(ctx, entity.Request{Name: "write", Arguments: encoded})
	if !result.IsError {
		result.Content = fmt.Sprintf("Successfully applied %d edit(s) to %s", len(args.Edits), args.Path)
	}
	return result
}

type piLSTool struct{ workspace workspaceRoot }

func (piLSTool) Info() entity.Info {
	return entity.Info{
		Name:        "ls",
		Access:      entity.ToolAccessRead,
		Description: "List directory contents. Returns entries sorted alphabetically, with '/' suffix for directories. Includes dotfiles. Output is truncated to 500 entries or 50KB (whichever is hit first).",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string","description":"Directory to list (default: current directory)"},"limit":{"type":"number","description":"Maximum number of entries to return (default: 500)"}}}`),
	}
}

func (t piLSTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Path  string `json:"path"`
		Limit int    `json:"limit"`
	}
	if err := entity.DecodeArguments(request, &args); err != nil {
		return toolError(err)
	}
	if err := ctx.Err(); err != nil {
		return toolError(err)
	}
	if strings.TrimSpace(args.Path) == "" {
		args.Path = "."
	}
	limit := args.Limit
	if limit == 0 || limit > 500 {
		limit = 500
	}
	if limit < 1 {
		return toolError(errors.New("ls: limit must be positive"))
	}
	path, err := t.workspace.resolveExisting(args.Path)
	if err != nil {
		return toolError(err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return toolError(err)
	}
	var out strings.Builder
	for index, entry := range entries {
		if index >= limit {
			out.WriteString("[Output truncated]\n")
			break
		}
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		if out.Len()+len(name)+1 > piToolMaxOutputBytes {
			out.WriteString("[Output truncated]\n")
			break
		}
		out.WriteString(name)
		out.WriteByte('\n')
	}
	return entity.Result{Content: strings.TrimRight(out.String(), "\n")}
}

type piFindTool struct{ workspace workspaceRoot }

func (piFindTool) Info() entity.Info {
	return entity.Info{
		Name:        "find",
		Access:      entity.ToolAccessRead,
		Description: "Search for files by glob pattern. Returns matching file paths relative to the search directory. Output is truncated to 1000 results or 50KB (whichever is hit first).",
		InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string","description":"Glob pattern to match files, e.g. '*.ts', '**/*.json', or 'src/**/*.spec.ts'"},"path":{"type":"string","description":"Directory to search in (default: current directory)"},"limit":{"type":"number","description":"Maximum number of results (default: 1000)"}}}`),
	}
}

func (t piFindTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Limit   int    `json:"limit"`
	}
	if err := entity.DecodeArguments(request, &args); err != nil {
		return toolError(err)
	}
	if strings.TrimSpace(args.Path) == "" {
		args.Path = "."
	}
	limit := args.Limit
	if limit == 0 || limit > 1000 {
		limit = 1000
	}
	if strings.TrimSpace(args.Pattern) == "" || limit < 1 {
		return toolError(errors.New("find: pattern and positive limit are required"))
	}
	root, err := t.workspace.resolveExisting(args.Path)
	if err != nil {
		return toolError(err)
	}
	matcher, err := compileGlob(args.Pattern)
	if err != nil {
		return toolError(err)
	}
	results := make([]string, 0)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			if path != root && shouldSkipDirectory(entry.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if matcher.MatchString(rel) {
			results = append(results, rel)
			if len(results) >= limit {
				return errResultLimit
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errResultLimit) {
		return toolError(err)
	}
	sort.Strings(results)
	return entity.Result{Content: truncateToolOutput(strings.Join(results, "\n"))}
}

type piGrepTool struct{ workspace workspaceRoot }

func (piGrepTool) Info() entity.Info {
	return entity.Info{
		Name:        "grep",
		Access:      entity.ToolAccessRead,
		Description: "Search file contents for a pattern. Returns matching lines with file paths and line numbers. Output is truncated to 100 matches or 50KB (whichever is hit first). Long lines are truncated to 500 chars.",
		InputSchema: json.RawMessage(`{"type":"object","required":["pattern"],"properties":{"pattern":{"type":"string","description":"Search pattern (regex or literal string)"},"path":{"type":"string","description":"Directory or file to search (default: current directory)"},"glob":{"type":"string","description":"Filter files by glob pattern, e.g. '*.ts' or '**/*.spec.ts'"},"ignoreCase":{"type":"boolean","description":"Case-insensitive search (default: false)"},"literal":{"type":"boolean","description":"Treat pattern as literal string instead of regex (default: false)"},"context":{"type":"number","description":"Number of lines to show before and after each match (default: 0)"},"limit":{"type":"number","description":"Maximum number of matches to return (default: 100)"}}}`),
	}
}

func (t piGrepTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Pattern    string `json:"pattern"`
		Path       string `json:"path"`
		Glob       string `json:"glob"`
		IgnoreCase bool   `json:"ignoreCase"`
		Literal    bool   `json:"literal"`
		Context    int    `json:"context"`
		Limit      int    `json:"limit"`
	}
	if err := entity.DecodeArguments(request, &args); err != nil {
		return toolError(err)
	}
	if strings.TrimSpace(args.Path) == "" {
		args.Path = "."
	}
	if args.Context < 0 || args.Context > 20 {
		return toolError(errors.New("grep: context must be between 0 and 20"))
	}
	limit := args.Limit
	if limit == 0 || limit > 100 {
		limit = 100
	}
	pattern := args.Pattern
	if args.Literal {
		pattern = regexp.QuoteMeta(pattern)
	}
	if args.IgnoreCase {
		pattern = "(?i)" + pattern
	}
	matcher, err := regexp.Compile(pattern)
	if err != nil {
		return toolError(fmt.Errorf("grep: invalid pattern: %w", err))
	}
	var glob *regexp.Regexp
	if strings.TrimSpace(args.Glob) != "" {
		glob, err = compileGlob(args.Glob)
		if err != nil {
			return toolError(err)
		}
	}
	root, err := t.workspace.resolveExisting(args.Path)
	if err != nil {
		return toolError(err)
	}
	paths := make([]string, 0)
	info, err := os.Stat(root)
	if err != nil {
		return toolError(err)
	}
	if !info.IsDir() {
		paths = append(paths, root)
	} else {
		err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.IsDir() {
				if path != root && shouldSkipDirectory(entry.Name()) {
					return filepath.SkipDir
				}
				return nil
			}
			paths = append(paths, path)
			return nil
		})
		if err != nil {
			return toolError(err)
		}
	}
	var out strings.Builder
	matches := 0
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return toolError(err)
		}
		rel, _ := filepath.Rel(root, path)
		if !info.IsDir() {
			rel = filepath.Base(path)
		}
		rel = filepath.ToSlash(rel)
		if glob != nil && !glob.MatchString(rel) {
			continue
		}
		data, readErr := os.ReadFile(path)
		if readErr != nil || len(data) > 4<<20 || !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for lineIndex, line := range lines {
			if !matcher.MatchString(line) {
				continue
			}
			start := maxInt(0, lineIndex-args.Context)
			end := minInt(len(lines), lineIndex+args.Context+1)
			for contextIndex := start; contextIndex < end; contextIndex++ {
				text := lines[contextIndex]
				if len(text) > 500 {
					text = text[:500] + "…"
				}
				fmt.Fprintf(&out, "%s:%d:%s\n", rel, contextIndex+1, text)
			}
			matches++
			if matches >= limit || out.Len() >= piToolMaxOutputBytes {
				out.WriteString("[Output truncated]\n")
				return entity.Result{Content: truncateToolOutput(out.String())}
			}
		}
	}
	return entity.Result{Content: strings.TrimRight(out.String(), "\n")}
}

type piBashTool struct{ workspace workspaceRoot }

func (piBashTool) Info() entity.Info {
	return entity.Info{
		Name:        "bash",
		Access:      entity.ToolAccessExecute,
		Description: "Execute a bash command in the current working directory. Returns stdout and stderr. Output is truncated to last 2000 lines or 50KB (whichever is hit first). Optionally provide a timeout in seconds.",
		InputSchema: json.RawMessage(`{"type":"object","required":["command"],"properties":{"command":{"type":"string","description":"Shell command to execute"},"timeout":{"type":"number","description":"Timeout in seconds (optional, default 120, maximum 1200)"}}}`),
	}
}

func (t piBashTool) Call(ctx context.Context, request entity.Request) entity.Result {
	var args struct {
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"`
	}
	if err := entity.DecodeArguments(request, &args); err != nil {
		return toolError(err)
	}
	if strings.TrimSpace(args.Command) == "" {
		return toolError(errors.New("bash: command must not be empty"))
	}
	timeout := piToolDefaultTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout * float64(time.Second))
	}
	if timeout <= 0 || timeout > piToolMaxTimeout {
		return toolError(fmt.Errorf("bash: timeout must be at most %s", piToolMaxTimeout))
	}
	commandCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandCtx, "bash", "-lc", args.Command)
	command.Dir = t.workspace.path
	command.Env = sanitizedCommandEnvironment(os.Environ(), t.workspace.path)
	var stdout, stderr tailCapture
	stdout.limit = piToolMaxOutputBytes
	stderr.limit = piToolMaxOutputBytes
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	content := formatCommandOutput(stdout.String(), stderr.String())
	if errors.Is(commandCtx.Err(), context.DeadlineExceeded) {
		return entity.Result{Content: content + "\ncommand timed out after " + timeout.String(), IsError: true}
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return entity.Result{Content: content + "\nexit code: " + strconv.Itoa(exitErr.ExitCode()), IsError: true}
		}
		return toolError(err)
	}
	if content == "" {
		content = "Command completed successfully with no output."
	}
	return entity.Result{Content: content}
}

var (
	errResultLimit = errors.New("result limit reached")
	sensitiveEnv   = regexp.MustCompile(`(?i)(api[_-]?key|token|secret|password|credential|authorization|cookie)`)
)

func compileGlob(pattern string) (*regexp.Regexp, error) {
	pattern = filepath.ToSlash(strings.TrimSpace(pattern))
	if pattern == "" {
		return nil, errors.New("glob pattern must not be empty")
	}
	var expression strings.Builder
	expression.WriteByte('^')
	for index := 0; index < len(pattern); index++ {
		switch pattern[index] {
		case '*':
			if index+1 < len(pattern) && pattern[index+1] == '*' {
				index++
				if index+1 < len(pattern) && pattern[index+1] == '/' {
					index++
					expression.WriteString(`(?:.*/)?`)
				} else {
					expression.WriteString(`.*`)
				}
			} else {
				expression.WriteString(`[^/]*`)
			}
		case '?':
			expression.WriteString(`[^/]`)
		default:
			expression.WriteString(regexp.QuoteMeta(string(pattern[index])))
		}
	}
	expression.WriteByte('$')
	return regexp.Compile(expression.String())
}

func shouldSkipDirectory(name string) bool {
	return name == ".git" || name == "node_modules"
}

func truncateToolOutput(value string) string {
	value = strings.TrimRight(value, "\n")
	lines := strings.Split(value, "\n")
	truncated := false
	if len(lines) > piToolMaxOutputLines {
		lines = lines[len(lines)-piToolMaxOutputLines:]
		truncated = true
	}
	value = strings.Join(lines, "\n")
	if len(value) > piToolMaxOutputBytes {
		value = value[len(value)-piToolMaxOutputBytes:]
		truncated = true
	}
	if truncated {
		return "[Earlier output truncated]\n" + value
	}
	return value
}

func sanitizedCommandEnvironment(environment []string, workspace string) []string {
	result := make([]string, 0, len(environment)+1)
	for _, item := range environment {
		key, _, found := strings.Cut(item, "=")
		if found && sensitiveEnv.MatchString(key) {
			continue
		}
		result = append(result, item)
	}
	return append(result, "PI_AGENT_WORKSPACE="+workspace)
}

func formatCommandOutput(stdout, stderr string) string {
	parts := make([]string, 0, 2)
	if stdout = truncateToolOutput(stdout); stdout != "" {
		parts = append(parts, "stdout:\n"+stdout)
	}
	if stderr = truncateToolOutput(stderr); stderr != "" {
		parts = append(parts, "stderr:\n"+stderr)
	}
	return strings.Join(parts, "\n")
}

type tailCapture struct {
	data  []byte
	limit int
}

func (w *tailCapture) Write(data []byte) (int, error) {
	w.data = append(w.data, data...)
	if overflow := len(w.data) - w.limit; overflow > 0 {
		copy(w.data, w.data[overflow:])
		w.data = w.data[:w.limit]
	}
	return len(data), nil
}

func (w *tailCapture) String() string { return string(w.data) }

func minInt(left, right int) int {
	if left < right {
		return left
	}
	return right
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
