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

package app

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	authpkg "github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/auth"
)

// 隔离缓存与 clock 的测试环境。
func isolateRuntimeTokenCache(t *testing.T) {
	t.Helper()
	oldTokens := cachedRuntimeTokens
	oldTTL := runtimeTokenCacheTTL
	oldNow := runtimeTokenNow
	cachedRuntimeTokenMu.Lock()
	cachedRuntimeTokens = map[string]cachedRuntimeTokenEntry{}
	cachedRuntimeTokenMu.Unlock()
	t.Cleanup(func() {
		cachedRuntimeTokenMu.Lock()
		cachedRuntimeTokens = oldTokens
		cachedRuntimeTokenMu.Unlock()
		runtimeTokenCacheTTL = oldTTL
		runtimeTokenNow = oldNow
		authpkg.SetRuntimeProfile("")
	})
}

// 用 fakeAccessTokenGetter 替换 OAuth provider，返回指定序列的 token。
func stubTokenProvider(t *testing.T, tokens ...string) *atomic.Int32 {
	t.Helper()
	oldProvider := newAccessTokenProvider
	t.Cleanup(func() { newAccessTokenProvider = oldProvider })
	var calls atomic.Int32
	newAccessTokenProvider = func(string) accessTokenGetter {
		n := int(calls.Add(1))
		tok := tokens[len(tokens)-1]
		if n <= len(tokens) {
			tok = tokens[n-1]
		}
		return fakeAccessTokenGetter{token: tok}
	}
	return &calls
}

