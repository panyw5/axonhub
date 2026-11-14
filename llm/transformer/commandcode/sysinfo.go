// 职责: 采集运行环境信息，构造与 commandcode CLI buildServerConfig 一致的请求 config 字段。
// 关联文件:
//
//	llm/transformer/commandcode/outbound.go
//	llm/transformer/commandcode/fingerprint.go
//
// 注意事项:
//
//	字段取值必须复刻官方 CLI 的格式：environment 是 process.platform 的值
//	（darwin/linux/win32，不带 arch），git 字段在非 git 目录全为空字符串。
package commandcode

import (
	"context"
	"crypto/rand"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// structureExcludedNames mirrors the CLI's readdir filter for config.structure.
var structureExcludedNames = map[string]struct{}{
	"node_modules": {}, "dist": {}, "build": {}, ".git": {}, ".svn": {}, ".hg": {},
	"coverage": {}, ".nyc_output": {}, ".cache": {}, "tmp": {}, "temp": {},
	".next": {}, ".nuxt": {}, "out": {},
}

// shellOutput runs a command and returns trimmed stdout, or "" on any failure,
// matching the CLI's shellOutput helper.
func shellOutput(ctx context.Context, dir string, args ...string) string {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// nodePlatform maps GOOS to the process.platform values the CLI reports.
func nodePlatform() string {
	switch runtime.GOOS {
	case "windows":
		return "win32"
	case "darwin", "linux", "freebsd", "openbsd":
		return runtime.GOOS
	default:
		return runtime.GOOS
	}
}

// nodeArch maps GOARCH to the process.arch values the CLI reports.
func nodeArch() string {
	switch runtime.GOARCH {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	case "arm64", "arm":
		return runtime.GOARCH
	default:
		return runtime.GOARCH
	}
}

// readStructure lists top-level, non-hidden entries of dir, filtered the same
// way the CLI filters its workspace listing. os.ReadDir output is already
// sorted by name, matching the CLI's .sort().
func readStructure(dir string) []any {
	structure := []any{}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return structure
	}
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if _, excluded := structureExcludedNames[name]; excluded {
			continue
		}
		structure = append(structure, name)
	}
	return structure
}

// resolveMainBranch mirrors the CLI: prefer origin/HEAD, fall back to scanning
// remote branches for main/master, then "main".
func resolveMainBranch(ctx context.Context, dir string) string {
	if ref := shellOutput(ctx, dir, "git", "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); ref != "" {
		return strings.TrimPrefix(ref, "origin/")
	}
	branches := shellOutput(ctx, dir, "git", "branch", "-r")
	switch {
	case strings.Contains(branches, "origin/main"):
		return "main"
	case strings.Contains(branches, "origin/master"):
		return "master"
	default:
		return "main"
	}
}

// collectServerConfig mirrors the CLI's buildServerConfig: real cwd, platform,
// directory listing, and git metadata when the cwd is inside a repository.
func collectServerConfig(ctx context.Context, dir string) requestConfig {
	config := requestConfig{
		WorkingDir:    dir,
		Date:          time.Now().UTC().Format(time.DateOnly),
		Environment:   nodePlatform(),
		Structure:     readStructure(dir),
		RecentCommits: []any{},
	}
	if shellOutput(ctx, dir, "git", "rev-parse", "--git-dir") == "" {
		return config
	}
	config.IsGitRepo = true
	config.CurrentBranch = shellOutput(ctx, dir, "git", "branch", "--show-current")
	config.MainBranch = resolveMainBranch(ctx, dir)
	if status := shellOutput(ctx, dir, "git", "status", "--porcelain"); status != "" {
		config.GitStatus = status
	} else {
		config.GitStatus = "Working tree clean"
	}
	if log := shellOutput(ctx, dir, "git", "log", "--oneline", "-3"); log != "" {
		for _, line := range strings.Split(log, "\n") {
			config.RecentCommits = append(config.RecentCommits, line)
		}
	}
	return config
}

// projectSlug derives a kebab-case slug from the working directory basename,
// matching the CLI's project slug header.
func projectSlug(dir string) string {
	base := filepath.Base(dir)
	var slug strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(base) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			slug.WriteRune(r)
			lastDash = false
		} else if !lastDash {
			slug.WriteByte('-')
			lastDash = true
		}
	}
	name := strings.Trim(slug.String(), "-")
	if name == "" {
		name = "project"
	}
	return name
}

// newTraceparent builds the W3C traceparent header the CLI emits on every
// generate request: 00-<32 hex trace id>-<16 hex span id>-01.
func newTraceparent() string {
	var traceID [16]byte
	var spanID [8]byte
	_, _ = rand.Read(traceID[:])
	_, _ = rand.Read(spanID[:])
	return fmt.Sprintf("00-%x-%x-01", traceID, spanID)
}
