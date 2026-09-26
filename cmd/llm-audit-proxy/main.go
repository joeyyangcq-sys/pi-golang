// llm-audit-proxy records a redacted summary of benchmark requests while
// transparently forwarding streaming OpenAI-compatible traffic to an upstream.
package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const maxAuditBodyBytes = 16 << 20

type wireRecord struct {
	Sequence        uint64         `json:"sequence"`
	At              time.Time      `json:"at"`
	Runner          string         `json:"runner"`
	Method          string         `json:"method"`
	Path            string         `json:"path"`
	PayloadBytes    int            `json:"payload_bytes"`
	PayloadSHA256   string         `json:"payload_sha256"`
	CanonicalSHA256 string         `json:"canonical_sha256,omitempty"`
	Semantic        map[string]any `json:"semantic,omitempty"`
	ParseError      string         `json:"parse_error,omitempty"`
}

type auditWriter struct {
	mu       sync.Mutex
	file     *os.File
	sequence atomic.Uint64
}

func newAuditWriter(path string) (*auditWriter, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create audit directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open audit file: %w", err)
	}
	return &auditWriter{file: file}, nil
}

func (w *auditWriter) close() error { return w.file.Close() }

func (w *auditWriter) write(record wireRecord) {
	record.Sequence = w.sequence.Add(1)
	record.At = time.Now().UTC()
	payload, err := json.Marshal(record)
	if err != nil {
		log.Printf("encode wire audit: %v", err)
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if _, err := w.file.Write(append(payload, '\n')); err != nil {
		log.Printf("write wire audit: %v", err)
	}
}

func main() {
	listen := flag.String("listen", "127.0.0.1:1235", "local address to listen on")
	upstreamRaw := flag.String("upstream", "http://127.0.0.1:1234", "LM Studio or OpenAI-compatible upstream")
	auditFile := flag.String("audit-file", "wire-requests.jsonl", "redacted JSONL audit output")
	flag.Parse()

	upstream, err := url.Parse(*upstreamRaw)
	if err != nil || upstream.Scheme == "" || upstream.Host == "" {
		log.Fatalf("invalid --upstream %q", *upstreamRaw)
	}
	writer, err := newAuditWriter(*auditFile)
	if err != nil {
		log.Fatal(err)
	}
	defer func() { _ = writer.close() }()

	proxy := httputil.NewSingleHostReverseProxy(upstream)
	proxy.FlushInterval = -1 // preserve SSE token delivery timing
	proxy.ErrorHandler = func(response http.ResponseWriter, request *http.Request, err error) {
		log.Printf("proxy %s: %v", request.URL.Path, err)
		http.Error(response, "audit proxy upstream error", http.StatusBadGateway)
	}

	handler := &auditProxy{proxy: proxy, audit: writer}
	server := &http.Server{Addr: *listen, Handler: handler}
	log.Printf("llm audit proxy listening on %s -> %s", *listen, upstream.Redacted())
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

type auditProxy struct {
	proxy *httputil.ReverseProxy
	audit *auditWriter
}

func (p *auditProxy) ServeHTTP(response http.ResponseWriter, request *http.Request) {
	if request.URL.Path == "/healthz" {
		response.WriteHeader(http.StatusNoContent)
		return
	}
	runner, path, ok := runnerPath(request.URL.Path)
	if !ok {
		http.NotFound(response, request)
		return
	}

	if request.Body != nil && request.Method != http.MethodGet && request.Method != http.MethodHead {
		body, err := io.ReadAll(io.LimitReader(request.Body, maxAuditBodyBytes+1))
		_ = request.Body.Close()
		if err != nil {
			http.Error(response, "read request body", http.StatusBadRequest)
			return
		}
		if len(body) > maxAuditBodyBytes {
			http.Error(response, "request body exceeds audit proxy limit", http.StatusRequestEntityTooLarge)
			return
		}
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.ContentLength = int64(len(body))
		p.audit.write(makeWireRecord(runner, request.Method, path, body))
	}

	request.URL.Path = path
	request.URL.RawPath = ""
	p.proxy.ServeHTTP(response, request)
}

func runnerPath(path string) (runner, upstreamPath string, ok bool) {
	if strings.HasPrefix(path, "/go-") || strings.HasPrefix(path, "/pi-") {
		parts := strings.SplitN(strings.TrimPrefix(path, "/"), "/", 2)
		if len(parts) == 2 && parts[0] != "" {
			return parts[0], "/" + parts[1], true
		}
	}
	for _, candidate := range []string{"go", "pi"} {
		prefix := "/" + candidate
		if path == prefix {
			return candidate, "/", true
		}
		if strings.HasPrefix(path, prefix+"/") {
			return candidate, strings.TrimPrefix(path, prefix), true
		}
	}
	return "", "", false
}

func makeWireRecord(runner, method, path string, body []byte) wireRecord {
	record := wireRecord{
		Runner:        runner,
		Method:        method,
		Path:          path,
		PayloadBytes:  len(body),
		PayloadSHA256: hash(body),
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		record.ParseError = err.Error()
		return record
	}
	canonical, err := json.Marshal(payload)
	if err != nil {
		record.ParseError = err.Error()
		return record
	}
	record.CanonicalSHA256 = hash(canonical)
	if object, ok := payload.(map[string]any); ok {
		record.Semantic = redactRequest(object)
	}
	return record
}

func redactRequest(payload map[string]any) map[string]any {
	semantic := make(map[string]any, len(payload))
	keys := make([]string, 0, len(payload))
	for key := range payload {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		switch key {
		case "messages":
			semantic[key] = redactMessages(payload[key])
		case "tools":
			semantic[key] = redactValue(payload[key])
		default:
			semantic[key] = redactValue(payload[key])
		}
	}
	return semantic
}

func redactMessages(value any) any {
	messages, ok := value.([]any)
	if !ok {
		return redactValue(value)
	}
	result := make([]any, 0, len(messages))
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok {
			result = append(result, redactValue(raw))
			continue
		}
		item := make(map[string]any, len(message))
		for key, field := range message {
			if key == "role" {
				item[key] = field
			} else {
				item[key] = redactValue(field)
			}
		}
		result = append(result, item)
	}
	return result
}

func redactValue(value any) any {
	switch typed := value.(type) {
	case string:
		return map[string]any{"bytes": len(typed), "sha256": hash([]byte(typed))}
	case []any:
		result := make([]any, len(typed))
		for index, item := range typed {
			result[index] = redactValue(item)
		}
		return result
	case map[string]any:
		result := make(map[string]any, len(typed))
		for key, item := range typed {
			result[key] = redactValue(item)
		}
		return result
	default:
		return value
	}
}

func hash(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}
