package harpoon

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/invopop/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"go.openai.org/api/tunnel-client/pkg/config"
	tclog "go.openai.org/api/tunnel-client/pkg/log"
	"go.openai.org/api/tunnel-client/pkg/transport"
)

const (
	defaultTimeout      = 30 * time.Second
	minTimeout          = 100 * time.Millisecond
	maxTimeout          = 120 * time.Second
	maxBodyLogFieldName = "response_bytes"
)

var (
	allowedMethods = map[string]struct{}{
		http.MethodGet:  {},
		http.MethodPost: {},
		http.MethodPut:  {},
	}
	listTargetsSchema = &jsonschema.Schema{Type: "object"}
	callTargetSchema  = buildCallTargetSchema()
)

// Server provides MCP tools for constrained HTTP access.
type Server struct {
	logger        *slog.Logger
	registry      *Registry
	cfg           *config.HarpoonConfig
	httpTransport http.RoundTripper
}

type callTargetRequest struct {
	Label            string            `json:"label"`
	Path             string            `json:"path"`
	Method           string            `json:"method"`
	Headers          map[string]string `json:"headers,omitempty"`
	Body             string            `json:"body,omitempty"`
	TimeoutMS        *int              `json:"timeout_ms,omitempty"`
	MaxResponseBytes *int              `json:"max_response_bytes,omitempty"`
	FollowRedirects  *bool             `json:"follow_redirects,omitempty"`
	MaxRedirects     *int              `json:"max_redirects,omitempty"`
}

type callTargetResponse struct {
	StatusCode int                 `json:"status_code"`
	Headers    map[string][]string `json:"headers,omitempty"`
	BodyBase64 string              `json:"body_base64,omitempty"`
	BodySize   int                 `json:"body_size_bytes"`
	Truncated  bool                `json:"truncated,omitempty"`
}

type listTargetsResponse struct {
	Targets []targetInfo `json:"targets"`
}

type targetInfo struct {
	Label          string   `json:"label"`
	Description    string   `json:"description,omitempty"`
	AllowedMethods []string `json:"allowed_methods"`
}

// NewServer constructs a harpoon MCP server.
func NewServer(cfg *config.HarpoonConfig, registry *Registry, logger *slog.Logger) (*Server, error) {
	if cfg == nil {
		return nil, errors.New("harpoon: config is required")
	}
	if registry == nil {
		return nil, errors.New("harpoon: registry is required")
	}
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		logger:        logger.With(tclog.FieldComponent, tclog.ComponentHarpoon),
		registry:      registry,
		cfg:           cfg,
		httpTransport: transport.CloneDefault(),
	}, nil
}

// MCPServer builds an MCP server with harpoon tools registered.
func (s *Server) MCPServer() *mcp.Server {
	server := mcp.NewServer(&mcp.Implementation{Name: "harpoon", Version: "1.0.0"}, nil)
	mcp.AddTool(server, &mcp.Tool{
		Name:        "list_targets",
		Description: "List available harpoon targets by label.",
		InputSchema: listTargetsSchema,
	}, s.listTargetsHandler())
	mcp.AddTool(server, &mcp.Tool{
		Name:        "call_target",
		Description: "Call an allowlisted HTTP target by label.",
		InputSchema: callTargetSchema,
	}, s.callTargetHandler())
	return server
}

func (s *Server) listTargetsHandler() mcp.ToolHandlerFor[map[string]any, map[string]any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		resp := s.listTargets()
		structured := map[string]any{"targets": resp.Targets}
		payload, err := json.Marshal(resp)
		if err != nil {
			return toolErrorResult("", "failed to encode response"), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}}}, structured, nil
	}
}

func (s *Server) callTargetHandler() mcp.ToolHandlerFor[map[string]any, map[string]any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, map[string]any, error) {
		var params callTargetRequest
		if err := decodeArguments(args, &params); err != nil {
			return toolErrorResult("", "invalid parameters"), nil, nil
		}
		resp, err := s.callTarget(ctx, params)
		if err != nil {
			if toolErr := asToolError(err); toolErr != nil {
				return toolErrorResult(toolErr.label, toolErr.msg), nil, nil
			}
			return toolErrorResult(params.Label, "request failed"), nil, nil
		}
		structured := map[string]any{
			"status_code":     resp.StatusCode,
			"headers":         resp.Headers,
			"body_base64":     resp.BodyBase64,
			"body_size_bytes": resp.BodySize,
			"truncated":       resp.Truncated,
		}
		payload, err := json.Marshal(resp)
		if err != nil {
			return toolErrorResult(params.Label, "failed to encode response"), nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(payload)}}}, structured, nil
	}
}

