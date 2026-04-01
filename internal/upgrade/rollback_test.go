// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0

package upgrade

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseVersionFromBackupName(t *testing.T) {
	tests := []struct {
		name string
		want string
	}{
		{"v1.0.6-20260401-093000", "1.0.6"},
		{"v0.2.7-20260314-100523", "0.2.7"},
		{"v1.0.0", "1.0.0"},
		{"invalid", "unknown"},
		{"v", "unknown"},
	}
	for _, tt := range tests {
		got := parseVersionFromBackupName(tt.name)
		if got != tt.want {
			t.Errorf("parseVersionFromBackupName(%q) = %q, want %q", tt.name, got, tt.want)
		}
	}
}

func TestRollbackManagerBackupAndList(t *testing.T) {
	dir := t.TempDir()
	rm := NewRollbackManagerWithDir(dir)

	// Create a fake binary to backup
	fakeExe := filepath.Join(dir, "dws")
	os.WriteFile(fakeExe, []byte("#!/bin/sh\necho fake"), 0755)

	// ListBackups on empty dir
	backups, err := rm.ListBackups()
	if err != nil {
		t.Fatalf("ListBackups() error = %v", err)
	}
	if len(backups) != 0 {
		t.Errorf("ListBackups() on empty = %d, want 0", len(backups))
	}
}

func TestRollbackManagerCleanup(t *testing.T) {
	dir := t.TempDir()
	rm := NewRollbackManagerWithDir(dir)

	// Create backup directories with proper timestamps in info.json
	names := []string{"v1.0.1-20260101-010000", "v1.0.2-20260102-020000", "v1.0.3-20260103-030000"}
	for i, name := range names {
		backupDir := filepath.Join(dir, name)
		os.MkdirAll(backupDir, 0755)
		info := BackupInfo{
			Path:      backupDir,
			Version:   "1.0." + string(rune('1'+i)),
			CreatedAt: mustParseTime(t, "2026-01-0"+string(rune('1'+i))+"T01:00:00Z"),
		}
		rm.saveBackupInfo(info)
	}

	// Keep only 1 (the newest)
	if err := rm.Cleanup(1); err != nil {
		t.Fatalf("Cleanup(1) error = %v", err)
	}

	entries, _ := os.ReadDir(dir)
	if len(entries) != 1 {
		t.Errorf("after Cleanup(1), %d entries remain, want 1", len(entries))
	}
}

func mustParseTime(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatal(err)
	}
	return ts
}
