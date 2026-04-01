// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0

package upgrade

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
)

// VerifySHA256 verifies a file against its expected SHA256 hash.
func VerifySHA256(filePath, expectedHash string) error {
	actual, err := ComputeSHA256(filePath)
	if err != nil {
		return err
	}

	expectedHash = strings.ToLower(strings.TrimSpace(expectedHash))
	if actual != expectedHash {
		return fmt.Errorf("SHA256 校验失败: 期望 %s..., 实际 %s...", expectedHash[:16], actual[:16])
	}
	return nil
}

// ComputeSHA256 computes the SHA256 hash of a file.
func ComputeSHA256(filePath string) (string, error) {
	data, err := os.ReadFile(filePath)
	if err != nil {
		return "", fmt.Errorf("读取文件失败: %w", err)
	}
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:]), nil
}

// ParseChecksumFile parses a SHA256SUMS-style file.
// Each line: "hash  filename" (two spaces or whitespace separated).
// Returns a map of filename -> lowercase hex hash.
func ParseChecksumFile(content string) map[string]string {
	result := make(map[string]string)
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.Fields(line)
		if len(parts) >= 2 {
			hash := strings.ToLower(parts[0])
			filename := parts[1]
			result[filename] = hash
		}
	}
	return result
}

// VerifyFileFromChecksums verifies a downloaded file against checksums.txt content.
func VerifyFileFromChecksums(filePath, filename, checksumsContent string) error {
	checksums := ParseChecksumFile(checksumsContent)
	expectedHash, ok := checksums[filename]
	if !ok {
		return fmt.Errorf("在校验和文件中未找到 %s", filename)
	}
	return VerifySHA256(filePath, expectedHash)
}
