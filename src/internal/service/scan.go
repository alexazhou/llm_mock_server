package service

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ScanScriptFiles 按相对路径字典序递归扫描 scripts_dir 中的 .yaml/.yml 普通文件。
// 符号链接（文件和目录）一律跳过；其他扩展名不参与扫描，只能被 raw.fixture 引用。
func ScanScriptFiles(scriptsDir string) ([]string, error) {
	info, err := os.Stat(scriptsDir)
	if err != nil {
		return nil, fmt.Errorf("scripts_dir %q: %w", scriptsDir, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("scripts_dir %q is not a directory", scriptsDir)
	}

	var files []string
	err = filepath.WalkDir(scriptsDir, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.Type()&fs.ModeSymlink != 0 {
			// 不跟随符号链接：既不进入链接目录，也不加载链接文件。
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext != ".yaml" && ext != ".yml" {
			return nil
		}
		rel, err := filepath.Rel(scriptsDir, path)
		if err != nil {
			return err
		}
		files = append(files, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("scan scripts_dir %q: %w", scriptsDir, err)
	}
	sort.Strings(files)
	return files, nil
}
