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

package helpers

import (
	"testing"
	"time"
)

func TestParseFlexTimeToMillis_RFC3339(t *testing.T) {
	t.Parallel()
	ms, err := parseFlexTimeToMillis("start", "2026-03-10T00:00:00+08:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms <= 0 {
		t.Errorf("expected positive milliseconds, got %d", ms)
	}
}

func TestParseFlexTimeToMillis_SpaceSeparated(t *testing.T) {
	t.Parallel()
	// This is the format that was causing the HTTP 400 error
	ms, err := parseFlexTimeToMillis("start", "2026-03-01 00:00:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms <= 0 {
		t.Errorf("expected positive milliseconds, got %d", ms)
	}
}

func TestParseFlexTimeToMillis_DateOnly(t *testing.T) {
	t.Parallel()
	ms, err := parseFlexTimeToMillis("start", "2026-03-01")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms <= 0 {
		t.Errorf("expected positive milliseconds, got %d", ms)
	}
}

func TestParseFlexTimeToMillis_NoTimezone(t *testing.T) {
	t.Parallel()
	ms, err := parseFlexTimeToMillis("start", "2026-03-10T14:00:00")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms <= 0 {
		t.Errorf("expected positive milliseconds, got %d", ms)
	}
}

func TestParseFlexTimeToMillis_SlashFormat(t *testing.T) {
	t.Parallel()
	ms, err := parseFlexTimeToMillis("start", "2026/03/10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms <= 0 {
		t.Errorf("expected positive milliseconds, got %d", ms)
	}
}

func TestParseFlexTimeToMillis_CompactFormat(t *testing.T) {
	t.Parallel()
	ms, err := parseFlexTimeToMillis("start", "20260310")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ms <= 0 {
		t.Errorf("expected positive milliseconds, got %d", ms)
	}
}

func TestParseFlexTimeToMillis_Empty(t *testing.T) {
	t.Parallel()
	_, err := parseFlexTimeToMillis("start", "")
	if err == nil {
		t.Fatal("expected error for empty value")
	}
}

func TestParseFlexTimeToMillis_Invalid(t *testing.T) {
	t.Parallel()
	_, err := parseFlexTimeToMillis("start", "not-a-date")
	if err == nil {
		t.Fatal("expected error for invalid date")
	}
}

func TestValidateTimeRange_Valid(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	end := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	if err := validateTimeRange(start, end); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestValidateTimeRange_Invalid(t *testing.T) {
	t.Parallel()
	start := time.Date(2026, 3, 10, 0, 0, 0, 0, time.UTC).UnixMilli()
	end := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if err := validateTimeRange(start, end); err == nil {
		t.Fatal("expected error when end is before start")
	}
}

func TestValidateTimeRange_Equal(t *testing.T) {
	t.Parallel()
	ts := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC).UnixMilli()
	if err := validateTimeRange(ts, ts); err == nil {
		t.Fatal("expected error when start equals end")
	}
}

func TestParseUserIDs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		input string
		want  int
	}{
		{"user1,user2,user3", 3},
		{"user1", 1},
		{"user1, user2, user3", 3},
		{"user1,,user2", 2},
		{"", 0},
		{" , , ", 0},
	}
	for _, tt := range tests {
		got := parseUserIDs(tt.input)
		if len(got) != tt.want {
			t.Errorf("parseUserIDs(%q) = %d items, want %d", tt.input, len(got), tt.want)
		}
	}
}
