package service

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadFixture 读取 source=raw 的 fixture 文件。
//
// 规则（configuration_reference.md / output_steps_design.md §4.2.4）：
//   - 路径相对当前剧本 YAML 所在目录，禁止绝对路径与 `..` 逃逸；
//   - 解析目录与目标文件的符号链接后，目标仍须位于该目录的真实路径内；
//   - 必须是普通文件，且在加载期一次性读取并校验大小（运行期不再读盘）。
func LoadFixture(scriptDir, relPath string, maxBytes int64) ([]byte, error) {
	if relPath == "" {
		return nil, fmt.Errorf("fixture path must not be empty")
	}
	if filepath.IsAbs(relPath) {
		return nil, fmt.Errorf("fixture %q must be a relative path", relPath)
	}
	cleaned := filepath.Clean(filepath.FromSlash(relPath))
	if cleaned == ".." || strings.HasPrefix(cleaned, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("fixture %q must not escape the script directory", relPath)
	}

	realDir, err := filepath.EvalSymlinks(scriptDir)
	if err != nil {
		return nil, fmt.Errorf("cannot resolve script directory: %v", err)
	}
	target := filepath.Join(realDir, cleaned)
	realTarget, err := filepath.EvalSymlinks(target)
	if err != nil {
		return nil, fmt.Errorf("fixture %q cannot be resolved: %v", relPath, err)
	}
	if !isWithin(realDir, realTarget) {
		return nil, fmt.Errorf("fixture %q resolves outside the script directory", relPath)
	}
	info, err := os.Stat(realTarget)
	if err != nil {
		return nil, fmt.Errorf("fixture %q cannot be read: %v", relPath, err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("fixture %q must be a regular file", relPath)
	}
	if info.Size() > maxBytes {
		return nil, fmt.Errorf("fixture %q is %d bytes, exceeding max_fixture_bytes %d", relPath, info.Size(), maxBytes)
	}
	data, err := os.ReadFile(realTarget)
	if err != nil {
		return nil, fmt.Errorf("fixture %q cannot be read: %v", relPath, err)
	}
	if int64(len(data)) > maxBytes {
		return nil, fmt.Errorf("fixture %q is %d bytes, exceeding max_fixture_bytes %d", relPath, len(data), maxBytes)
	}
	return data, nil
}

// isWithin 判断 target 是否位于 dir 之内（含 dir 自身）。
func isWithin(dir, target string) bool {
	rel, err := filepath.Rel(dir, target)
	if err != nil {
		return false
	}
	if rel == "." {
		return true
	}
	return !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && rel != ".."
}
