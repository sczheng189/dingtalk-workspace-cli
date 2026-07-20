// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	authpkg "github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/auth"
	dwsevent "github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/event"
	"github.com/DingTalk-Real-AI/dingtalk-workspace-cli/pkg/config"
	"github.com/gorilla/websocket"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/event"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/payload"
)

const (
	personalRawDebugPayloadLimit = 8192
	personalReconnectMinBackoff  = time.Second
	personalReconnectMaxBackoff  = 30 * time.Second
)

type PersonalConfig struct {
	// AccessToken 是静态 token（创建时定格）。保留用于测试与兼容；生产路径
	// 应优先使用 AccessTokenProvider，让每次取票都能拿到当前有效 token。
	AccessToken string
	// AccessTokenProvider 在每次 fetchTicket 前被调用，返回当前有效的 access
	// token。长驻 bus 进程内的 AT 过期后，重连取票会拿到刷新后的新 token，
	// 而不是进程启动时定格的旧 token。优先级高于 AccessToken。
	AccessTokenProvider func(context.Context) (string, error)
	// ForceRefreshToken 在 fetchTicket 收到 401 时被调用一次，入参是被服务端
	// 拒绝的那个 token；实现应在“盘上 AT 仍是它”的前提下强刷（ compare-and-
	// mark ），返回新 token 供立即重试一次。为 nil 时 401 维持不可重试语义。
	ForceRefreshToken func(ctx context.Context, rejectedToken string) (string, error)
	ClientID          string
	ClientSecret      string
	SourceID          string
	TicketURL         string
	TicketMode        string
	HTTPClient        *http.Client
	WebSocketDialer   *websocket.Dialer
	Now               func() time.Time
	ReconnectMin      time.Duration
	ReconnectMax      time.Duration
}

type PersonalSource struct {
	cfg     PersonalConfig
	machine *Machine
	started atomic.Bool
}

type retryablePersonalError struct {
	err error
}

func (e *retryablePersonalError) Error() string { return e.err.Error() }
func (e *retryablePersonalError) Unwrap() error { return e.err }

type ticketResponse struct {
	Endpoint string `json:"endpoint"`
	Ticket   string `json:"ticket"`
}

func NewPersonal(cfg PersonalConfig) (*PersonalSource, error) {
	if strings.TrimSpace(cfg.AccessToken) == "" && cfg.AccessTokenProvider == nil {
		return nil, errors.New("personal source: AccessToken or AccessTokenProvider is required")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("personal source: ClientID is required")
	}
	if strings.TrimSpace(cfg.SourceID) == "" {
		return nil, errors.New("personal source: SourceID is required")
	}
	if strings.TrimSpace(cfg.TicketURL) == "" {
		return nil, errors.New("personal source: TicketURL is required")
	}
	if cfg.TicketMode == "" {
		cfg.TicketMode = "normal"
	}
	if cfg.TicketMode == "custom" && strings.TrimSpace(cfg.ClientSecret) == "" {
		return nil, errors.New("personal source: ClientSecret is required when stream ticket mode is custom")
	}
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 30 * time.Second}
	}
	if cfg.WebSocketDialer == nil {
		cfg.WebSocketDialer = websocket.DefaultDialer
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ReconnectMin <= 0 {
		cfg.ReconnectMin = personalReconnectMinBackoff
	}
	if cfg.ReconnectMax <= 0 {
		cfg.ReconnectMax = personalReconnectMaxBackoff
	}
	if cfg.ReconnectMax < cfg.ReconnectMin {
		cfg.ReconnectMax = cfg.ReconnectMin
	}
	m := NewMachine()
	m.now = cfg.Now
	return &PersonalSource{cfg: cfg, machine: m}, nil
}

func (s *PersonalSource) State() Snapshot { return s.machine.Snapshot() }

func (s *PersonalSource) Start(ctx context.Context, emit dwsevent.EmitFn) error {
	if emit == nil {
		return errors.New("personal source: emit is required")
	}
	if !s.started.CompareAndSwap(false, true) {
		return errors.New("personal source: Start called twice")
	}
	s.machine.OnConnecting()
	defer s.machine.OnStopped()

	backoff := s.cfg.ReconnectMin
	for {
		acked, err := s.runAttempt(ctx, emit)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !isRetryablePersonalError(err) {
			return err
		}
		if acked {
			backoff = s.cfg.ReconnectMin
		}
		s.machine.OnReconnect()
		slog.Warn("personal source reconnecting",
			"error", personalRetryLogError(err),
			"retry_in", backoff,
			"reconnect_count", s.machine.Snapshot().ReconnectCount,
		)
		if err := waitPersonalReconnect(ctx, backoff); err != nil {
			return err
		}
		backoff = nextPersonalBackoff(backoff, s.cfg.ReconnectMax)
	}
}

