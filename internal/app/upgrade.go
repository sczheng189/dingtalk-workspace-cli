// Copyright 2026 Alibaba Group
// Licensed under the Apache License, Version 2.0

package app

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/DingTalk-Real-AI/dingtalk-workspace-cli/internal/upgrade"
	"github.com/spf13/cobra"
)

func newUpgradeCommand() *cobra.Command {
	var (
		flagCheck      bool
		flagList       bool
		flagVersion    string
		flagRollback   bool
		flagForce      bool
		flagSkipSkills bool
		flagSkipVerify bool
	)

	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "升级 DWS CLI 到最新版本",
		Long: `检查并升级 DWS CLI 到最新版本。

升级内容:
  • CLI 二进制文件（自动匹配当前平台）
  • 技能包 dws-skills（SKILL.md + references + scripts）

安全验证:
  • SHA256 校验和验证

示例:
  dws upgrade              # 交互式升级到最新版本
  dws upgrade --check      # 仅检查是否有新版本
  dws upgrade --list       # 列出所有可用版本
  dws upgrade --version 1.0.5  # 升级到指定版本
  dws upgrade --rollback   # 回滚到上一版本
  dws upgrade -y           # 跳过确认直接升级`,
		RunE: func(cmd *cobra.Command, args []string) error {
			yes, _ := cmd.Flags().GetBool("yes")

			if flagList {
				return runUpgradeList()
			}
			if flagRollback {
				return runUpgradeRollback(yes)
			}
			if flagCheck {
				return runUpgradeCheck()
			}
			return runUpgrade(cmd.Context(), upgradeOptions{
				targetVersion: flagVersion,
				force:         flagForce,
				skipSkills:    flagSkipSkills,
				skipVerify:    flagSkipVerify,
				yes:           yes,
			})
		},
	}

	cmd.Flags().BoolVar(&flagCheck, "check", false, "仅检查是否有新版本")
	cmd.Flags().BoolVar(&flagList, "list", false, "列出所有可用版本")
	cmd.Flags().StringVar(&flagVersion, "version", "", "升级到指定版本")
	cmd.Flags().BoolVar(&flagRollback, "rollback", false, "回滚到上一版本")
	cmd.Flags().BoolVar(&flagForce, "force", false, "强制重新安装当前版本")
	cmd.Flags().BoolVar(&flagSkipSkills, "skip-skills", false, "跳过技能包更新")
	cmd.Flags().BoolVar(&flagSkipVerify, "skip-verify", false, "跳过校验（危险）")

	return cmd
}

type upgradeOptions struct {
	targetVersion string
	force         bool
	skipSkills    bool
	skipVerify    bool
	yes           bool
}

// --- dws upgrade --check ---

func runUpgradeCheck() error {
	fmt.Println("检查更新...")

	client := upgrade.NewClient()
	latest, err := client.FetchLatestRelease()
	if err != nil {
		return fmt.Errorf("检查更新失败: %w", err)
	}

	currentVer := version
	if !upgrade.NeedsUpgrade(currentVer, latest.Version) {
		fmt.Printf("已是最新版本 %s\n", currentVer)
		return nil
	}

	fmt.Printf("发现新版本: %s → v%s\n", currentVer, latest.Version)
	if latest.Date != "" {
		fmt.Printf("  发布日期: %s\n", latest.Date)
	}
	if latest.Prerelease {
		fmt.Printf("  通道: pre-release\n")
	}
	if latest.Changelog != "" {
		fmt.Printf("  更新内容: %s\n", truncateChangelog(latest.Changelog))
	}
	fmt.Println()
	fmt.Println("运行 dws upgrade 进行升级")
	return nil
}

// --- dws upgrade --list ---

