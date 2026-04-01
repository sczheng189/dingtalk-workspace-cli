// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0

package upgrade

import (
	"strconv"
	"strings"
)

// CompareVersions compares two semver strings (e.g. "1.0.5", "v1.0.6-beta").
// Returns -1 if a < b, 0 if equal, 1 if a > b.
// Only the numeric major.minor.patch segments are compared; prerelease suffixes
// after "-" are stripped before comparison.
func CompareVersions(a, b string) int {
	pa := parseVersionParts(a)
	pb := parseVersionParts(b)
	for i := 0; i < 3; i++ {
		if pa[i] < pb[i] {
			return -1
		}
		if pa[i] > pb[i] {
			return 1
		}
	}
	return 0
}

// NeedsUpgrade returns true when remoteVersion is newer than currentVersion.
func NeedsUpgrade(currentVersion, remoteVersion string) bool {
	return CompareVersions(currentVersion, remoteVersion) < 0
}

func parseVersionParts(v string) [3]int {
	v = strings.TrimPrefix(v, "v")
	if idx := strings.IndexByte(v, '-'); idx >= 0 {
		v = v[:idx]
	}
	parts := strings.SplitN(v, ".", 3)
	var result [3]int
	for i, p := range parts {
		if i < 3 {
			result[i], _ = strconv.Atoi(p)
		}
	}
	return result
}