func (s *PersonalSource) runAttempt(ctx context.Context, emit dwsevent.EmitFn) (bool, error) {
	ticket, err := s.fetchTicket(ctx)
	if err != nil {
		return false, err
	}
	wsURL, err := endpointWithTicket(ticket.Endpoint, ticket.Ticket)
	if err != nil {
		return false, err
	}
	conn, _, err := s.cfg.WebSocketDialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return false, retryPersonal(fmt.Errorf("personal source: dial websocket: %w", err))
	}
	attemptCtx, cancel := context.WithCancel(ctx)
	defer func() {
		cancel()
		_ = conn.Close()
	}()
	closePersonalWebSocketOnContext(attemptCtx, conn)
	s.machine.OnConnected()

	acked := false
	for {
		messageType, data, err := conn.ReadMessage()
		if err != nil {
			if ctx.Err() != nil {
				return acked, ctx.Err()
			}
			return acked, retryPersonal(fmt.Errorf("personal source: read websocket: %w", err))
		}
		if messageType != websocket.TextMessage {
			continue
		}
		if err := s.handleFrame(conn, data, emit); err != nil {
			return acked, err
		}
		acked = true
	}
}

// currentAccessToken 返回本次取票应使用的 token：优先 AccessTokenProvider
// （每次调用，长驻进程也能拿到刷新后的 token），否则回退到静态 AccessToken。
func (s *PersonalSource) currentAccessToken(ctx context.Context) (string, error) {
	if s.cfg.AccessTokenProvider != nil {
		tok, err := s.cfg.AccessTokenProvider(ctx)
		if err != nil {
			return "", fmt.Errorf("personal source: resolve access token: %w", err)
		}
		if strings.TrimSpace(tok) == "" {
			return "", errors.New("personal source: access token provider returned empty token")
		}
		return strings.TrimSpace(tok), nil
	}
	return s.cfg.AccessToken, nil
}

// fetchTicket 获取 WebSocket endpoint + ticket。遇到 401 且配置了
// ForceRefreshToken 时自救一次：强刷（入参为被拒绝的 token，实现方负责
// compare-and-mark 防并发覆盖），用新 token 立即重试一次；第二次无论结果
// 如何都直接返回，绝不无限循环。
func (s *PersonalSource) fetchTicket(ctx context.Context) (*ticketResponse, error) {
	token, err := s.currentAccessToken(ctx)
	if err != nil {
		return nil, err
	}
	ticket, status, err := s.fetchTicketOnce(ctx, token)
	if err == nil {
		return ticket, nil
	}
	if status != http.StatusUnauthorized || s.cfg.ForceRefreshToken == nil {
		return nil, err
	}
	fresh, ferr := s.cfg.ForceRefreshToken(ctx, token)
	if ferr != nil || strings.TrimSpace(fresh) == "" {
		// 强刷失败原因必须与原始 401 一起保留：否则真正的 RT 交换失败（网络波动、
		// RT 被吊销）会被折叠成裸 401，排查时完全看不到根因。
		if ferr != nil {
			return nil, errors.Join(err, fmt.Errorf("force refresh after 401: %w", ferr))
		}
		return nil, errors.Join(err, errors.New("force refresh after 401: refreshed token is empty"))
	}
	ticket, _, err = s.fetchTicketOnce(ctx, strings.TrimSpace(fresh))
	return ticket, err
}