func runUpgradeList() error {
	fmt.Println("获取版本列表...")

	client := upgrade.NewClient()
	versions, err := client.FetchAllReleases()
	if err != nil {
		return fmt.Errorf("获取版本列表失败: %w", err)
	}

	if len(versions) == 0 {
		fmt.Println("未找到任何版本")
		return nil
	}

	fmt.Println()
	fmt.Printf("  %-12s %-12s %-12s %s\n", "VERSION", "DATE", "TYPE", "CHANGELOG")
	fmt.Printf("  %s\n", strings.Repeat("-", 70))

	currentVer := strings.TrimPrefix(version, "v")
	for _, v := range versions {
		releaseType := "stable"
		if v.Prerelease {
			releaseType = "pre-release"
		}
		marker := ""
		if v.Version == currentVer {
			marker = " (已安装)"
		}
		changelog := v.Changelog
		if changelog == "" {
			changelog = "-"
		}
		fmt.Printf("  %-12s %-12s %-12s %s%s\n", v.Version, v.Date, releaseType, changelog, marker)
	}

	fmt.Println()
	fmt.Printf("已安装: %s\n", version)
	fmt.Println("提示: 使用 dws upgrade --version <version> 安装指定版本")
	return nil
}

// --- dws upgrade --rollback ---

func runUpgradeRollback(yes bool) error {
	rm := upgrade.NewRollbackManager()

	backups, err := rm.ListBackups()
	if err != nil {
		return fmt.Errorf("获取备份列表失败: %w", err)
	}
	if len(backups) == 0 {
		return fmt.Errorf("没有可用的备份，无法回滚")
	}

	fmt.Println("可用备份:")
	for i, b := range backups {
		if i >= 5 {
			fmt.Printf("  ... 还有 %d 个备份\n", len(backups)-5)
			break
		}
		fmt.Printf("  v%s (%s)\n", b.Version, b.CreatedAt.Format("2006-01-02 15:04"))
	}

	if !yes {
		fmt.Println()
		fmt.Print("是否回滚到最近的备份? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("已取消")
			return nil
		}
	}

	fmt.Print("回滚中...")
	if err := rm.Rollback(); err != nil {
		return fmt.Errorf("\n回滚失败: %w", err)
	}
	fmt.Println(" 完成")

	// Verify the rolled-back binary
	currentExe, _ := upgrade.CurrentBinaryPath()
	if currentExe != "" {
		if info, err := os.Stat(currentExe); err == nil && info.Size() > 0 && info.Mode()&0111 != 0 {
			fmt.Println("验证: 文件存在且可执行")
		}
	}

	fmt.Println()
	fmt.Printf("已回滚到 v%s\n", backups[0].Version)
	fmt.Println("运行 dws version 验证")
	return nil
}

// --- dws upgrade (full) ---

