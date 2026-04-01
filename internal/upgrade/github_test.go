// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0

package upgrade

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFetchLatestRelease(t *testing.T) {
	release := GitHubRelease{
		TagName:     "v1.0.6",
		Name:        "v1.0.6",
		Body:        "Bug fixes and improvements",
		Prerelease:  false,
		Draft:       false,
		PublishedAt: "2026-04-01T09:29:05Z",
		Assets: []GitHubAsset{
			{Name: "dws-darwin-arm64.tar.gz", Size: 3942308, BrowserDownloadURL: "https://example.com/dws-darwin-arm64.tar.gz"},
			{Name: "dws-skills.zip", Size: 50000, BrowserDownloadURL: "https://example.com/dws-skills.zip"},
			{Name: "checksums.txt", Size: 534, BrowserDownloadURL: "https://example.com/checksums.txt"},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/DingTalk-Real-AI/dingtalk-workspace-cli/releases/latest" {
			t.Errorf("unexpected path: %s", r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(release)
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)
	info, err := client.FetchLatestRelease()
	if err != nil {
		t.Fatalf("FetchLatestRelease() error = %v", err)
	}

	if info.Version != "1.0.6" {
		t.Errorf("Version = %q, want %q", info.Version, "1.0.6")
	}
	if info.Date != "2026-04-01" {
		t.Errorf("Date = %q, want %q", info.Date, "2026-04-01")
	}
	if len(info.Assets) != 3 {
		t.Errorf("len(Assets) = %d, want 3", len(info.Assets))
	}
}

func TestFetchReleaseByTag(t *testing.T) {
	release := GitHubRelease{
		TagName:     "v1.0.5",
		PublishedAt: "2026-03-25T10:00:00Z",
		Assets: []GitHubAsset{
			{Name: "dws-darwin-arm64.tar.gz", Size: 3800000, BrowserDownloadURL: "https://example.com/dws-darwin-arm64.tar.gz"},
		},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		expected := "/repos/DingTalk-Real-AI/dingtalk-workspace-cli/releases/tags/v1.0.5"
		if r.URL.Path != expected {
			t.Errorf("path = %q, want %q", r.URL.Path, expected)
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(release)
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)

	// Without "v" prefix
	info, err := client.FetchReleaseByTag("1.0.5")
	if err != nil {
		t.Fatalf("FetchReleaseByTag() error = %v", err)
	}
	if info.Version != "1.0.5" {
		t.Errorf("Version = %q, want %q", info.Version, "1.0.5")
	}
}

func TestFetchAllReleases(t *testing.T) {
	releases := []GitHubRelease{
		{TagName: "v1.0.6", PublishedAt: "2026-04-01T09:00:00Z", Draft: false, Prerelease: false},
		{TagName: "v1.0.6-beta", PublishedAt: "2026-03-28T09:00:00Z", Draft: false, Prerelease: true},
		{TagName: "v1.0.5", PublishedAt: "2026-03-25T09:00:00Z", Draft: false, Prerelease: false},
		{TagName: "v1.0.4-draft", PublishedAt: "2026-03-20T09:00:00Z", Draft: true, Prerelease: false},
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(releases)
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)
	versions, err := client.FetchAllReleases()
	if err != nil {
		t.Fatalf("FetchAllReleases() error = %v", err)
	}

	// Draft should be filtered out
	if len(versions) != 3 {
		t.Fatalf("len(versions) = %d, want 3", len(versions))
	}
	if versions[0].Version != "1.0.6" {
		t.Errorf("versions[0].Version = %q, want %q", versions[0].Version, "1.0.6")
	}
	if versions[1].Prerelease != true {
		t.Errorf("versions[1].Prerelease = false, want true")
	}
}

func TestFindBinaryAsset(t *testing.T) {
	assets := []GitHubAsset{
		{Name: "checksums.txt", Size: 534},
		{Name: "dws-darwin-amd64.tar.gz", Size: 4276643},
		{Name: "dws-darwin-arm64.tar.gz", Size: 3942308},
		{Name: "dws-linux-amd64.tar.gz", Size: 4394696},
		{Name: "dws-windows-amd64.zip", Size: 4500000},
		{Name: "dws-skills.zip", Size: 50000},
	}

	tests := []struct {
		goos, goarch string
		wantName     string
		wantErr      bool
	}{
		{"darwin", "arm64", "dws-darwin-arm64.tar.gz", false},
		{"darwin", "amd64", "dws-darwin-amd64.tar.gz", false},
		{"linux", "amd64", "dws-linux-amd64.tar.gz", false},
		{"windows", "amd64", "dws-windows-amd64.zip", false},
		{"freebsd", "amd64", "", true},
	}

	for _, tt := range tests {
		asset, err := FindBinaryAssetFor(assets, tt.goos, tt.goarch)
		if tt.wantErr {
			if err == nil {
				t.Errorf("FindBinaryAssetFor(%s, %s) expected error", tt.goos, tt.goarch)
			}
			continue
		}
		if err != nil {
			t.Errorf("FindBinaryAssetFor(%s, %s) error = %v", tt.goos, tt.goarch, err)
			continue
		}
		if asset.Name != tt.wantName {
			t.Errorf("FindBinaryAssetFor(%s, %s).Name = %q, want %q", tt.goos, tt.goarch, asset.Name, tt.wantName)
		}
	}
}

func TestFindSkillsAsset(t *testing.T) {
	assets := []GitHubAsset{
		{Name: "dws-darwin-arm64.tar.gz"},
		{Name: "dws-skills.zip", Size: 50000},
		{Name: "checksums.txt"},
	}

	skill := FindSkillsAsset(assets)
	if skill == nil {
		t.Fatal("FindSkillsAsset() returned nil")
	}
	if skill.Name != "dws-skills.zip" {
		t.Errorf("Name = %q, want %q", skill.Name, "dws-skills.zip")
	}

	// No skills asset
	noSkills := []GitHubAsset{{Name: "dws-darwin-arm64.tar.gz"}}
	if got := FindSkillsAsset(noSkills); got != nil {
		t.Errorf("FindSkillsAsset() = %v, want nil", got)
	}
}

func TestFindChecksumsAsset(t *testing.T) {
	assets := []GitHubAsset{
		{Name: "dws-darwin-arm64.tar.gz"},
		{Name: "checksums.txt", Size: 534},
	}

	checksums := FindChecksumsAsset(assets)
	if checksums == nil {
		t.Fatal("FindChecksumsAsset() returned nil")
	}
	if checksums.Name != "checksums.txt" {
		t.Errorf("Name = %q, want %q", checksums.Name, "checksums.txt")
	}
}

func TestExtractDigestSHA256(t *testing.T) {
	tests := []struct {
		digest string
		want   string
	}{
		{"sha256:abcdef1234567890", "abcdef1234567890"},
		{"sha256:", ""},
		{"md5:abcdef", ""},
		{"", ""},
	}
	for _, tt := range tests {
		got := ExtractDigestSHA256(tt.digest)
		if got != tt.want {
			t.Errorf("ExtractDigestSHA256(%q) = %q, want %q", tt.digest, got, tt.want)
		}
	}
}

func TestRateLimitError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-RateLimit-Remaining", "0")
		w.WriteHeader(http.StatusForbidden)
	}))
	defer server.Close()

	client := NewClientWithBaseURL(server.URL)
	_, err := client.FetchLatestRelease()
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	if got := err.Error(); !contains(got, "频率超限") {
		t.Errorf("error = %q, want to contain rate limit message", got)
	}
}

func contains(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