func TestRuntimeTokenCacheTTL(t *testing.T) {
	t.Run("cache hit within TTL resolves only once", func(t *testing.T) {
		isolateRuntimeTokenCache(t)
		calls := stubTokenProvider(t, "token-1")
		configDir := t.TempDir()
		t.Setenv("DWS_CONFIG_DIR", configDir)

		for i := 0; i < 3; i++ {
			tok, err := getCachedRuntimeToken(context.Background())
			if err != nil || tok != "token-1" {
				t.Fatalf("call %d = %q, %v", i, tok, err)
			}
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("provider calls = %d, want 1", got)
		}
	})

	t.Run("expired TTL re-resolves and returns fresh token", func(t *testing.T) {
		isolateRuntimeTokenCache(t)
		calls := stubTokenProvider(t, "token-old", "token-new")
		configDir := t.TempDir()
		t.Setenv("DWS_CONFIG_DIR", configDir)

		now := time.Now()
		runtimeTokenNow = func() time.Time { return now }
		tok, err := getCachedRuntimeToken(context.Background())
		if err != nil || tok != "token-old" {
			t.Fatalf("first = %q, %v", tok, err)
		}

		// 前进 61 秒（注入 clock，无真实 Sleep）
		now = now.Add(61 * time.Second)
		tok, err = getCachedRuntimeToken(context.Background())
		if err != nil || tok != "token-new" {
			t.Fatalf("after TTL = %q, %v", tok, err)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("provider calls = %d, want 2", got)
		}
	})

	t.Run("reset forces re-resolution", func(t *testing.T) {
		isolateRuntimeTokenCache(t)
		calls := stubTokenProvider(t, "token-a", "token-b")
		configDir := t.TempDir()
		t.Setenv("DWS_CONFIG_DIR", configDir)

		if _, err := getCachedRuntimeToken(context.Background()); err != nil {
			t.Fatal(err)
		}
		ResetRuntimeTokenCache()
		tok, err := getCachedRuntimeToken(context.Background())
		if err != nil || tok != "token-b" {
			t.Fatalf("after reset = %q, %v", tok, err)
		}
		if got := calls.Load(); got != 2 {
			t.Fatalf("provider calls = %d, want 2", got)
		}
	})

	t.Run("errors are never cached", func(t *testing.T) {
		isolateRuntimeTokenCache(t)
		oldProvider := newAccessTokenProvider
		t.Cleanup(func() { newAccessTokenProvider = oldProvider })
		var calls atomic.Int32
		newAccessTokenProvider = func(string) accessTokenGetter {
			if calls.Add(1) == 1 {
				return fakeAccessTokenGetter{err: authpkg.NewCredentialError("refresh boom", authpkg.ErrRefreshFailed, errors.New("boom"))}
			}
			return fakeAccessTokenGetter{token: "token-later"}
		}
		configDir := t.TempDir()
		t.Setenv("DWS_CONFIG_DIR", configDir)

		if _, err := getCachedRuntimeToken(context.Background()); !errors.Is(err, authpkg.ErrRefreshFailed) {
			t.Fatalf("first error = %v", err)
		}
		// 错误不缓存：TTL 未到期也会重新解析并成功。
		tok, err := getCachedRuntimeToken(context.Background())
		if err != nil || tok != "token-later" {
			t.Fatalf("retry = %q, %v", tok, err)
		}
	})

	t.Run("per-profile cache isolation", func(t *testing.T) {
		isolateRuntimeTokenCache(t)
		oldProvider := newAccessTokenProvider
		t.Cleanup(func() { newAccessTokenProvider = oldProvider })
		newAccessTokenProvider = func(string) accessTokenGetter {
			return fakeAccessTokenGetter{token: "tok-" + authpkg.RuntimeProfile()}
		}
		configDir := t.TempDir()
		t.Setenv("DWS_CONFIG_DIR", configDir)

		authpkg.SetRuntimeProfile("corp1:user1")
		tok1, err := getCachedRuntimeToken(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		authpkg.SetRuntimeProfile("corp2:user2")
		tok2, err := getCachedRuntimeToken(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if tok1 == tok2 {
			t.Fatalf("profiles share cache entry: %q vs %q", tok1, tok2)
		}
	})

	t.Run("concurrent access is safe and consistent", func(t *testing.T) {
		isolateRuntimeTokenCache(t)
		stubTokenProvider(t, "token-concurrent")
		configDir := t.TempDir()
		t.Setenv("DWS_CONFIG_DIR", configDir)

		var wg sync.WaitGroup
		errs := make(chan error, 32)
		for i := 0; i < 32; i++ {
			wg.Add(1)
			go func(n int) {
				defer wg.Done()
				tok, err := getCachedRuntimeToken(context.Background())
				if err != nil {
					errs <- fmt.Errorf("goroutine %d: %w", n, err)
					return
				}
				if tok != "token-concurrent" {
					errs <- fmt.Errorf("goroutine %d: token %q", n, tok)
				}
			}(i)
		}
		wg.Wait()
		close(errs)
		for err := range errs {
			t.Fatal(err)
		}
	})
}

// 强刷流程中，只要 stale 标记成功就必须立即清空进程缓存——即使随后的 RT 交换
// 失败，也不能让已被服务端拒绝的 token 继续被缓存复用长达一个 TTL。
func TestForceRefreshClearsCacheOnRefreshFailure(t *testing.T) {
	isolateRuntimeTokenCache(t)
	oldMark, oldMarkIf, oldProv := markAccessTokenStale, markAccessTokenStaleIfCurrent, newRefreshProvider
	t.Cleanup(func() {
		markAccessTokenStale, markAccessTokenStaleIfCurrent, newRefreshProvider = oldMark, oldMarkIf, oldProv
	})
	markAccessTokenStale = func(string) error { return nil }
	markAccessTokenStaleIfCurrent = func(string, string) (bool, error) { return true, nil }
	newRefreshProvider = func(string) accessTokenGetter {
		return fakeAccessTokenGetter{err: errors.New("rt exchange down")}
	}

	seedCache := func() {
		cachedRuntimeTokenMu.Lock()
		cachedRuntimeTokens["__default__"] = cachedRuntimeTokenEntry{token: "rejected-at", loadedAt: runtimeTokenNow()}
		cachedRuntimeTokenMu.Unlock()
	}
	assertCleared := func(name string) {
		cachedRuntimeTokenMu.Lock()
		defer cachedRuntimeTokenMu.Unlock()
		if len(cachedRuntimeTokens) != 0 {
			t.Fatalf("%s: rejected token still cached after failed force refresh", name)
		}
	}

	seedCache()
	if _, err := ForceRefreshAccessToken(context.Background(), t.TempDir()); err == nil {
		t.Fatal("ForceRefreshAccessToken succeeded, want refresh failure")
	}
	assertCleared("ForceRefreshAccessToken")

	seedCache()
	if _, err := ForceRefreshAccessTokenIfCurrent(context.Background(), t.TempDir(), "rejected-at"); err == nil {
		t.Fatal("ForceRefreshAccessTokenIfCurrent succeeded, want refresh failure")
	}
	assertCleared("ForceRefreshAccessTokenIfCurrent")

	// mark 失败（如本地持久化损坏）同样必须清缓存：AT 已被服务端明确拒绝，
	// 不能因为本地标记没写成就继续复用它。
	markAccessTokenStale = func(string) error { return errors.New("profiles corrupted") }
	markAccessTokenStaleIfCurrent = func(string, string) (bool, error) {
		return false, errors.New("profiles corrupted")
	}
	seedCache()
	if _, err := ForceRefreshAccessToken(context.Background(), t.TempDir()); err == nil {
		t.Fatal("ForceRefreshAccessToken succeeded, want mark failure")
	}
	assertCleared("ForceRefreshAccessToken/mark-failure")
	seedCache()
	if _, err := ForceRefreshAccessTokenIfCurrent(context.Background(), t.TempDir(), "rejected-at"); err == nil {
		t.Fatal("ForceRefreshAccessTokenIfCurrent succeeded, want mark failure")
	}
	assertCleared("ForceRefreshAccessTokenIfCurrent/mark-failure")
}
