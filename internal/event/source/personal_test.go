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
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authpkg "github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/auth"
	dwsevent "github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/event"
	"github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/event/bus"
	"github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/event/transport"
	"github.com/gorilla/websocket"
	streamevent "github.com/open-dingtalk/dingtalk-stream-sdk-go/event"
	"github.com/open-dingtalk/dingtalk-stream-sdk-go/payload"
)

func TestPersonalSourceFetchTicketConnectsAndACKs(t *testing.T) {
	logs := capturePersonalSourceDebugLogs(t)
	var wsEndpoint string
	ackCh := make(chan payload.DataFrameResponse, 1)
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("x-user-access-token"); got != "token-1" {
			t.Fatalf("x-user-access-token = %q", got)
		}
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode ticket request: %v", err)
		}
		if req["sourceId"] != "open" || req["mode"] != "normal" {
			t.Fatalf("ticket request = %#v", req)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"endpoint": wsEndpoint,
				"ticket":   "ticket-1",
			},
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("ticket"); got != "ticket-1" {
			t.Fatalf("ticket query = %q", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("upgrade: %v", err)
		}
		defer conn.Close()
		df := payload.DataFrame{
			Type: "event",
			Headers: payload.DataFrameHeader{
				payload.DataFrameHeaderKMessageId:         "msg-1",
				streamevent.DataFrameHeaderKEventId:       "evt-1",
				streamevent.DataFrameHeaderKEventBornTime: "1234",
				streamevent.DataFrameHeaderKEventCorpId:   "corp-1",
				streamevent.DataFrameHeaderKEventType:     "user_im_message_receive_at",
				"subscribeId":                             "sub-1",
				"ruleType":                                "at",
				"sourceId":                                "open",
				"accessToken":                             "header-secret-token",
			},
			Data: `{"message":{"text":"hi"},"access_token":"data-secret-token","client_secret":"data-secret","ticket":"data-ticket","Authorization":"Bearer data-auth"}`,
		}
		if err := conn.WriteJSON(df); err != nil {
			t.Fatalf("write dataframe: %v", err)
		}
		var ack payload.DataFrameResponse
		if err := conn.ReadJSON(&ack); err != nil {
			t.Fatalf("read ack: %v", err)
		}
		ackCh <- ack
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsEndpoint = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	src, err := NewPersonal(PersonalConfig{
		AccessToken: "token-1",
		ClientID:    "client-1",
		SourceID:    "open",
		TicketURL:   srv.URL + "/ticket",
		TicketMode:  "normal",
		HTTPClient:  srv.Client(),
		Now:         func() time.Time { return time.Unix(10, 0) },
	})
	if err != nil {
		t.Fatalf("NewPersonal() error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	evCh := make(chan *dwsevent.RawEvent, 1)
	done := make(chan error, 1)
	go func() {
		done <- src.Start(ctx, func(ev *dwsevent.RawEvent) { evCh <- ev })
	}()

	select {
	case ev := <-evCh:
		if ev.SubscribeID != "sub-1" || ev.RuleType != "at" || ev.SourceID != "open" {
			t.Fatalf("event personal fields = %#v", ev)
		}
		if ev.EventType != "user_im_message_receive_at" || ev.EventID != "evt-1" {
			t.Fatalf("event = %#v", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event")
	}
	select {
	case ack := <-ackCh:
		if ack.Code != payload.DataFrameResponseStatusCodeKOK {
			t.Fatalf("ack code = %d", ack.Code)
		}
		if ack.GetHeader(payload.DataFrameHeaderKMessageId) != "msg-1" {
			t.Fatalf("ack message id = %q", ack.GetHeader(payload.DataFrameHeaderKMessageId))
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for ack")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("source did not stop after cancel")
	}

	// Start can log a reconnect after emitting the event and before observing
	// cancellation. Read the shared log buffer only after the goroutine exits so
	// the assertion remains race-free under `go test -race`.
	out := logs.String()
	for _, want := range []string{"personal source received dataframe", "user_im_message_receive_at", "evt-1", "sub-1", "sourceId", "message", "<redacted>"} {
		if !strings.Contains(out, want) {
			t.Fatalf("debug log missing %q: %s", want, out)
		}
	}
	for _, leaked := range []string{"header-secret-token", "data-secret-token", "data-secret", "data-ticket", "Bearer data-auth"} {
		if strings.Contains(out, leaked) {
			t.Fatalf("debug log leaked %q: %s", leaked, out)
		}
	}
}

func TestPersonalSourceReconnectsWithFreshTicket(t *testing.T) {
	var wsEndpoint string
	var ticketCalls atomic.Int32
	ackCh := make(chan int, 2)
	holdSecond := make(chan struct{})
	var releaseSecondOnce sync.Once
	releaseSecond := func() { releaseSecondOnce.Do(func() { close(holdSecond) }) }
	upgrader := websocket.Upgrader{}
	var ticketTokensMu sync.Mutex
	var ticketTokens []string
	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", func(w http.ResponseWriter, r *http.Request) {
		attempt := int(ticketCalls.Add(1))
		ticketTokensMu.Lock()
		ticketTokens = append(ticketTokens, r.Header.Get("x-user-access-token"))
		ticketTokensMu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result": map[string]any{
				"endpoint": wsEndpoint,
				"ticket":   fmt.Sprintf("ticket-%d", attempt),
			},
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		attempt, err := strconv.Atoi(strings.TrimPrefix(r.URL.Query().Get("ticket"), "ticket-"))
		if err != nil {
			http.Error(w, "bad ticket", http.StatusBadRequest)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(personalTestDataFrame(attempt)); err != nil {
			return
		}
		var ack payload.DataFrameResponse
		if err := conn.ReadJSON(&ack); err != nil {
			return
		}
		ackCh <- attempt
		if attempt == 2 {
			<-holdSecond
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer releaseSecond()
	wsEndpoint = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	src, err := NewPersonal(PersonalConfig{
		// 动态 provider：每次取票前被调用，重连时返回与首次不同的 token，
		// 验证“重连拿新 AT”而不只是“拿新 ticket”。
		AccessTokenProvider: func(context.Context) (string, error) {
			if ticketCalls.Load() == 0 {
				return "token-v1", nil
			}
			return "token-v2", nil
		},
		ClientID:     "client",
		SourceID:     "open",
		TicketURL:    srv.URL + "/ticket",
		HTTPClient:   srv.Client(),
		ReconnectMin: 5 * time.Millisecond,
		ReconnectMax: 10 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan *dwsevent.RawEvent, 2)
	done := make(chan error, 1)
	go func() { done <- src.Start(ctx, func(ev *dwsevent.RawEvent) { events <- ev }) }()

	for i := 1; i <= 2; i++ {
		select {
		case ev := <-events:
			if ev.EventID != fmt.Sprintf("evt-%d", i) {
				t.Fatalf("event %d ID = %q", i, ev.EventID)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for event %d", i)
		}
		select {
		case attempt := <-ackCh:
			if attempt != i {
				t.Fatalf("ack attempt = %d, want %d", attempt, i)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for ACK %d", i)
		}
	}
	if got := ticketCalls.Load(); got != 2 {
		t.Fatalf("ticket calls = %d, want 2", got)
	}
	ticketTokensMu.Lock()
	gotTokens := append([]string(nil), ticketTokens...)
	ticketTokensMu.Unlock()
	if len(gotTokens) != 2 || gotTokens[0] != "token-v1" || gotTokens[1] != "token-v2" {
		t.Fatalf("ticket request tokens = %v, want [token-v1 token-v2]", gotTokens)
	}
	if got := src.State().ReconnectCount; got != 1 {
		t.Fatalf("reconnect count = %d, want 1", got)
	}
	cancel()
	releaseSecond()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() error = %v, want context canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("source did not stop after cancel")
	}
}

// 401 自救：第一次取票被 401 拒绝 → ForceRefreshToken 被调一次（入参为被拒绝的
// token）→ 用新 token 重试成功。这是“服务端提前废掉 AT”场景的完整闭环。
func TestPersonalSourceTicket401ForceRefreshRetry(t *testing.T) {
	var wsEndpoint string
	var ticketCalls atomic.Int32
	var forceCalls atomic.Int32
	var rejectedSeen atomic.Value
	var ticketTokensMu sync.Mutex
	var ticketTokens []string
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", func(w http.ResponseWriter, r *http.Request) {
		attempt := int(ticketCalls.Add(1))
		ticketTokensMu.Lock()
		ticketTokens = append(ticketTokens, r.Header.Get("x-user-access-token"))
		ticketTokensMu.Unlock()
		if attempt == 1 {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  map[string]any{"endpoint": wsEndpoint, "ticket": fmt.Sprintf("ticket-%d", attempt)},
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(personalTestDataFrame(1)); err != nil {
			return
		}
		var ack payload.DataFrameResponse
		_ = conn.ReadJSON(&ack)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsEndpoint = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	src, err := NewPersonal(PersonalConfig{
		AccessTokenProvider: func(context.Context) (string, error) { return "old-at", nil },
		ForceRefreshToken: func(_ context.Context, rejectedToken string) (string, error) {
			forceCalls.Add(1)
			rejectedSeen.Store(rejectedToken)
			return "new-at", nil
		},
		ClientID:   "client",
		SourceID:   "open",
		TicketURL:  srv.URL + "/ticket",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	events := make(chan *dwsevent.RawEvent, 1)
	done := make(chan error, 1)
	go func() { done <- src.Start(ctx, func(ev *dwsevent.RawEvent) { events <- ev }) }()

	select {
	case <-events:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for event after 401 force-refresh retry")
	}
	if got := forceCalls.Load(); got != 1 {
		t.Fatalf("force refresh calls = %d, want 1", got)
	}
	if got := rejectedSeen.Load(); got != "old-at" {
		t.Fatalf("force refresh rejected token = %v, want old-at", got)
	}
	ticketTokensMu.Lock()
	gotTokens := append([]string(nil), ticketTokens...)
	ticketTokensMu.Unlock()
	if len(gotTokens) != 2 || gotTokens[0] != "old-at" || gotTokens[1] != "new-at" {
		t.Fatalf("ticket request tokens = %v, want [old-at new-at]", gotTokens)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("source did not stop after cancel")
	}
}

// 二次 401 必须终止：强刷后重试仍被拒 → 返回错误，ForceRefreshToken 只被调一次，
// 绝不无限循环。
func TestPersonalSourceTicket401SecondFailureStops(t *testing.T) {
	var ticketCalls atomic.Int32
	var forceCalls atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", func(w http.ResponseWriter, _ *http.Request) {
		ticketCalls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src, err := NewPersonal(PersonalConfig{
		AccessTokenProvider: func(context.Context) (string, error) { return "old-at", nil },
		ForceRefreshToken: func(context.Context, string) (string, error) {
			forceCalls.Add(1)
			return "new-at", nil
		},
		ClientID:   "client",
		SourceID:   "open",
		TicketURL:  srv.URL + "/ticket",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runErr := src.Start(context.Background(), func(*dwsevent.RawEvent) {})
	if runErr == nil || !strings.Contains(runErr.Error(), "ticket HTTP 401") {
		t.Fatalf("Start() error = %v, want ticket HTTP 401", runErr)
	}
	if got := forceCalls.Load(); got != 1 {
		t.Fatalf("force refresh calls = %d, want 1", got)
	}
	if got := ticketCalls.Load(); got != 2 {
		t.Fatalf("ticket calls = %d, want 2", got)
	}
}

// 401 后强刷失败：错误必须同时包含原始 401 和强刷失败原因，不得只报裸 401。
func TestPersonalSourceTicket401ForceRefreshErrorVisible(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src, err := NewPersonal(PersonalConfig{
		AccessTokenProvider: func(context.Context) (string, error) { return "old-at", nil },
		ForceRefreshToken: func(context.Context, string) (string, error) {
			return "", errors.New("rt exchange: connection reset")
		},
		ClientID:   "client",
		SourceID:   "open",
		TicketURL:  srv.URL + "/ticket",
		HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runErr := src.Start(context.Background(), func(*dwsevent.RawEvent) {})
	if runErr == nil {
		t.Fatal("Start() succeeded, want joined 401 + force refresh error")
	}
	for _, want := range []string{"ticket HTTP 401", "force refresh after 401", "connection reset"} {
		if !strings.Contains(runErr.Error(), want) {
			t.Fatalf("Start() error missing %q: %v", want, runErr)
		}
	}
}

// 401 后强刷返回空 token：错误同样要说明是强刷结果为空，而不是只报 401。
func TestPersonalSourceTicket401ForceRefreshEmptyTokenVisible(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	src, err := NewPersonal(PersonalConfig{
		AccessTokenProvider: func(context.Context) (string, error) { return "old-at", nil },
		ForceRefreshToken:   func(context.Context, string) (string, error) { return "", nil },
		ClientID:            "client",
		SourceID:            "open",
		TicketURL:           srv.URL + "/ticket",
		HTTPClient:          srv.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runErr := src.Start(context.Background(), func(*dwsevent.RawEvent) {})
	if runErr == nil || !strings.Contains(runErr.Error(), "refreshed token is empty") ||
		!strings.Contains(runErr.Error(), "ticket HTTP 401") {
		t.Fatalf("Start() error = %v, want 401 + empty refreshed token", runErr)
	}
}

// 暂时性 RT 刷新失败（ErrRefreshFailed）应进入退避重连而不是退出 bus：
// 第一次取 token 失败，第二次成功并建立 WebSocket 连接。
func TestPersonalSourceRetriesTransientRefreshFailure(t *testing.T) {
	if !isRetryablePersonalError(fmt.Errorf("wrap: %w", authpkg.ErrRefreshFailed)) {
		t.Fatal("ErrRefreshFailed must be retryable")
	}
	if isRetryablePersonalError(fmt.Errorf("wrap: %w", authpkg.ErrNoCredentials)) {
		t.Fatal("ErrNoCredentials must stay fatal")
	}

	var wsEndpoint string
	connected := make(chan struct{})
	var connectOnce sync.Once
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"success": true,
			"result":  map[string]any{"endpoint": wsEndpoint, "ticket": "t-1"},
		})
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		connectOnce.Do(func() { close(connected) })
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	wsEndpoint = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	var providerCalls atomic.Int32
	src, err := NewPersonal(PersonalConfig{
		AccessTokenProvider: func(context.Context) (string, error) {
			if providerCalls.Add(1) == 1 {
				return "", fmt.Errorf("rt exchange down: %w", authpkg.ErrRefreshFailed)
			}
			return "good-at", nil
		},
		ClientID:     "client",
		SourceID:     "open",
		TicketURL:    srv.URL + "/ticket",
		HTTPClient:   srv.Client(),
		ReconnectMin: 10 * time.Millisecond,
		ReconnectMax: 20 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- src.Start(ctx, func(*dwsevent.RawEvent) {}) }()

	select {
	case <-connected:
	case err := <-done:
		t.Fatalf("source exited instead of retrying transient refresh failure: %v", err)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for reconnect after transient refresh failure")
	}
	if got := providerCalls.Load(); got < 2 {
		t.Fatalf("provider calls = %d, want >= 2 (retry after transient failure)", got)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("source did not stop after cancel")
	}
}

// bus.log 里的重连原因必须保留 RT 刷新的真实失败原因（网络错误 / HTTP 状态），
// 不能被压缩成 “retryable stream error”；同时要做长度截断防刷屏。
func TestPersonalRetryLogErrorPreservesRefreshFailureReason(t *testing.T) {
	refreshErr := authpkg.NewCredentialError("refresh_token 刷新失败: HTTP 500: upstream down", authpkg.ErrRefreshFailed, errors.New("upstream down"))
	wrapped := fmt.Errorf("personal source: resolve access token: %w", refreshErr)
	out := personalRetryLogError(wrapped)
	if !strings.Contains(out, "token refresh failed") || !strings.Contains(out, "HTTP 500") {
		t.Fatalf("log message lost refresh reason: %q", out)
	}

	long := fmt.Errorf("%w: %s", authpkg.ErrRefreshFailed, strings.Repeat("x", 500))
	if got := personalRetryLogError(long); len(got) > 360 || !strings.HasSuffix(got, "...(truncated)") {
		t.Fatalf("log message not truncated: len=%d", len(got))
	}

	// 非刷新类错误的既有压缩映射不得改变。
	if got := personalRetryLogError(errors.New("personal source: dial websocket: x")); got != "personal source: dial websocket: network error" {
		t.Fatalf("non-refresh error mapping changed: %q", got)
	}
	if got := personalRetryLogError(errors.New("something else")); got != "personal source: retryable stream error" {
		t.Fatalf("default mapping changed: %q", got)
	}
}

func TestPersonalSourceRetriesTicket500And429(t *testing.T) {
	var wsEndpoint string
	var ticketCalls atomic.Int32
	acked := make(chan struct{}, 1)
	hold := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(hold) }) }
	upgrader := websocket.Upgrader{}
	mux := http.NewServeMux()
	mux.HandleFunc("/ticket", func(w http.ResponseWriter, _ *http.Request) {
		switch ticketCalls.Add(1) {
		case 1:
			w.WriteHeader(http.StatusInternalServerError)
			return
		case 2:
			w.WriteHeader(http.StatusTooManyRequests)
			return
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"success": true,
				"result":  map[string]any{"endpoint": wsEndpoint, "ticket": "ticket-ok"},
			})
		}
	})
	mux.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(personalTestDataFrame(1)); err != nil {
			return
		}
		var ack payload.DataFrameResponse
		if err := conn.ReadJSON(&ack); err != nil {
			return
		}
		acked <- struct{}{}
		<-hold
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	defer release()
	wsEndpoint = "ws" + strings.TrimPrefix(srv.URL, "http") + "/ws"

	src, err := NewPersonal(PersonalConfig{
		AccessToken:  "token",
		ClientID:     "client",
		SourceID:     "open",
		TicketURL:    srv.URL + "/ticket",
		HTTPClient:   srv.Client(),
		ReconnectMin: time.Millisecond,
		ReconnectMax: 2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- src.Start(ctx, func(*dwsevent.RawEvent) {}) }()
	select {
	case <-acked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for recovered connection")
	}
	if got := ticketCalls.Load(); got != 3 {
		t.Fatalf("ticket calls = %d, want 3", got)
	}
	if got := src.State().ReconnectCount; got != 2 {
		t.Fatalf("reconnect count = %d, want 2", got)
	}
	cancel()
	release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("source did not stop after cancel")
	}
}

func TestPersonalSourceTicketFatalResponsesDoNotRetry(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
	}{
		{name: "unauthorized", status: http.StatusUnauthorized},
		{name: "forbidden", status: http.StatusForbidden},
		{name: "malformed success", status: http.StatusOK, body: `{"success":true,"result":{"endpoint":"","ticket":""}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				calls.Add(1)
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			src, err := NewPersonal(PersonalConfig{
				AccessToken:  "token",
				ClientID:     "client",
				SourceID:     "open",
				TicketURL:    srv.URL,
				HTTPClient:   srv.Client(),
				ReconnectMin: time.Millisecond,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := src.Start(t.Context(), func(*dwsevent.RawEvent) {}); err == nil {
				t.Fatal("Start() error = nil, want fatal ticket error")
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("ticket calls = %d, want 1", got)
			}
		})
	}
}

func TestPersonalSourceRetriesTicketNetworkError(t *testing.T) {
	var calls atomic.Int32
	secondCall := make(chan struct{}, 1)
	httpClient := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		if calls.Add(1) == 2 {
			secondCall <- struct{}{}
		}
		return nil, errors.New("network unavailable")
	})}
	src, err := NewPersonal(PersonalConfig{
		AccessToken:  "token",
		ClientID:     "client",
		SourceID:     "open",
		TicketURL:    "https://ticket.invalid",
		HTTPClient:   httpClient,
		ReconnectMin: time.Millisecond,
		ReconnectMax: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- src.Start(ctx, func(*dwsevent.RawEvent) {}) }()
	select {
	case <-secondCall:
	case <-time.After(time.Second):
		t.Fatal("ticket network error was not retried")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() error = %v, want context canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("source did not stop after cancel")
	}
}

func TestPersonalSourceCancelDuringReconnectBackoff(t *testing.T) {
	requestDone := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestDone <- struct{}{}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	src, err := NewPersonal(PersonalConfig{
		AccessToken:  "token",
		ClientID:     "client",
		SourceID:     "open",
		TicketURL:    srv.URL,
		HTTPClient:   srv.Client(),
		ReconnectMin: 5 * time.Second,
		ReconnectMax: 5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- src.Start(ctx, func(*dwsevent.RawEvent) {}) }()
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("ticket request did not arrive")
	}
	deadline := time.Now().Add(time.Second)
	for src.State().ReconnectCount == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if src.State().ReconnectCount == 0 {
		t.Fatal("source did not enter reconnect backoff")
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Start() error = %v, want context canceled", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("source did not exit promptly during reconnect backoff")
	}
}

func TestNextPersonalBackoffCapsAtMaximum(t *testing.T) {
	if got := nextPersonalBackoff(time.Second, 30*time.Second); got != 2*time.Second {
		t.Fatalf("next backoff = %s, want 2s", got)
	}
	if got := nextPersonalBackoff(20*time.Second, 30*time.Second); got != 30*time.Second {
		t.Fatalf("capped backoff = %s, want 30s", got)
	}
}

func TestPersonalRetryLogErrorDoesNotExposeWebSocketURL(t *testing.T) {
	err := retryPersonal(errors.New("personal source: dial websocket: wss://example.test/ws?ticket=secret-ticket"))
	got := personalRetryLogError(err)
	if strings.Contains(got, "secret-ticket") || strings.Contains(got, "example.test") {
		t.Fatalf("retry log exposed websocket details: %q", got)
	}
}

func TestPersonalSourceParsesUppercaseHeaders(t *testing.T) {
	src := personalSourceForRawEventTests()
	data := `{"payload":true}`
	raw := src.rawEventFromDataFrame(&payload.DataFrame{
		Time: 12345,
		Headers: payload.DataFrameHeader{
			"EVENT_TYPE": "user_im_message_receive_o2o",
			"SUB_ID":     "sub-1",
			"SOURCE_ID":  "pre_open_source",
			"MESSAGE_ID": "evt-1",
		},
		Data: data,
	})

	if raw.EventType != "user_im_message_receive_o2o" {
		t.Fatalf("EventType = %q", raw.EventType)
	}
	if raw.SubscribeID != "sub-1" {
		t.Fatalf("SubscribeID = %q", raw.SubscribeID)
	}
	if raw.SourceID != "pre_open_source" {
		t.Fatalf("SourceID = %q", raw.SourceID)
	}
	if raw.EventID != "evt-1" {
		t.Fatalf("EventID = %q", raw.EventID)
	}
	if raw.EventScope != "personal" {
		t.Fatalf("EventScope = %q", raw.EventScope)
	}
	if raw.Data != data {
		t.Fatalf("Data changed: %q", raw.Data)
	}
}

func TestPersonalSourceParsesTopicFallbackAndIgnoresWildcard(t *testing.T) {
	src := personalSourceForRawEventTests()
	raw := src.rawEventFromDataFrame(&payload.DataFrame{
		Headers: payload.DataFrameHeader{
			"TOPIC": "user_im_message_receive_o2o",
			"topic": "*",
		},
	})
	if raw.EventType != "user_im_message_receive_o2o" {
		t.Fatalf("EventType from TOPIC = %q", raw.EventType)
	}

	raw = src.rawEventFromDataFrame(&payload.DataFrame{
		Headers: payload.DataFrameHeader{"topic": "*"},
	})
	if raw.EventType != "" {
		t.Fatalf("EventType from wildcard topic = %q, want empty", raw.EventType)
	}
}

func TestPersonalSourceParsesDataFallbackWithoutChangingData(t *testing.T) {
	src := personalSourceForRawEventTests()
	payloadJSON := `{"eventKey":"user_im_message_receive_o2o","subId":"sub-data","eventId":"evt-data","ext":{"ruleType":"singleChat"}}`
	encodedPayload, err := json.Marshal(payloadJSON)
	if err != nil {
		t.Fatal(err)
	}
	data := string(encodedPayload)

	raw := src.rawEventFromDataFrame(&payload.DataFrame{Data: data})
	if raw.EventType != "user_im_message_receive_o2o" {
		t.Fatalf("EventType = %q", raw.EventType)
	}
	if raw.SubscribeID != "sub-data" {
		t.Fatalf("SubscribeID = %q", raw.SubscribeID)
	}
	if raw.EventID != "evt-data" {
		t.Fatalf("EventID = %q", raw.EventID)
	}
	if raw.RuleType != "singleChat" {
		t.Fatalf("RuleType = %q", raw.RuleType)
	}
	if raw.Data != data {
		t.Fatalf("Data changed: %q", raw.Data)
	}
}

func TestPersonalSourceParsesSourceTagDataFallback(t *testing.T) {
	src := personalSourceForRawEventTests()
	data := `{"source":{"tag":"user_im_message_receive_group"},"subId":"sub-group"}`

	raw := src.rawEventFromDataFrame(&payload.DataFrame{Data: data})
	if raw.EventType != "user_im_message_receive_group" {
		t.Fatalf("EventType = %q", raw.EventType)
	}
	if raw.SubscribeID != "sub-group" {
		t.Fatalf("SubscribeID = %q", raw.SubscribeID)
	}
}

func TestPersonalSourceParsedHeadersPassNormalBusFilter(t *testing.T) {
	src := personalSourceForRawEventTests()
	raw := src.rawEventFromDataFrame(&payload.DataFrame{
		Headers: payload.DataFrameHeader{
			"EVENT_TYPE": "user_im_message_receive_o2o",
			"SUB_ID":     "sub-1",
		},
	})
	h := bus.NewHub(10)
	c, err := h.Register(transport.Hello{
		EventTypes:  []string{"user_im_message_receive_o2o"},
		SubscribeID: "sub-1",
	})
	if err != nil {
		t.Fatal(err)
	}

	h.Deliver(raw)

	select {
	case frame := <-c.SendCh:
		ev, ok := frame.(transport.Event)
		if !ok {
			t.Fatalf("frame = %T, want transport.Event", frame)
		}
		if ev.EventType != "user_im_message_receive_o2o" || ev.SubscribeID != "sub-1" {
			t.Fatalf("event = %#v", ev)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for filtered event")
	}
}

func TestPersonalSourceActionEventPassesNormalBusFilter(t *testing.T) {
	const (
		eventKey    = "user_im_message_reaction_group"
		subscribeID = "sub-reaction-group"
	)
	src := personalSourceForRawEventTests()
	raw := src.rawEventFromDataFrame(&payload.DataFrame{
		Headers: payload.DataFrameHeader{
			"EVENT_TYPE": eventKey,
			"SUB_ID":     subscribeID,
		},
		Data: `{"eventKey":"user_im_message_reaction_group","subId":"sub-reaction-group","payload":{}}`,
	})
	h := bus.NewHub(10)
	consumer, err := h.Register(transport.Hello{
		EventTypes:  []string{eventKey},
		SubscribeID: subscribeID,
	})
	if err != nil {
		t.Fatal(err)
	}

	h.Deliver(raw)

	select {
	case frame := <-consumer.SendCh:
		eventFrame, ok := frame.(transport.Event)
		if !ok {
			t.Fatalf("frame = %T, want transport.Event", frame)
		}
		if eventFrame.EventType != eventKey || eventFrame.SubscribeID != subscribeID {
			t.Fatalf("event = %#v", eventFrame)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for action event")
	}
}

func TestPersonalSourceSenderEventPassesNormalBusFilter(t *testing.T) {
	const (
		eventKey    = "user_im_message_receive_user"
		subscribeID = "sub-receive-user"
	)
	src := personalSourceForRawEventTests()
	raw := src.rawEventFromDataFrame(&payload.DataFrame{
		Headers: payload.DataFrameHeader{
			"EVENT_TYPE": eventKey,
			"SUB_ID":     subscribeID,
		},
		Data: `{"eventKey":"user_im_message_receive_user","subId":"sub-receive-user","payload":{}}`,
	})
	h := bus.NewHub(10)
	consumer, err := h.Register(transport.Hello{
		EventTypes:  []string{eventKey},
		SubscribeID: subscribeID,
	})
	if err != nil {
		t.Fatal(err)
	}

	h.Deliver(raw)

	select {
	case frame := <-consumer.SendCh:
		eventFrame, ok := frame.(transport.Event)
		if !ok {
			t.Fatalf("frame = %T, want transport.Event", frame)
		}
		if eventFrame.EventType != eventKey || eventFrame.SubscribeID != subscribeID {
			t.Fatalf("event = %#v", eventFrame)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for sender event")
	}
}

func personalSourceForRawEventTests() *PersonalSource {
	return &PersonalSource{cfg: PersonalConfig{
		SourceID: "fallback_source",
		Now:      func() time.Time { return time.Unix(20, 0) },
	}}
}

func personalTestDataFrame(attempt int) payload.DataFrame {
	return payload.DataFrame{
		Type: "event",
		Headers: payload.DataFrameHeader{
			payload.DataFrameHeaderKMessageId:     fmt.Sprintf("msg-%d", attempt),
			streamevent.DataFrameHeaderKEventId:   fmt.Sprintf("evt-%d", attempt),
			streamevent.DataFrameHeaderKEventType: "user_im_message_receive_o2o",
			"SUB_ID":                              fmt.Sprintf("sub-%d", attempt),
		},
		Data: fmt.Sprintf(`{"eventKey":"user_im_message_receive_o2o","eventId":"evt-%d"}`, attempt),
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func capturePersonalSourceDebugLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() {
		slog.SetDefault(previous)
	})
	return &buf
}
