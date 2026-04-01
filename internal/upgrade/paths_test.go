// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0

package upgrade

import (
	"os"
	"path/filepath"
	"testing"
)

func TestIsBlacklisted(t *testing.T) {
	tests := []struct {
		dir  string
		want bool
	}{
		{".real/skills", true},
		{".real", true},
		{".agents/skills", false},
		{".claude/skills", false},
		{".cursor/skills", false},
		{".realtime/skills", false},
	}
	for _, tt := range tests {
		got := isBlacklisted(tt.dir)
		if got != tt.want {
			t.Errorf("isBlacklisted(%q) = %v, want %v", tt.dir, got, tt.want)
		}
	}
}

func TestLocateSkillMD(t *testing.T) {
	// Flat layout
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte("# skill"), 0644)

	result := LocateSkillMD(dir)
	if result != dir {
		t.Errorf("flat layout: LocateSkillMD() = %q, want %q", result, dir)
	}

	// Nested layout
	dir2 := t.TempDir()
	os.MkdirAll(filepath.Join(dir2, "dws"), 0755)
	os.WriteFile(filepath.Join(dir2, "dws", "SKILL.md"), []byte("# skill"), 0644)

	result2 := LocateSkillMD(dir2)
	want2 := filepath.Join(dir2, "dws")
	if result2 != want2 {
		t.Errorf("nested layout: LocateSkillMD() = %q, want %q", result2, want2)
	}

	// No SKILL.md
	dir3 := t.TempDir()
	result3 := LocateSkillMD(dir3)
	if result3 != "" {
		t.Errorf("empty dir: LocateSkillMD() = %q, want empty", result3)
	}
}

func TestUpgradeSkillLocations(t *testing.T) {
	// Create a temp skill source
	skillSrc := t.TempDir()
	os.WriteFile(filepath.Join(skillSrc, "SKILL.md"), []byte("# test skill"), 0644)
	os.MkdirAll(filepath.Join(skillSrc, "references"), 0755)
	os.WriteFile(filepath.Join(skillSrc, "references", "guide.md"), []byte("# guide"), 0644)

	// Test the function installs at least to the primary location
	updated, err := UpgradeSkillLocations(skillSrc)
	if err != nil {
		t.Fatalf("UpgradeSkillLocations() error = %v", err)
	}

	if len(updated) == 0 {
		t.Fatal("UpgradeSkillLocations() returned 0 updated locations")
	}

	// Verify primary location has the files
	homeDir, _ := os.UserHomeDir()
	primaryDest := filepath.Join(homeDir, ".agents", "skills", "dws")

	found := false
	for _, u := range updated {
		if u == primaryDest {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("primary location %s not in updated list: %v", primaryDest, updated)
	}

	// Verify SKILL.md was copied
	skillMDPath := filepath.Join(primaryDest, "SKILL.md")
	if _, err := os.Stat(skillMDPath); os.IsNotExist(err) {
		t.Errorf("SKILL.md not found at %s", skillMDPath)
	}

	// Verify references/ was copied
	guidePath := filepath.Join(primaryDest, "references", "guide.md")
	if _, err := os.Stat(guidePath); os.IsNotExist(err) {
		t.Errorf("references/guide.md not found at %s", guidePath)
	}

	// Cleanup
	os.RemoveAll(primaryDest)
}

func TestBlacklistPreventsRealDir(t *testing.T) {
	// Ensure .real is not in knownSkillDirs (it shouldn't be, but verify)
	for _, dir := range knownSkillDirs {
		if isBlacklisted(dir) {
			t.Errorf("knownSkillDirs contains blacklisted entry: %q", dir)
		}
	}
}
