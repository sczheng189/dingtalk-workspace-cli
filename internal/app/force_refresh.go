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
	"fmt"
	"io"
	"log/slog"
	"strings"

	authpkg "github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/auth"
)

type accessTokenGetter interface {
	GetAccessToken(context.Context) (string, error)
}

var (
	markAccessTokenStale          = authpkg.MarkAccessTokenStale
	markAccessTokenStaleIfCurrent = authpkg.MarkAccessTokenStaleIfCurrent
	newRefreshProvider            = func(configDir string) accessTokenGetter {
		disc := slog.New(slog.NewTextHandler(io.Discard, nil))
		provider := authpkg.NewOAuthProvider(configDir, disc)
		configureOAuthProviderCompatibility(provider, configDir)
		return provider
	}
)

// ForceRefreshAccessToken forces a single refresh_token exchange and returns
// the new access_token. It is intended for callers that have observed a
// server-side rejection (HTTP 401 or business code such as
// TOKEN_VERIFIED_FAILED) on what locally appeared to be a still-valid token.
//
// Steps:
//  1. MarkAccessTokenStale rewrites ExpiresAt to a past instant so
//     OAuthProvider.GetAccessToken's fast-path will miss.
//  2. NewOAuthProvider + GetAccessToken triggers lockedRefresh, which uses the
//     existing dual-layer lock (process + file) to serialize concurrent
//     refresh attempts across goroutines and processes.
//  3. ResetRuntimeTokenCache clears the per-process sync.Once cache so the
//     next resolveAuthToken call re-reads from disk.
//
// Existing OAuthProvider.GetAccessToken behaviour is unchanged; this helper
// is the only entry point that orchestrates "force refresh" semantics.
func ForceRefreshAccessToken(ctx context.Context, configDir string) (string, error) {
	if strings.TrimSpace(configDir) == "" {
		return "", fmt.Errorf("config directory is empty")
	}
	// 该 access_token 已被服务端明确拒绝：无论本地 stale 标记成功与否，
	// 都不能让进程继续缓存复用它长达一个 TTL，先清缓存再标记。
	ResetRuntimeTokenCache()
	if err := markAccessTokenStale(configDir); err != nil {
		return "", fmt.Errorf("mark access token stale: %w", err)
	}
	provider := newRefreshProvider(configDir)
	tok, err := provider.GetAccessToken(ctx)
	if err != nil {
		return "", err
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", fmt.Errorf("force refresh returned empty access token")
	}
	return tok, nil
}

// ForceRefreshAccessTokenIfCurrent is the concurrency-safe form for callers
// that know exactly which access_token the server rejected. The stale marking
// only happens when the persisted access_token still equals rejectedToken;
// if another goroutine/process has already refreshed (persisted token differs),
// the refresh exchange is skipped and GetAccessToken simply returns the newer
// token from disk.
//
// Returns the (possibly already-refreshed) access token. The refresh itself is
// serialized by lockedRefresh's dual-layer lock, so at most one RT exchange is
// performed even under concurrent 401s.
func ForceRefreshAccessTokenIfCurrent(ctx context.Context, configDir, rejectedToken string) (string, error) {
	if strings.TrimSpace(configDir) == "" {
		return "", fmt.Errorf("config directory is empty")
	}
	// 该 access_token 已被服务端明确拒绝：无论本地 stale 标记成功与否，
	// 都不能让进程继续缓存复用它长达一个 TTL，先清缓存再标记。
	ResetRuntimeTokenCache()
	if _, err := markAccessTokenStaleIfCurrent(configDir, rejectedToken); err != nil {
		return "", fmt.Errorf("mark access token stale: %w", err)
	}
	provider := newRefreshProvider(configDir)
	tok, err := provider.GetAccessToken(ctx)
	if err != nil {
		return "", err
	}
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return "", fmt.Errorf("force refresh returned empty access token")
	}
	return tok, nil
}