// fetchTicketOnce 执行单次取票请求，返回响应 HTTP 状态码（网络/解析错误时为 0）。
func (s *PersonalSource) fetchTicketOnce(ctx context.Context, accessToken string) (*ticketResponse, int, error) {
	body := map[string]any{
		"sourceId": s.cfg.SourceID,
		"mode":     s.cfg.TicketMode,
	}
	if s.cfg.TicketMode == "custom" {
		body["clientId"] = s.cfg.ClientID
		body["clientSecret"] = s.cfg.ClientSecret
	}
	b, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.TicketURL, bytes.NewReader(b))
	if err != nil {
		return nil, 0, fmt.Errorf("personal source: create ticket request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("x-user-access-token", accessToken)
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("X-DWS-Client-Id", s.cfg.ClientID)
	req.Header.Set("X-DWS-Source-Id", s.cfg.SourceID)

	resp, err := s.cfg.HTTPClient.Do(req)
	if err != nil {
		return nil, 0, retryPersonal(fmt.Errorf("personal source: fetch ticket: %w", err))
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, config.MaxResponseBodySize))
	if err != nil {
		return nil, resp.StatusCode, retryPersonal(fmt.Errorf("personal source: read ticket response: %w", err))
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("personal source: ticket HTTP %d", resp.StatusCode)
		if retryableTicketStatus(resp.StatusCode) {
			return nil, resp.StatusCode, retryPersonal(err)
		}
		return nil, resp.StatusCode, err
	}
	ticket, err := decodeTicket(data)
	if err != nil {
		return nil, resp.StatusCode, err
	}
	if ticket.Endpoint == "" || ticket.Ticket == "" {
		return nil, resp.StatusCode, errors.New("personal source: ticket response missing endpoint or ticket")
	}
	return ticket, resp.StatusCode, nil
}

func (s *PersonalSource) handleFrame(conn *websocket.Conn, data []byte, emit dwsevent.EmitFn) error {
	df, err := payload.DecodeDataFrame(data)
	if err != nil {
		return fmt.Errorf("personal source: decode dataframe: %w", err)
	}
	raw := s.rawEventFromDataFrame(df)
	logPersonalDataFrame(raw, df.Data)
	emit(raw)
	resp := payload.NewSuccessDataFrameResponse()
	resp.SetHeader(payload.DataFrameHeaderKMessageId, df.GetMessageId())
	resp.SetHeader(payload.DataFrameHeaderKContentType, payload.DataFrameContentTypeKJson)
	_ = resp.SetJson(event.NewEventProcessResultSuccess())
	if err := conn.WriteJSON(resp); err != nil {
		return retryPersonal(fmt.Errorf("personal source: write ack: %w", err))
	}
	s.machine.OnEvent()
	return nil
}

func (s *PersonalSource) rawEventFromDataFrame(df *payload.DataFrame) *dwsevent.RawEvent {
	hdr := event.NewEventHeaderFromDataFrame(df)
	dataFields := parsePersonalDataFields(df.Data)
	now := time.Now
	if s != nil && s.cfg.Now != nil {
		now = s.cfg.Now
	}
	sourceID := ""
	if s != nil {
		sourceID = s.cfg.SourceID
	}
	raw := &dwsevent.RawEvent{
		EventID:           firstNonEmpty(hdr.EventId, df.GetMessageId(), headerAny(df.Headers, "MESSAGE_ID", "messageId", "event_id", "eventId"), dataFields.string("eventId", "event_id")),
		EventBornTime:     firstInt64(hdr.EventBornTime, df.GetTimestamp(), df.Time),
		EventCorpID:       hdr.EventCorpId,
		EventType:         firstNonEmpty(hdr.EventType, eventTypeFromHeaders(df.Headers), dataFields.string("eventKey", "event_key"), dataFields.nestedString([]string{"source", "tag"})),
		EventUnifiedAppID: hdr.EventUnifiedAppId,
		EventScope:        firstNonEmpty(headerAny(df.Headers, "event_scope", "eventScope"), "personal"),
		SubscribeID:       firstNonEmpty(headerAny(df.Headers, "SUB_ID", "subscribe_id", "subscribeId", "sub_id", "subId"), dataFields.string("subId", "sub_id", "subscribeId", "subscribe_id")),
		SourceID:          firstNonEmpty(headerAny(df.Headers, "SOURCE_ID", "source_id", "sourceId"), sourceID),
		RuleType:          firstNonEmpty(headerAny(df.Headers, "RULE_TYPE", "rule_type", "ruleType"), dataFields.string("ruleType", "rule_type"), dataFields.nestedString([]string{"ext", "ruleType"}, []string{"ext", "rule_type"})),
		Data:              df.Data,
		Headers:           copyHeaders(df.Headers),
		ReceivedAt:        now().UTC(),
	}
	return raw
}