func runUpgrade(ctx context.Context, opts upgradeOptions) error {
	fmt.Println("检查更新...")

	if err := upgrade.EnsureUpgradeDirectories(); err != nil {
		return fmt.Errorf("初始化目录结构失败: %w", err)
	}

	client := upgrade.NewClient()
	var release *upgrade.ReleaseInfo
	var err error

	if opts.targetVersion != "" {
		fmt.Printf("指定版本: v%s\n", opts.targetVersion)
		release, err = client.FetchReleaseByTag(opts.targetVersion)
		if err != nil {
			return fmt.Errorf("获取版本 %s 信息失败: %w", opts.targetVersion, err)
		}
	} else {
		release, err = client.FetchLatestRelease()
		if err != nil {
			return fmt.Errorf("检查更新失败: %w", err)
		}
	}

	currentVer := version
	if !opts.force && !upgrade.NeedsUpgrade(currentVer, release.Version) {
		fmt.Printf("已是最新版本 %s\n", currentVer)
		return nil
	}

	// Show update info
	fmt.Printf("\n发现新版本: %s → v%s\n", currentVer, release.Version)
	if release.Date != "" {
		fmt.Printf("  发布日期: %s\n", release.Date)
	}
	if release.Prerelease {
		fmt.Println("  注意: 这是预发布版本")
	}

	// Confirm
	if !opts.yes {
		fmt.Println()
		fmt.Print("是否升级? [y/N] ")
		var answer string
		fmt.Scanln(&answer)
		if answer != "y" && answer != "Y" {
			fmt.Println("已取消")
			return nil
		}
	}

	// Find binary asset for current platform
	binaryAsset, err := upgrade.FindBinaryAsset(release.Assets)
	if err != nil {
		return err
	}
	fmt.Printf("平台: %s\n", binaryAsset.Name)

	// Create temp workspace
	tmpDir, err := os.MkdirTemp(upgrade.DownloadCacheDir(), "upgrade-*")
	if err != nil {
		tmpDir, err = os.MkdirTemp("", "dws-upgrade-*")
		if err != nil {
			return fmt.Errorf("创建临时目录失败: %w", err)
		}
	}
	defer os.RemoveAll(tmpDir)

	// Backup current version
	fmt.Print("备份当前版本...")
	rm := upgrade.NewRollbackManager()
	backupPath, backupErr := rm.Backup(strings.TrimPrefix(currentVer, "v"))
	if backupErr != nil {
		fmt.Printf(" 警告: %v\n", backupErr)
	} else {
		fmt.Println(" 完成")
	}

	// Download checksums.txt for verification
	var checksumsContent string
	if !opts.skipVerify {
		checksumsAsset := upgrade.FindChecksumsAsset(release.Assets)
		if checksumsAsset != nil {
			checksumsPath := filepath.Join(tmpDir, "checksums.txt")
			if _, dlErr := upgrade.Download(checksumsAsset.BrowserDownloadURL, checksumsPath); dlErr == nil {
				if data, readErr := os.ReadFile(checksumsPath); readErr == nil {
					checksumsContent = string(data)
				}
			}
		}
	}

	// Download binary
	binaryArchivePath := filepath.Join(tmpDir, binaryAsset.Name)
	fmt.Print("下载二进制...")
	start := time.Now()
	n, err := upgrade.DownloadWithProgress(ctx, binaryAsset.BrowserDownloadURL, binaryArchivePath,
		func(percent float64, downloaded, total int64) {
			bar := progressBar(percent)
			fmt.Printf("\r下载二进制... [%s] %5.1f%%", bar, percent)
		})
	if err != nil {
		return fmt.Errorf("\n下载失败: %w", err)
	}
	elapsed := time.Since(start)
	fmt.Printf("\r下载二进制... 完成 (%.1fMB, %.1fs)\n", float64(n)/1024/1024, elapsed.Seconds())

	// Verify SHA256
	if !opts.skipVerify {
		fmt.Print("验证 SHA256...")
		verified := false
		// Priority 1: checksums.txt
		if checksumsContent != "" {
			if err := upgrade.VerifyFileFromChecksums(binaryArchivePath, binaryAsset.Name, checksumsContent); err == nil {
				verified = true
			}
		}
		// Priority 2: GitHub asset digest
		if !verified {
			if digest := upgrade.ExtractDigestSHA256(binaryAsset.Digest); digest != "" {
				if err := upgrade.VerifySHA256(binaryArchivePath, digest); err == nil {
					verified = true
				}
			}
		}
		if verified {
			fmt.Println(" 通过")
		} else {
			fmt.Println(" 跳过 (无可用校验信息)")
		}
	}

	// Extract archive
	fmt.Print("解压...")
	extractDir := filepath.Join(tmpDir, "extracted")
	if strings.HasSuffix(binaryAsset.Name, ".zip") {
		if err := upgrade.ExtractZip(binaryArchivePath, extractDir); err != nil {
			return fmt.Errorf("\n解压失败: %w", err)
		}
	} else {
		// tar.gz
		if err := extractTarGz(binaryArchivePath, extractDir); err != nil {
			return fmt.Errorf("\n解压失败: %w", err)
		}
	}

	binaryPath := upgrade.FindBinaryInDir(extractDir)
	if binaryPath == "" {
		return fmt.Errorf("在解压目录中未找到 dws 二进制文件")
	}
	fmt.Println(" 完成")

	// Validate new binary
	fmt.Print("验证新版本...")
	if err := validateNewBinary(binaryPath, release.Version); err != nil {
		return fmt.Errorf("\n验证失败: %w", err)
	}
	fmt.Println(" 通过")

	// Replace binary
	fmt.Print("替换二进制...")
	if err := upgrade.ReplaceSelf(binaryPath); err != nil {
		fmt.Printf("\n替换失败: %v\n", err)
		if backupPath != "" {
			fmt.Print("尝试回滚...")
			if rbErr := rm.Rollback(); rbErr != nil {
				fmt.Printf(" 回滚也失败了: %v\n", rbErr)
			} else {
				fmt.Println(" 已回滚")
			}
		}
		return err
	}
	fmt.Println(" 完成")

	// Upgrade skills
	if !opts.skipSkills {
		upgradeSkills(ctx, release, tmpDir, opts.skipVerify, checksumsContent)
	}

	// Cleanup old backups
	rm.Cleanup(5)

	fmt.Println()
	fmt.Printf("升级完成! %s → v%s\n", currentVer, release.Version)
	fmt.Println("运行 dws version 验证")
	if backupPath != "" {
		fmt.Println("如遇问题，运行 dws upgrade --rollback 回滚")
	}

	return nil
}

