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
	"fmt"
	"strings"
	"time"
)

// MarkAccessTokenStale loads the persisted TokenData, sets ExpiresAt to a past
// instant (preserving access_token and refresh_token), and writes it back. The
// next OAuthProvider.GetAccessToken call will see IsAccessTokenValid() == false
// and proceed to lockedRefresh, exchanging the refresh_token for a fresh
// access_token.
//
// Use this only when the server has rejected the current access_token but the
// local expiry has not yet elapsed (zombie token scenario). It does not delete
// any token material.
//
// CONCURRENCY WARNING: the load and the save run under two SEPARATE locks, so
// two concurrent callers can interleave and the loser's stale snapshot can
// overwrite the winner's freshly refreshed token. Prefer
// MarkAccessTokenStaleIfCurrent whenever the caller knows which access_token
// the server rejected — it compares and marks under a single lock.
//
// Returns the original load error when there is no usable token on disk; a
// nil error when there is no access_token to invalidate (no-op).
func MarkAccessTokenStale(configDir string) error {
	data, err := LoadTokenData(configDir)
	if err != nil {
		return err
	}
	if data == nil || data.AccessToken == "" {
		return nil
	}
	data.ExpiresAt = time.Now().Add(-1 * time.Minute)
	return SaveTokenData(configDir, data)
}

// MarkAccessTokenStaleIfCurrent is the concurrency-safe variant of
// MarkAccessTokenStale for the "server rejected this exact access_token" flow.
// The comparison and the ExpiresAt rewrite happen under ONE profiles lock:
//
//   - persisted access_token != rejectedToken → another goroutine/process has
//     already refreshed; the persisted data is left untouched and changed=false
//     is returned (caller should re-read and reuse the newer token).
//   - persisted access_token == rejectedToken → only ExpiresAt is moved to the
//     past (access_token / refresh_token preserved), so the next
//     GetAccessToken call performs the refresh exchange exactly once.
//
// Returns the original load error when there is no usable token on disk; a nil
// error with changed=false when there is no access_token to invalidate.
func MarkAccessTokenStaleIfCurrent(configDir, rejectedToken string) (changed bool, err error) {
	rejectedToken = strings.TrimSpace(rejectedToken)
	if rejectedToken == "" {
		return false, fmt.Errorf("rejected access token is empty")
	}
	err = withProfilesLock(configDir, func() error {
		data, loadErr := loadTokenDataForProfileLocked(configDir, RuntimeProfile())
		if loadErr != nil {
			return loadErr
		}
		if data == nil || data.AccessToken == "" {
			return nil
		}
		if data.AccessToken != rejectedToken {
			// 另一个请求已刷新过，保留较新的凭证，不做修改。
			return nil
		}
		data.ExpiresAt = time.Now().Add(-1 * time.Minute)
		changed = true
		return saveTokenDataLocked(configDir, data)
	})
	return changed, err
}