func logPersonalDataFrame(raw *dwsevent.RawEvent, data string) {
	if raw == nil {
		return
	}
	slog.Debug("personal source received dataframe",
		"event_type", raw.EventType,
		"event_key", raw.EventType,
		"event_id", raw.EventID,
		"subscribe_id", raw.SubscribeID,
		"source_id", raw.SourceID,
		"rule_type", raw.RuleType,
		"headers", redactPersonalRawStringMap(raw.Headers),
		"data", sanitizePersonalRawPayload([]byte(data)),
	)
}

func decodeTicket(data []byte) (*ticketResponse, error) {
	var env struct {
		Success bool            `json:"success"`
		Result  json.RawMessage `json:"result"`
		Data    json.RawMessage `json:"data"`
		Error   any             `json:"error"`
	}
	if err := json.Unmarshal(data, &env); err == nil && (env.Result != nil || env.Data != nil || env.Error != nil) {
		raw := env.Result
		if len(raw) == 0 || string(raw) == "null" {
			raw = env.Data
		}
		var tr ticketResponse
		if err := json.Unmarshal(raw, &tr); err != nil {
			return nil, fmt.Errorf("personal source: parse ticket result: %w", err)
		}
		return &tr, nil
	}
	var tr ticketResponse
	if err := json.Unmarshal(data, &tr); err != nil {
		return nil, fmt.Errorf("personal source: parse ticket response: %w", err)
	}
	return &tr, nil
}

func endpointWithTicket(endpoint, ticket string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(endpoint))
	if err != nil {
		return "", fmt.Errorf("personal source: parse endpoint: %w", err)
	}
	if (u.Scheme != "ws" && u.Scheme != "wss") || u.Host == "" {
		return "", errors.New("personal source: ticket response contains invalid websocket endpoint")
	}
	q := u.Query()
	if q.Get("ticket") == "" {
		q.Set("ticket", ticket)
		u.RawQuery = q.Encode()
	}
	return u.String(), nil
}

func retryPersonal(err error) error {
	if err == nil {
		return nil
	}
	return &retryablePersonalError{err: err}
}

// isRetryablePersonalError 判定错误是否值得退避重连。除连接/取票类可重试错误外，
// RT 交换失败（ErrRefreshFailed，通常是网络波动或服务端暂时故障）也按可重试处理：
// 长驻订阅进程遇到暂时性刷新失败应退避重试、等待自愈，而不是直接退出 bus。
// ErrNoCredentials（真的需要重新登录）保持致命，立即退出并暴露给用户。
func isRetryablePersonalError(err error) bool {
	if errors.Is(err, authpkg.ErrRefreshFailed) {
		return true
	}
	var retryable *retryablePersonalError
	return errors.As(err, &retryable)
}

// personalRetryLogError 生成写进 bus.log 的重连原因。RT 刷新失败必须保留真实原因
// （网络错误 / HTTP 状态 / 持久化失败），否则“AT 到期 → 自动刷新失败”在日志里
// 只剩一句 retryable stream error，无法排障。输出做长度截断防止刷屏；该错误链
// 由凭证解析层构造，不含 token 本体。
func personalRetryLogError(err error) string {
	if errors.Is(err, authpkg.ErrRefreshFailed) {
		return "personal source: token refresh failed: " + truncatePersonalLogMessage(err.Error())
	}
	message := err.Error()
	switch {
	case strings.Contains(message, "ticket HTTP"):
		return message
	case strings.Contains(message, "fetch ticket"):
		return "personal source: fetch ticket: network error"
	case strings.Contains(message, "read ticket response"):
		return "personal source: read ticket response: network error"
	case strings.Contains(message, "dial websocket"):
		return "personal source: dial websocket: network error"
	case strings.Contains(message, "read websocket"):
		return "personal source: read websocket: connection closed"
	case strings.Contains(message, "write ack"):
		return "personal source: write ack: connection error"
	default:
		return "personal source: retryable stream error"
	}
}

// truncatePersonalLogMessage 截断写入 bus.log 的错误文本，防止异常长输出刷屏。
func truncatePersonalLogMessage(message string) string {
	const maxLen = 300
	if len(message) <= maxLen {
		return message
	}
	return message[:maxLen] + "...(truncated)"
}

func retryableTicketStatus(status int) bool {
	return status == http.StatusRequestTimeout ||
		status == http.StatusTooManyRequests ||
		status >= http.StatusInternalServerError
}

