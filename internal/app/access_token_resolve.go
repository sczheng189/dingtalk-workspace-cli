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
	"io"
	"log/slog"
	"path/filepath"
	"strings"

	authpkg "github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/auth"
	"github.com/DingTalk-Real-AI/dingtalk-workspace-cli/pkg/edition"
)

type legacyTokenGetter interface {
	GetToken() (string, string, error)
}

var (
	newAccessTokenProvider = func(configDir string) accessTokenGetter {
		disc := slog.New(slog.NewTextHandler(io.Discard, nil))
		provider := authpkg.NewOAuthProvider(configDir, disc)
		configureOAuthProviderCompatibility(provider, configDir)
		return provider
	}
	newLegacyTokenManager = func(configDir string) legacyTokenGetter {
		manager := authpkg.NewManager(configDir, nil)
		configureLegacyAuthManagerCompatibility(manager)
		return manager
	}
)

// resolveAccessTokenFromDir loads OAuth then legacy token from configDir, applying
// the same host compatibility hooks as MCP. It mirrors the former body of
// getCachedRuntimeToken (excluding process-level cache and timing).
//
// The legacy fallback is reached ONLY when OAuth reports ErrNoCredentials
// (truly no usable OAuth credential). A failed refresh_token exchange
// (ErrRefreshFailed) or any other error is returned verbatim so callers can
// surface the real cause instead of having it masked by a stale legacy token.
func resolveAccessTokenFromDir(ctx context.Context, configDir string) (string, error) {
	provider := newAccessTokenProvider(configDir)
	token, tokenErr := provider.GetAccessToken(ctx)
	if tokenErr == nil && strings.TrimSpace(token) != "" {
		return strings.TrimSpace(token), nil
	}
	if tokenErr != nil && errors.Is(tokenErr, authpkg.ErrTokenDecryption) {
		return "", tokenErr
	}
	// RT 交换失败 / RT 过期以外的错误原样透传，不走 legacy 掩盖真实原因。
	if tokenErr != nil && !errors.Is(tokenErr, authpkg.ErrNoCredentials) {
		return "", tokenErr
	}
	if strings.TrimSpace(authpkg.RuntimeProfile()) != "" {
		if tokenErr != nil {
			return "", tokenErr
		}
		return "", nil
	}
	manager := newLegacyTokenManager(configDir)
	if leg, _, err := manager.GetToken(); err == nil && strings.TrimSpace(leg) != "" {
		return strings.TrimSpace(leg), nil
	}
	if tokenErr != nil {
		return "", tokenErr
	}
	return "", nil
}

// ResolveAuxiliaryAccessToken resolves a bearer token for HTTP clients that should
// align with MCP tool calls. Non-empty explicitToken wins. When configDir matches
// the active edition config directory, the same process-cached path as MCP is used.
// Otherwise tokens are loaded from configDir with host compatibility hooks applied.
func ResolveAuxiliaryAccessToken(ctx context.Context, configDir, explicitToken string) (string, error) {
	if t := strings.TrimSpace(explicitToken); t != "" {
		return t, nil
	}
	if strings.TrimSpace(configDir) == "" {
		return "", fmt.Errorf("config directory is empty")
	}
	if filepath.Clean(configDir) == filepath.Clean(defaultConfigDir()) {
		tok, err := resolveRuntimeAuthToken(ctx, "")
		if err != nil {
			// “无凭证”必须转换成带登录引导的用户文案，同时保留 sentinel 供 errors.Is。
			if errors.Is(err, authpkg.ErrNoCredentials) {
				return "", noCredentialsError()
			}
			return "", err
		}
		if tok != "" {
			return tok, nil
		}
		return "", noCredentialsError()
	}
	tok, err := resolveAccessTokenFromDir(ctx, configDir)
	if err != nil {
		if errors.Is(err, authpkg.ErrNoCredentials) {
			return "", noCredentialsError()
		}
		return "", err
	}
	if tok != "" {
		return tok, nil
	}
	return "", noCredentialsError()
}

// noCredentialsError 返回带登录引导的“无凭证”错误：用户可见文案与历史一致，
// 同时包装 authpkg.ErrNoCredentials，调用方仍可用 errors.Is 判断类别。
func noCredentialsError() error {
	if edition.Get().IsEmbedded {
		return authpkg.NewCredentialError("认证信息已失效，请重新认证", authpkg.ErrNoCredentials, nil)
	}
	return authpkg.NewCredentialError("no credentials found, run: dws auth login", authpkg.ErrNoCredentials, nil)
}
