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

package auth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/DingTalk-Real-AI/dingtalk-workspace-cli/pkg/edition"
)

// GetAccessToken 的凭证加载错误分类：只有 ErrTokenDataNotFound 才能归类为
// ErrNoCredentials；keychain 权限、解密失败、profiles 损坏等真实错误必须
// 原样透传（保留 cause），不得被折叠成“未登录”。
func TestGetAccessTokenLoadErrorClassification(t *testing.T) {
	oldLoad := oauthLoadToken
	t.Cleanup(func() { oauthLoadToken = oldLoad })
	p := &OAuthProvider{configDir: t.TempDir()}

	t.Run("token data not found maps to ErrNoCredentials", func(t *testing.T) {
		oauthLoadToken = func(string) (*TokenData, error) {
			return nil, fmt.Errorf("load: %w", ErrTokenDataNotFound)
		}
		_, err := p.GetAccessToken(context.Background())
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("ErrTokenDataNotFound must map to ErrNoCredentials, got %v", err)
		}
	})

	t.Run("keychain or storage error passes through verbatim", func(t *testing.T) {
		storageErr := errors.New("keychain: user interaction not allowed")
		oauthLoadToken = func(string) (*TokenData, error) { return nil, storageErr }
		_, err := p.GetAccessToken(context.Background())
		if !errors.Is(err, storageErr) {
			t.Fatalf("storage error must pass through, got %v", err)
		}
		if errors.Is(err, ErrNoCredentials) {
			t.Fatalf("storage error must NOT be classified as ErrNoCredentials: %v", err)
		}
	})

	t.Run("decryption failure keeps its own sentinel", func(t *testing.T) {
		oauthLoadToken = func(string) (*TokenData, error) {
			return nil, fmt.Errorf("%w: bad key", ErrTokenDecryption)
		}
		_, err := p.GetAccessToken(context.Background())
		if !errors.Is(err, ErrTokenDecryption) {
			t.Fatalf("decryption sentinel lost: %v", err)
		}
		if errors.Is(err, ErrNoCredentials) {
			t.Fatalf("decryption failure must NOT be classified as ErrNoCredentials: %v", err)
		}
	})

	t.Run("os.ErrNotExist maps to ErrNoCredentials (fresh machine / secure store missing)", func(t *testing.T) {
		oauthLoadToken = func(string) (*TokenData, error) {
			return nil, fmt.Errorf("reading secure data file: %w", os.ErrNotExist)
		}
		_, err := p.GetAccessToken(context.Background())
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("os.ErrNotExist must map to ErrNoCredentials, got %v", err)
		}
	})
}

// edition hook 契约：Hooks.LoadToken 无法 import internal 包的 sentinel，公开契约是
// edition.ErrNoCredentials（或 os.ErrNotExist）。core 必须将其映射为“未登录”分类；
// 其余 hook 错误（宿主 RPC 失败等）必须原样透传，不得折叠成“未登录”。
func TestGetAccessTokenEditionHookErrorClassification(t *testing.T) {
	prev := edition.Get()
	t.Cleanup(func() { edition.Override(prev) })
	oldLoad := oauthLoadToken
	t.Cleanup(func() { oauthLoadToken = oldLoad })
	oauthLoadToken = oldLoad // 走真实 LoadTokenData → hook 路径
	p := &OAuthProvider{configDir: t.TempDir()}

	t.Run("hook edition.ErrNoCredentials maps to not-logged-in", func(t *testing.T) {
		edition.Override(&edition.Hooks{
			Name:      "test-overlay",
			LoadToken: func(string) ([]byte, error) { return nil, edition.ErrNoCredentials },
		})
		_, err := p.GetAccessToken(context.Background())
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("hook ErrNoCredentials must map to ErrNoCredentials, got %v", err)
		}
	})

	t.Run("hook os.ErrNotExist maps to not-logged-in", func(t *testing.T) {
		edition.Override(&edition.Hooks{
			Name:      "test-overlay",
			LoadToken: func(string) ([]byte, error) { return nil, fmt.Errorf("open host token: %w", os.ErrNotExist) },
		})
		_, err := p.GetAccessToken(context.Background())
		if !errors.Is(err, ErrNoCredentials) {
			t.Fatalf("hook os.ErrNotExist must map to ErrNoCredentials, got %v", err)
		}
	})

	t.Run("hook generic failure passes through verbatim", func(t *testing.T) {
		hostErr := errors.New("host rpc down")
		edition.Override(&edition.Hooks{
			Name:      "test-overlay",
			LoadToken: func(string) ([]byte, error) { return nil, hostErr },
		})
		_, err := p.GetAccessToken(context.Background())
		if !errors.Is(err, hostErr) {
			t.Fatalf("hook error must pass through, got %v", err)
		}
		if errors.Is(err, ErrNoCredentials) {
			t.Fatalf("hook generic error must NOT be classified as ErrNoCredentials: %v", err)
		}
	})
}

