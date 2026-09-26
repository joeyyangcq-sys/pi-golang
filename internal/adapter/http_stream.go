package adapter

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"
	"time"

	"pi-golang/internal/entity"
)

const (
	defaultHTTPTimeout          = 60 * time.Second
	defaultStreamOverallTimeout = 10 * time.Minute
	maxStreamEventBytes         = 1 << 20
	maxStreamResponseBytes      = 16 << 20
	maxJSONResponseBytes        = 1 << 20
)

// serverSentEvent is the provider-neutral SSE envelope. Adapters own only the
// JSON protocol inside Data; framing, limits, cancellation, and HTTP errors are
// handled once by doStreamingRequest.
type serverSentEvent struct {
	Type string
	Data []byte
}

type streamResponseHandlers struct {
	SSE  func(serverSentEvent) (streamEventProgress, error)
	JSON func([]byte) error
}

// StreamingHTTPConfig controls defaults used when a provider creates its own
// HTTP client. A caller-supplied *http.Client remains authoritative, so tests
// and deployments can provide custom transports or context policies directly.
type StreamingHTTPConfig struct {
	DialTimeout           time.Duration
	ResponseHeaderTimeout time.Duration
	OverallTimeout        time.Duration
}

// streamEventProgress lets the shared transport distinguish protocol heartbeats
// from actual model activity without understanding any provider's JSON shape.
type streamEventProgress struct {
	ModelEvent  bool
	VisibleText bool
}

func newStreamingHTTPClientWithConfig(client *http.Client, config StreamingHTTPConfig) *http.Client {
	if client != nil {
		return client
	}
	baseTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		if config.OverallTimeout <= 0 {
			config.OverallTimeout = defaultStreamOverallTimeout
		}
		return &http.Client{Transport: http.DefaultTransport, Timeout: config.OverallTimeout}
	}
	transport := baseTransport.Clone()
	if config.DialTimeout > 0 {
		dialer := &net.Dialer{Timeout: config.DialTimeout, KeepAlive: 30 * time.Second}
		transport.DialContext = dialer.DialContext
	}
	if config.ResponseHeaderTimeout <= 0 {
		config.ResponseHeaderTimeout = defaultHTTPTimeout
	}
	if config.OverallTimeout <= 0 {
		config.OverallTimeout = defaultStreamOverallTimeout
	}
	transport.ResponseHeaderTimeout = config.ResponseHeaderTimeout
	return &http.Client{Transport: transport, Timeout: config.OverallTimeout}
}

// doStreamingRequest owns the complete HTTP/SSE lifecycle. JSON is retained as
// a compatibility fallback for gateways that accept stream=true but return a
// conventional response.
func doStreamingRequest(
	ctx context.Context,
	client *http.Client,
	request *http.Request,
	provider string,
	handlers streamResponseHandlers,
) (metadata entity.LLMCallMetadata, err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	started := time.Now()
	metadata = entity.LLMCallMetadata{Provider: provider, Attempts: 1}
	defer func() {
		metadata.Duration = time.Since(started)
		var llmErr *entity.LLMError
		if errors.As(err, &llmErr) && llmErr != nil {
			llmErr.Metadata.Duration = metadata.Duration
		}
	}()

	var wroteRequest, gotFirstResponseByte bool
	trace := &httptrace.ClientTrace{
		WroteRequest: func(httptrace.WroteRequestInfo) { wroteRequest = true },
		GotFirstResponseByte: func() {
			gotFirstResponseByte = true
			if metadata.TimeToFirstByte == 0 {
				metadata.TimeToFirstByte = time.Since(started)
			}
		},
	}
	request = request.WithContext(httptrace.WithClientTrace(ctx, trace))
	response, err := client.Do(request)
	if err != nil {
		class := entity.LLMErrorTransport
		if errors.Is(err, context.Canceled) {
			class = entity.LLMErrorCanceled
		} else if isTimeoutError(err) {
			class = entity.LLMErrorTimeout
			metadata.TimeoutPhase = timeoutPhase(wroteRequest, gotFirstResponseByte)
		}
		return metadata, &entity.LLMError{
			Class: class, Metadata: metadata, Err: fmt.Errorf("%s: 请求失败: %w", provider, err),
		}
	}
	defer func() { _ = response.Body.Close() }()
	metadata.HTTPStatus = response.StatusCode

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := io.ReadAll(io.LimitReader(response.Body, 4097))
		if readErr != nil {
			return metadata, responseReadError(provider, metadata, readErr)
		}
		class := entity.LLMErrorHTTP4xx
		if response.StatusCode >= http.StatusInternalServerError {
			class = entity.LLMErrorHTTP5xx
		}
		return metadata, &entity.LLMError{
			Class: class, Metadata: metadata,
			Err: fmt.Errorf("%s: HTTP %d: %s", provider, response.StatusCode, truncateBody(body, 4096)),
		}
	}

	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if strings.Contains(contentType, "text/event-stream") {
		if handlers.SSE == nil {
			return metadata, protocolError(provider, metadata, errors.New("未配置 SSE 处理器"))
		}
		seenModelEvent := false
		seenVisibleText := false
		consume := func(event serverSentEvent) error {
			progress, consumeErr := handlers.SSE(event)
			if progress.ModelEvent && !seenModelEvent {
				seenModelEvent = true
				metadata.TimeToFirstEvent = time.Since(started)
			}
			if progress.VisibleText && !seenVisibleText {
				seenVisibleText = true
				metadata.TimeToFirstContent = time.Since(started)
			}
			return consumeErr
		}
		if streamErr := readServerSentEvents(response.Body, consume); streamErr != nil {
			var readErr *streamBodyReadError
			if errors.As(streamErr, &readErr) || errors.Is(streamErr, context.Canceled) || isTimeoutError(streamErr) {
				return metadata, responseReadError(provider, metadata, streamErr)
			}
			return metadata, protocolError(provider, metadata, streamErr)
		}
		return metadata, nil
	}

	if handlers.JSON == nil {
		return metadata, protocolError(provider, metadata, fmt.Errorf("不支持的 Content-Type %q", contentType))
	}
	body, readErr := readBounded(response.Body, maxJSONResponseBytes)
	if readErr != nil {
		var tooLarge *responseTooLargeError
		if errors.As(readErr, &tooLarge) {
			return metadata, protocolError(provider, metadata, readErr)
		}
		return metadata, responseReadError(provider, metadata, readErr)
	}
	if decodeErr := handlers.JSON(body); decodeErr != nil {
		return metadata, protocolError(provider, metadata, fmt.Errorf("解码响应: %w", decodeErr))
	}
	return metadata, nil
}

