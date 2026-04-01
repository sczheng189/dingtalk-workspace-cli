// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0

package upgrade

import (
	"archive/zip"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// ReplaceSelf atomically replaces the currently running binary with newBinaryPath.
// Strategy:
//  1. Resolve current exe path (follow symlinks)
//  2. Set correct permissions (0755) on new binary
//  3. Try atomic os.Rename (same filesystem, instant swap)
//  4. Fallback to copy for cross-device or Windows locks
//  5. Sync to disk for durability
func ReplaceSelf(newBinaryPath string) error {
	currentExe, err := os.Executable()
	if err != nil {
		return fmt.Errorf("无法获取当前二进制路径: %w", err)
	}
	currentExe, err = filepath.EvalSymlinks(currentExe)
	if err != nil {
		return fmt.Errorf("无法解析符号链接: %w", err)
	}

	if err := os.Chmod(newBinaryPath, filePermBinary); err != nil {
		return fmt.Errorf("设置权限失败: %w", err)
	}

	if err := os.Rename(newBinaryPath, currentExe); err != nil {
		if err := copyFile(newBinaryPath, currentExe, filePermBinary); err != nil {
			return err
		}
	}

	syncParentDir(currentExe)
	return nil
}

// ExtractZip unzips zipPath contents into targetDir.
// Contains zip-slip protection against path traversal attacks.
func ExtractZip(zipPath, targetDir string) error {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return fmt.Errorf("打开 zip 失败: %w", err)
	}
	defer r.Close()

	targetDir = filepath.Clean(targetDir)
	for _, f := range r.File {
		destPath := filepath.Join(targetDir, f.Name)
		rel, err := filepath.Rel(targetDir, destPath)
		if err != nil || strings.HasPrefix(rel, "..") {
			continue // zip-slip guard
		}
		if f.FileInfo().IsDir() {
			os.MkdirAll(destPath, dirPermShared)
			continue
		}
		if err := extractZipEntry(f, destPath); err != nil {
			return err
		}
	}
	return nil
}

// FindBinaryInDir recursively finds the dws binary in an extracted directory.
func FindBinaryInDir(dir string) string {
	candidates := []string{
		filepath.Join(dir, "dws"),
		filepath.Join(dir, "dws.exe"),
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}

	var found string
	filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		name := info.Name()
		if name == "dws" || name == "dws.exe" {
			found = path
			return filepath.SkipAll
		}
		return nil
	})
	return found
}

func extractZipEntry(f *zip.File, destPath string) error {
	os.MkdirAll(filepath.Dir(destPath), dirPermShared)

	rc, err := f.Open()
	if err != nil {
		return fmt.Errorf("读取 zip 条目失败: %w", err)
	}
	defer rc.Close()

	out, err := os.OpenFile(destPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, filePermConfig)
	if err != nil {
		return fmt.Errorf("创建文件失败: %w", err)
	}
	defer out.Close()

	_, err = io.Copy(out, rc)
	return err
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	defer out.Close()

	if _, err = io.Copy(out, in); err != nil {
		return err
	}
	return out.Sync()
}

func syncParentDir(path string) {
	dir := filepath.Dir(path)
	f, err := os.Open(dir)
	if err != nil {
		return
	}
	f.Sync()
	f.Close()
}