func (s *Server) listTargets() listTargetsResponse {
	allowed := allowedMethodsList()
	targets := s.registry.Targets()
	out := make([]targetInfo, 0, len(targets))
	for _, target := range targets {
		out = append(out, targetInfo{
			Label:          target.Label,
			Description:    target.Description,
			AllowedMethods: allowed,
		})
	}
	return listTargetsResponse{Targets: out}
}

func (s *Server) callTarget(ctx context.Context, params callTargetRequest) (*callTargetResponse, error) {
	logger := tclog.LoggerWithContextIdentifiers(ctx, s.logger)

	label := strings.TrimSpace(params.Label)
	if label == "" {
		return nil, newToolError(label, "label is required")
	}

	if _, ok := s.registry.Lookup(label); !ok {
		return nil, newToolError(label, "unknown target")
	}

	method := strings.ToUpper(strings.TrimSpace(params.Method))
	if _, ok := allowedMethods[method]; !ok {
		return nil, newToolError(label, "invalid method")
	}

	resolved, err := s.registry.Resolve(label, params.Path)
	if err != nil {
		return nil, newToolError(label, "invalid path")
	}

	timeout, err := normalizeTimeout(params.TimeoutMS)
	if err != nil {
		return nil, newToolError(label, err.Error())
	}

	maxResponseBytes, err := s.normalizeMaxResponseBytes(params.MaxResponseBytes)
	if err != nil {
		return nil, newToolError(label, err.Error())
	}

	maxRedirects, followRedirects, err := s.normalizeRedirects(params.FollowRedirects, params.MaxRedirects)
	if err != nil {
		return nil, newToolError(label, err.Error())
	}

	bodyBytes := []byte(params.Body)
	if len(bodyBytes) > maxResponseBytes {
		return nil, newToolError(label, "request body exceeds size limit")
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, method, resolved.String(), bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, newToolError(label, "request failed")
	}
	for key, value := range params.Headers {
		if strings.TrimSpace(key) == "" {
			continue
		}
		req.Header.Set(key, value)
	}

	client := &http.Client{Transport: s.httpTransport}
	client.CheckRedirect = s.redirectPolicy(label, maxRedirects, followRedirects)

	resp, err := client.Do(req)
	if err != nil {
		cause := classifyRequestError(err)
		logger.InfoContext(ctx, "harpoon request failed",
			slog.String("label", label),
			slog.String("url", resolved.String()),
			slog.String("method", method),
			slog.String("error", cause),
			slog.Int("request_bytes", len(bodyBytes)),
			slog.Int("status_code", 0),
			slog.Int(maxBodyLogFieldName, 0),
		)
		return nil, newToolError(label, cause)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			logger.DebugContext(ctx, "harpoon response close failed", slog.String("error", err.Error()))
		}
	}()

	body, tooLarge, readErr := readLimited(resp.Body, maxResponseBytes)
	if readErr != nil {
		logger.InfoContext(ctx, "harpoon response read failed",
			slog.String("label", label),
			slog.String("url", resp.Request.URL.String()),
			slog.String("method", method),
			slog.String("error", "response read failed"),
			slog.Int("request_bytes", len(bodyBytes)),
			slog.Int("status_code", resp.StatusCode),
			slog.Int(maxBodyLogFieldName, len(body)),
		)
		return nil, newToolError(label, "response read failed")
	}
	if tooLarge {
		logger.InfoContext(ctx, "harpoon response too large",
			slog.String("label", label),
			slog.String("url", resp.Request.URL.String()),
			slog.String("method", method),
			slog.Int("request_bytes", len(bodyBytes)),
			slog.Int("status_code", resp.StatusCode),
			slog.Int(maxBodyLogFieldName, len(body)),
		)
		return nil, newToolError(label, "response exceeds size limit")
	}

	logger.InfoContext(ctx, "harpoon request completed",
		slog.String("label", label),
		slog.String("url", resp.Request.URL.String()),
		slog.String("method", method),
		slog.Int("status_code", resp.StatusCode),
		slog.Int("request_bytes", len(bodyBytes)),
		slog.Int(maxBodyLogFieldName, len(body)),
	)

	return &callTargetResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Header,
		BodyBase64: base64.StdEncoding.EncodeToString(body),
		BodySize:   len(body),
		Truncated:  false,
	}, nil
}

