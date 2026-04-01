// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0

package upgrade

import (
	"os"
	"path/filepath"
	"runtime"
)

// Permission constants following Unix best practices.
const (
	dirPermSecure  os.FileMode = 0o700
	dirPermShared  os.FileMode = 0o755
	filePermBinary os.FileMode = 0o755
	filePermConfig os.FileMode = 0o644
)

// knownSkillDirs lists all known Agent skill directories (relative to $HOME).
// Kept in sync with build/npm/install.js AGENT_DIRS.
// The first entry (.agents/skills) is always updated; subsequent entries are
// only updated when their parent directory already exists.
var knownSkillDirs = []string{
	".agents/skills",
	".claude/skills",
	".cursor/skills",
	".gemini/skills",
	".codex/skills",
	".github/skills",
	".windsurf/skills",
	".augment/skills",
	".cline/skills",
	".amp/skills",
	".kiro/skills",
	".trae/skills",
}

// skillDirBlacklist contains parent directories whose skills are managed by
// external mechanisms (e.g. IDE extensions) and must NOT be touched by upgrade.
var skillDirBlacklist = []string{
	".real",
}

// UpgradeSkillLocations installs skills from extractedDir into all locations
// where they are currently installed or expected.
//
// Strategy (matches npm install.js installSkillsToHomes):
//   - ~/.agents/skills/dws/ is ALWAYS updated (primary install location)
//   - Other agent dirs (claude, cursor, ...) are updated only when the parent
//     directory exists (e.g. ~/.claude/ exists => user has Claude)
//   - ~/.real/ and other blacklisted paths are NEVER touched
//   - If no location was updated at all, fall back to ~/.agents/skills/dws/
//
// Returns the list of directories that were updated.
func UpgradeSkillLocations(extractedDir string) ([]string, error) {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return nil, err
	}

	var updated []string
	for i, agentDir := range knownSkillDirs {
		if isBlacklisted(agentDir) {
			continue
		}

		destDir := filepath.Join(homeDir, agentDir, "dws")

		if i > 0 {
			// For non-primary dirs, only update when the agent's parent dir exists.
			// e.g. for ".claude/skills", check if ~/.claude exists.
			parentGate := filepath.Dir(filepath.Join(homeDir, agentDir))
			if _, err := os.Stat(parentGate); os.IsNotExist(err) {
				continue
			}
		}

		os.RemoveAll(destDir)
		if err := copyDir(extractedDir, destDir); err != nil {
			continue
		}
		updated = append(updated, destDir)
	}

	// Fallback: ensure at least one location has skills
	if len(updated) == 0 {
		dest := filepath.Join(homeDir, ".agents", "skills", "dws")
		os.MkdirAll(filepath.Dir(dest), dirPermShared)
		if err := copyDir(extractedDir, dest); err != nil {
			return nil, err
		}
		updated = append(updated, dest)
	}

	return updated, nil
}

// LocateSkillMD finds the directory containing SKILL.md in an extracted zip.
// It handles both flat layouts (SKILL.md at root) and nested layouts (dws/SKILL.md).
func LocateSkillMD(extractDir string) string {
	// Check nested: {extractDir}/dws/SKILL.md
	nested := filepath.Join(extractDir, "dws", "SKILL.md")
	if _, err := os.Stat(nested); err == nil {
		return filepath.Join(extractDir, "dws")
	}

	// Check flat: {extractDir}/SKILL.md
	flat := filepath.Join(extractDir, "SKILL.md")
	if _, err := os.Stat(flat); err == nil {
		return extractDir
	}

	return ""
}

// EnsureUpgradeDirectories creates the directories needed for upgrade operations.
func EnsureUpgradeDirectories() error {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return err
	}

	dirs := []struct {
		path string
		perm os.FileMode
	}{
		{filepath.Join(homeDir, ".dws"), dirPermSecure},
		{filepath.Join(homeDir, ".dws", "data"), dirPermSecure},
		{filepath.Join(homeDir, ".dws", "data", "backups"), dirPermSecure},
		{filepath.Join(homeDir, ".dws", "cache"), dirPermSecure},
		{filepath.Join(homeDir, ".dws", "cache", "downloads"), dirPermSecure},
	}

	for _, d := range dirs {
		if err := ensureDir(d.path, d.perm); err != nil {
			return err
		}
	}
	return nil
}

// DownloadCacheDir returns the path for temporary downloads during upgrade.
func DownloadCacheDir() string {
	homeDir, _ := os.UserHomeDir()
	return filepath.Join(homeDir, ".dws", "cache", "downloads")
}

// CurrentBinaryPath returns the resolved path of the currently running binary.
func CurrentBinaryPath() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

// BinaryName returns the platform-specific binary name.
func BinaryName() string {
	if runtime.GOOS == "windows" {
		return "dws.exe"
	}
	return "dws"
}

func isBlacklisted(agentDir string) bool {
	for _, bl := range skillDirBlacklist {
		// agentDir is like ".real/skills" — check if it starts with a blacklisted prefix
		if len(agentDir) >= len(bl) && agentDir[:len(bl)] == bl {
			next := len(bl)
			if next == len(agentDir) || agentDir[next] == '/' {
				return true
			}
		}
	}
	return false
}

func ensureDir(path string, perm os.FileMode) error {
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return os.MkdirAll(path, perm)
	}
	if err != nil {
		return err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != perm {
		if info.Mode().Perm()&^perm != 0 {
			return os.Chmod(path, perm)
		}
	}
	return nil
}
