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
	"testing"
	"time"
)

// MarkAccessTokenStaleIfCurrent 的并发安全语义：比较与标记必须原子完成，
// 盘上 AT 已被并发刷新（与 rejectedToken 不同）时不得再改动凭证。
func TestMarkAccessTokenStaleIfCurrent(t *testing.T) {
	t.Run("empty rejected token is rejected", func(t *testing.T) {
		dir := t.TempDir()
		if _, err := MarkAccessTokenStaleIfCurrent(dir, "  "); err == nil {
			t.Fatal("empty rejected token succeeded")
		}
	})

	t.Run("matching token is marked stale preserving credentials", func(t *testing.T) {
		dir := t.TempDir()
		original := &TokenData{
			AccessToken:  "old-at",
			RefreshToken: "keep-rt",
			ExpiresAt:    time.Now().Add(time.Hour),
			RefreshExpAt: time.Now().Add(24 * time.Hour),
		}
		if err := SaveTokenData(dir, original); err != nil {
			t.Fatal(err)
		}
		changed, err := MarkAccessTokenStaleIfCurrent(dir, "old-at")
		if err != nil || !changed {
			t.Fatalf("mark matching = changed %v, err %v", changed, err)
		}
		loaded, err := LoadTokenData(dir)
		if err != nil {
			t.Fatal(err)
		}
		if !loaded.ExpiresAt.Before(time.Now()) {
			t.Fatalf("ExpiresAt was not moved to the past: %v", loaded.ExpiresAt)
		}
		if loaded.AccessToken != "old-at" || loaded.RefreshToken != "keep-rt" {
			t.Fatalf("credentials were not preserved: %#v", loaded)
		}
	})

	t.Run("different persisted token is left untouched", func(t *testing.T) {
		dir := t.TempDir()
		freshExpiry := time.Now().Add(time.Hour)
		original := &TokenData{
			AccessToken:  "new-at",
			RefreshToken: "new-rt",
			ExpiresAt:    freshExpiry,
		}
		if err := SaveTokenData(dir, original); err != nil {
			t.Fatal(err)
		}
		changed, err := MarkAccessTokenStaleIfCurrent(dir, "old-at")
		if err != nil {
			t.Fatal(err)
		}
		if changed {
			t.Fatal("stale mark reported changed for a different persisted token")
		}
		loaded, err := LoadTokenData(dir)
		if err != nil {
			t.Fatal(err)
		}
		if loaded.AccessToken != "new-at" || !loaded.ExpiresAt.Equal(freshExpiry) {
			t.Fatalf("persisted token was modified: %#v", loaded)
		}
	})

	t.Run("missing token data propagates load error", func(t *testing.T) {
		// 隔离本机 keychain / secure store，避免测试读到真实凭证。
		oldExists, oldSecure := tokenKeychainExists, tokenLoadSecure
		t.Cleanup(func() { tokenKeychainExists, tokenLoadSecure = oldExists, oldSecure })
		tokenKeychainExists = func() bool { return false }
		tokenLoadSecure = func(string) (*TokenData, error) { return nil, ErrTokenDataNotFound }
		dir := t.TempDir()
		if _, err := MarkAccessTokenStaleIfCurrent(dir, "any"); err == nil {
			t.Fatal("missing token data succeeded")
		}
	})
}