// RT 刷新失败的二分：refresh 端点明确拒绝（400/401/403）→ ErrNoCredentials
// （需重新登录、不会自愈、对长驻进程是致命错误）；网络错误与 5xx →
// ErrRefreshFailed（暂时性、可重试）。
func TestGetAccessTokenRefreshFailureClassification(t *testing.T) {
	oldLoad, oldLoadLocked := oauthLoadToken, oauthLoadTokenLocked
	oldAcquire, oldRefresh, oldMark := oauthAcquireLock, oauthRefreshToken, oauthMarkProfile
	t.Cleanup(func() {
		oauthLoadToken, oauthLoadTokenLocked = oldLoad, oldLoadLocked
		oauthAcquireLock, oauthRefreshToken, oauthMarkProfile = oldAcquire, oldRefresh, oldMark
	})
	expired := &TokenData{
		AccessToken:  "old",
		ExpiresAt:    time.Now().Add(-time.Hour),
		RefreshToken: "rt",
		RefreshExpAt: time.Now().Add(time.Hour),
	}
	oauthLoadToken = func(string) (*TokenData, error) { return expired, nil }
	oauthLoadTokenLocked = func(configDir, _ string) (*TokenData, error) { return oauthLoadToken(configDir) }
	oauthAcquireLock = func(context.Context, string) (*DualLock, error) { return &DualLock{}, nil }
	oauthMarkProfile = func(string, string, string) error { return nil }
	p := &OAuthProvider{configDir: t.TempDir()}

	cases := []struct {
		name       string
		refreshErr error
		want       error
		wantNot    error
	}{
		{"RT rejected 401 → re-login", &HTTPStatusError{Status: 401, Body: "invalid refresh_token"}, ErrNoCredentials, ErrRefreshFailed},
		{"RT rejected 400 → re-login", &HTTPStatusError{Status: 400, Body: "invalid_grant"}, ErrNoCredentials, ErrRefreshFailed},
		{"RT rejected 403 → re-login", &HTTPStatusError{Status: 403, Body: "forbidden"}, ErrNoCredentials, ErrRefreshFailed},
		{"server 500 → transient", &HTTPStatusError{Status: 500, Body: "internal error"}, ErrRefreshFailed, ErrNoCredentials},
		{"rate limited 429 → transient", &HTTPStatusError{Status: 429, Body: "too many requests"}, ErrRefreshFailed, ErrNoCredentials},
		{"network error → transient", errors.New("sending request: connection reset"), ErrRefreshFailed, ErrNoCredentials},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			oauthRefreshToken = func(*OAuthProvider, context.Context, *TokenData) (*TokenData, error) {
				return nil, tc.refreshErr
			}
			_, err := p.GetAccessToken(context.Background())
			if !errors.Is(err, tc.want) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", err, tc.want)
			}
			if errors.Is(err, tc.wantNot) {
				t.Fatalf("error = %v, must NOT match %v", err, tc.wantNot)
			}
		})
	}
}