func allowedMethodsList() []string {
	return []string{http.MethodGet, http.MethodPost, http.MethodPut}
}

func normalizeTimeout(timeoutMS *int) (time.Duration, error) {
	if timeoutMS == nil {
		return defaultTimeout, nil
	}
	if *timeoutMS <= 0 {
		return 0, errors.New("timeout must be positive")
	}
	timeout := time.Duration(*timeoutMS) * time.Millisecond
	if timeout < minTimeout {
		return 0, fmt.Errorf("timeout must be at least %dms", minTimeout.Milliseconds())
	}
	if timeout > maxTimeout {
		return 0, fmt.Errorf("timeout must be at most %dms", maxTimeout.Milliseconds())
	}
	return timeout, nil
}

func (s *Server) normalizeMaxResponseBytes(value *int) (int, error) {
	limit := s.cfg.MaxResponseBytes
	if limit <= 0 {
		limit = config.DefaultHarpoonMaxResponseBytes
	}
	if value == nil {
		return limit, nil
	}
	if *value <= 0 {
		return 0, errors.New("max_response_bytes must be positive")
	}
	if *value > limit {
		return 0, fmt.Errorf("max_response_bytes must be less than or equal to %d", limit)
	}
	return *value, nil
}

func (s *Server) normalizeRedirects(followRedirects *bool, maxRedirects *int) (int, bool, error) {
	follow := true
	if followRedirects != nil {
		follow = *followRedirects
	}
	if !follow {
		return 0, false, nil
	}
	limit := s.cfg.MaxRedirects
	if limit <= 0 {
		limit = config.DefaultHarpoonMaxRedirects
	}
	if maxRedirects == nil {
		return limit, true, nil
	}
	if *maxRedirects < 0 {
		return 0, false, errors.New("max_redirects must be non-negative")
	}
	if *maxRedirects > limit {
		return 0, false, fmt.Errorf("max_redirects must be less than or equal to %d", limit)
	}
	return *maxRedirects, true, nil
}

func (s *Server) redirectPolicy(label string, maxRedirects int, followRedirects bool) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if !followRedirects {
			return http.ErrUseLastResponse
		}
		if len(via) > maxRedirects {
			return newToolError(label, "redirect limit exceeded")
		}
		if req == nil || req.URL == nil {
			return newToolError(label, "redirect blocked")
		}
		if !s.registry.AllowsURL(req.URL) {
			return newToolError(label, "redirect blocked")
		}
		return nil
	}
}

func readLimited(reader io.Reader, limit int) ([]byte, bool, error) {
	if limit <= 0 {
		return nil, false, errors.New("limit must be positive")
	}
	limited := io.LimitReader(reader, int64(limit)+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return data, false, err
	}
	if len(data) > limit {
		return data[:limit], true, nil
	}
	return data, false, nil
}

func decodeArguments(args map[string]any, out any) error {
	if out == nil {
		return errors.New("output is nil")
	}
	if args == nil {
		return nil
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return err
	}
	return json.Unmarshal(payload, out)
}

func buildCallTargetSchema() *jsonschema.Schema {
	schema := jsonschema.Reflect(callTargetRequest{})
	if schema.Type == "" {
		schema.Type = "object"
	}
	schema.Required = []string{"label", "path", "method"}
	return schema
}

type toolError struct {
	label string
	msg   string
}

func newToolError(label, msg string) *toolError {
	return &toolError{label: label, msg: msg}
}

func (e *toolError) Error() string {
	label := e.label
	if label == "" {
		label = "unknown"
	}
	return fmt.Sprintf("label %s: %s", label, e.msg)
}

func asToolError(err error) *toolError {
	var te *toolError
	if errors.As(err, &te) {
		return te
	}
	return nil
}

func toolErrorResult(label, msg string) *mcp.CallToolResult {
	if label == "" {
		label = "unknown"
	}
	return &mcp.CallToolResult{
		IsError: true,
		Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("label %s: %s", label, msg)}},
	}
}

func classifyRequestError(err error) string {
	if err == nil {
		return "request failed"
	}
	var te *toolError
	if errors.As(err, &te) {
		return te.msg
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if strings.Contains(err.Error(), "redirect") {
		return "redirect blocked"
	}
	return "request failed"
}