type streamBodyReadError struct{ err error }

func (e *streamBodyReadError) Error() string { return e.err.Error() }
func (e *streamBodyReadError) Unwrap() error { return e.err }

type responseTooLargeError struct {
	limit int64
}

func (e *responseTooLargeError) Error() string {
	return fmt.Sprintf("响应超过 %d bytes", e.limit)
}

func readServerSentEvents(reader io.Reader, consume func(serverSentEvent) error) error {
	var eventType string
	var dataLines []string
	var eventBytes, totalBytes int

	emit := func() error {
		if len(dataLines) == 0 {
			eventType = ""
			eventBytes = 0
			return nil
		}
		event := serverSentEvent{
			Type: eventType,
			Data: []byte(strings.Join(dataLines, "\n")),
		}
		eventType = ""
		dataLines = dataLines[:0]
		eventBytes = 0
		return consume(event)
	}

	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 4096), maxStreamEventBytes)
	for scanner.Scan() {
		line := scanner.Text()
		lineBytes := len(line) + 1
		totalBytes += lineBytes
		eventBytes += lineBytes
		if totalBytes > maxStreamResponseBytes {
			return fmt.Errorf("流式响应超过 %d bytes", maxStreamResponseBytes)
		}
		if eventBytes > maxStreamEventBytes {
			return fmt.Errorf("SSE 事件超过 %d bytes", maxStreamEventBytes)
		}
		if line == "" {
			if err := emit(); err != nil {
				return err
			}
			continue
		}
		if strings.HasPrefix(line, ":") {
			continue
		}
		field, value, found := strings.Cut(line, ":")
		if found {
			value = strings.TrimPrefix(value, " ")
		}
		switch field {
		case "event":
			eventType = value
		case "data":
			dataLines = append(dataLines, value)
		}
	}
	if err := scanner.Err(); err != nil {
		return &streamBodyReadError{err: err}
	}
	return emit()
}

func readBounded(reader io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, &responseTooLargeError{limit: limit}
	}
	return body, nil
}

func timeoutPhase(wroteRequest, gotFirstResponseByte bool) string {
	if !wroteRequest {
		return "connect"
	}
	if !gotFirstResponseByte {
		return "response_headers"
	}
	return "response_body"
}

func isTimeoutError(err error) bool {
	return errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "Client.Timeout")
}

func responseReadError(provider string, metadata entity.LLMCallMetadata, err error) error {
	class := entity.LLMErrorResponseRead
	if errors.Is(err, context.Canceled) {
		class = entity.LLMErrorCanceled
	} else if isTimeoutError(err) {
		class = entity.LLMErrorTimeout
		metadata.TimeoutPhase = "response_body"
	}
	return &entity.LLMError{
		Class: class, Metadata: metadata, Err: fmt.Errorf("%s: 读取响应: %w", provider, err),
	}
}

func protocolError(provider string, metadata entity.LLMCallMetadata, err error) error {
	return &entity.LLMError{
		Class:    entity.LLMErrorProtocol,
		Metadata: metadata,
		Err:      fmt.Errorf("%s: 解析响应: %w", provider, err),
	}
}