func waitPersonalReconnect(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func nextPersonalBackoff(current, maximum time.Duration) time.Duration {
	if current >= maximum || current > maximum/2 {
		return maximum
	}
	return current * 2
}

func closePersonalWebSocketOnContext(ctx context.Context, conn *websocket.Conn) {
	go func() {
		<-ctx.Done()
		_ = conn.Close()
	}()
}

func headerAny(h payload.DataFrameHeader, names ...string) string {
	for _, name := range names {
		if v := strings.TrimSpace(headerValue(h, name)); v != "" {
			return v
		}
	}
	return ""
}

func headerValue(h payload.DataFrameHeader, name string) string {
	if len(h) == 0 {
		return ""
	}
	if v := h.Get(name); strings.TrimSpace(v) != "" {
		return v
	}
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}

func eventTypeFromHeaders(h payload.DataFrameHeader) string {
	if v := headerAny(h, "EVENT_TYPE", "event_type", "eventType", "EVENT_KEY", "event_key", "eventKey"); v != "" {
		return v
	}
	if v := headerAny(h, "TOPIC", "topic"); v != "" && v != "*" {
		return v
	}
	return ""
}

type personalDataFields map[string]any

func parsePersonalDataFields(raw string) personalDataFields {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var value any
	if err := json.Unmarshal([]byte(raw), &value); err != nil {
		return nil
	}
	if s, ok := value.(string); ok {
		var nested any
		if err := json.Unmarshal([]byte(strings.TrimSpace(s)), &nested); err == nil {
			value = nested
		}
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	return personalDataFields(m)
}

func (f personalDataFields) string(names ...string) string {
	if len(f) == 0 {
		return ""
	}
	for _, name := range names {
		if v, ok := f[name].(string); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
		for k, raw := range f {
			if strings.EqualFold(k, name) {
				if v, ok := raw.(string); ok && strings.TrimSpace(v) != "" {
					return strings.TrimSpace(v)
				}
			}
		}
	}
	return ""
}

func (f personalDataFields) nestedString(paths ...[]string) string {
	if len(f) == 0 {
		return ""
	}
	for _, path := range paths {
		if v := nestedStringCI(map[string]any(f), path...); v != "" {
			return v
		}
	}
	return ""
}

func nestedStringCI(m map[string]any, path ...string) string {
	var cur any = m
	for _, segment := range path {
		obj, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		next, ok := obj[segment]
		if !ok {
			for k, v := range obj {
				if strings.EqualFold(k, segment) {
					next = v
					ok = true
					break
				}
			}
		}
		if !ok {
			return ""
		}
		cur = next
	}
	if v, ok := cur.(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func firstInt64(values ...int64) int64 {
	for _, v := range values {
		if v != 0 {
			return v
		}
	}
	return 0
}

func redactPersonalRawStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		if personalRawSensitiveKey(k) {
			out[k] = "<redacted>"
			continue
		}
		out[k] = v
	}
	return out
}

func sanitizePersonalRawPayload(data []byte) string {
	data = bytes.TrimSpace(data)
	if len(data) == 0 {
		return ""
	}
	var parsed any
	if err := json.Unmarshal(data, &parsed); err == nil {
		redacted := redactPersonalRawJSONValue(parsed)
		if s, err := marshalPersonalRawJSON(redacted); err == nil {
			return truncatePersonalRawLog(s)
		}
	}
	return truncatePersonalRawLog(string(data))
}

func redactPersonalRawJSONValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, value := range x {
			if personalRawSensitiveKey(k) {
				out[k] = "<redacted>"
				continue
			}
			out[k] = redactPersonalRawJSONValue(value)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, value := range x {
			out[i] = redactPersonalRawJSONValue(value)
		}
		return out
	default:
		return v
	}
}

func marshalPersonalRawJSON(v any) (string, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", err
	}
	return strings.TrimSpace(buf.String()), nil
}

func personalRawSensitiveKey(key string) bool {
	key = strings.ToLower(strings.TrimSpace(key))
	return strings.Contains(key, "token") ||
		strings.Contains(key, "secret") ||
		strings.Contains(key, "ticket") ||
		strings.Contains(key, "authorization")
}

func truncatePersonalRawLog(s string) string {
	if len(s) <= personalRawDebugPayloadLimit {
		return s
	}
	return s[:personalRawDebugPayloadLimit] + "...<truncated>"
}