// upgradeSkills downloads and installs the skills pack.
func upgradeSkills(ctx context.Context, release *upgrade.ReleaseInfo, tmpDir string, skipVerify bool, checksumsContent string) {
	skillsAsset := upgrade.FindSkillsAsset(release.Assets)
	if skillsAsset == nil {
		return
	}

	fmt.Print("下载技能包...")
	skillsZipPath := filepath.Join(tmpDir, "dws-skills.zip")
	n, err := upgrade.Download(skillsAsset.BrowserDownloadURL, skillsZipPath)
	if err != nil {
		fmt.Printf(" 跳过 (%v)\n", err)
		return
	}
	fmt.Printf(" %.1fKB 完成\n", float64(n)/1024)

	// Verify skills SHA256
	if !skipVerify && checksumsContent != "" {
		if err := upgrade.VerifyFileFromChecksums(skillsZipPath, "dws-skills.zip", checksumsContent); err != nil {
			fmt.Printf("  技能包校验失败: %v (继续安装)\n", err)
		}
	}

	// Extract to temp
	extractDir := filepath.Join(tmpDir, "skills-extracted")
	os.MkdirAll(extractDir, 0755)
	if err := upgrade.ExtractZip(skillsZipPath, extractDir); err != nil {
		fmt.Printf("  技能包解压失败: %v (已跳过)\n", err)
		return
	}

	// Locate SKILL.md root
	skillSrc := upgrade.LocateSkillMD(extractDir)
	if skillSrc == "" {
		fmt.Println("  技能包中未找到 SKILL.md (已跳过)")
		return
	}

	// Install to all known locations
	fmt.Print("安装技能包...")
	updated, err := upgrade.UpgradeSkillLocations(skillSrc)
	if err != nil {
		fmt.Printf(" 失败: %v\n", err)
		return
	}
	fmt.Printf(" 完成 (%d 个位置)\n", len(updated))
	for _, dir := range updated {
		fmt.Printf("  → %s\n", shortenHome(dir))
	}
}

// validateNewBinary checks the downloaded binary is valid.
func validateNewBinary(binaryPath, expectedVersion string) error {
	info, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("文件不存在: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("文件为空")
	}
	if err := os.Chmod(binaryPath, 0755); err != nil {
		return fmt.Errorf("设置执行权限失败: %w", err)
	}

	// Try running the binary
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, binaryPath, "version").CombinedOutput()
	if err != nil {
		return fmt.Errorf("二进制无法执行: %w", err)
	}

	if !strings.Contains(string(out), expectedVersion) {
		// Not fatal, version format might differ
		fmt.Printf("\n  注意: 版本输出中未包含 %s", expectedVersion)
	}
	return nil
}

// extractTarGz extracts a .tar.gz file using the system tar command.
func extractTarGz(archivePath, destDir string) error {
	os.MkdirAll(destDir, 0755)
	cmd := exec.Command("tar", "xzf", archivePath, "-C", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("tar 解压失败: %v: %s", err, string(out))
	}
	return nil
}

func progressBar(percent float64) string {
	width := 20
	filled := int(percent / 100 * float64(width))
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

func truncateChangelog(s string) string {
	lines := strings.Split(s, "\n")
	var result []string
	for i, line := range lines {
		if i >= 3 {
			result = append(result, "...")
			break
		}
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "#") {
			result = append(result, line)
		}
	}
	if len(result) == 0 {
		if len(s) > 80 {
			return s[:77] + "..."
		}
		return s
	}
	return strings.Join(result, "; ")
}

func shortenHome(path string) string {
	homeDir, err := os.UserHomeDir()
	if err != nil {
		return path
	}
	if strings.HasPrefix(path, homeDir) {
		return "~" + path[len(homeDir):]
	}
	return path
}
