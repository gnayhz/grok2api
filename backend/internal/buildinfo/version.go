package buildinfo

import (
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
)

// Version 可在发布构建时通过 -ldflags -X 注入。
var Version string

// Commit 可在发布构建时通过 -ldflags -X 注入（如 git rev-parse --short=12
// HEAD）。背景：2026-08-27 线上质量守卫事故里，仓库已合并修复但线上容器
// 未滚动部署，管理端却没有任何字段能区分"运行的到底是哪个提交"，只能靠
// 审计行为特征反推。把 commit 暴露成一等字段，让"部署滞后"一眼可见。
var Commit string

// CurrentVersion 返回当前运行实例的版本。源码运行优先读取仓库 VERSION，
// 容器和发行包可将 VERSION 放在可执行文件同目录。
func CurrentVersion() string {
	if value := cleanVersion(Version); value != "" {
		return value
	}
	if value := cleanVersion(os.Getenv("GROK2API_VERSION")); value != "" {
		return value
	}
	candidates := []string{"VERSION", filepath.Join("..", "VERSION")}
	if executable, err := os.Executable(); err == nil {
		candidates = append(candidates, filepath.Join(filepath.Dir(executable), "VERSION"))
	}
	for _, candidate := range candidates {
		if data, err := os.ReadFile(candidate); err == nil {
			if value := cleanVersion(string(data)); value != "" {
				return value
			}
		}
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		return info.Main.Version
	}
	return "dev"
}

// CurrentCommit 返回当前运行实例的构建提交。回退链：ldflags 注入 >
// GROK2API_COMMIT 环境变量 > go build 内嵌的 VCS revision（截短 12 位）
// > "unknown"。
func CurrentCommit() string {
	if value := cleanCommit(Commit); value != "" {
		return value
	}
	if value := cleanCommit(os.Getenv("GROK2API_COMMIT")); value != "" {
		return value
	}
	if info, ok := debug.ReadBuildInfo(); ok {
		for _, setting := range info.Settings {
			if setting.Key != "vcs.revision" {
				continue
			}
			if value := cleanCommit(setting.Value); value != "" {
				if len(value) > 12 {
					value = value[:12]
				}
				return value
			}
		}
	}
	return "unknown"
}

// cleanCommit 与 cleanVersion 同语义：拒绝超长与控制字符，防止注入的
// 提交串经管理端回显成攻击面。
func cleanCommit(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 64 || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	return value
}

func cleanVersion(value string) string {
	value = strings.TrimSpace(value)
	if len(value) > 128 || strings.IndexFunc(value, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0 {
		return ""
	}
	return value
}
