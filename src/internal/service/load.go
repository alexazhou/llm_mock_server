package service

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// Loaded 是一次完整配置加载的结果：server 配置、补齐后的默认值与只读剧本注册表。
type Loaded struct {
	// ConfigPath 是本次加载使用的 server.yaml 绝对路径。
	ConfigPath string
	Server     *model.ServerConfig
	Resolved   *model.ResolvedServer
	// ScriptsDir 是解析后的 scripts_dir 绝对路径。
	ScriptsDir string
	Registry   *model.Registry
}

// Load 严格加载 server.yaml 与其 scripts_dir 下的全部剧本。
// 任一配置错误都会终止加载：serve 不会在配置非法时开始监听。
func Load(configPath string) (*Loaded, error) {
	absConfig, err := filepath.Abs(configPath)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", configPath, err)
	}
	data, err := os.ReadFile(absConfig)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", absConfig, err)
	}

	var server model.ServerConfig
	if err := utils.DecodeStrict(data, absConfig, &server); err != nil {
		return nil, err
	}
	doc, err := utils.DecodeSingleDocument(data, absConfig)
	if err != nil {
		return nil, err
	}
	if err := NewNullRejecter(nil).Check(absConfig, doc, ""); err != nil {
		return nil, err
	}

	resolved, err := server.Resolve()
	if err != nil {
		return nil, fmt.Errorf("%s: %w", absConfig, err)
	}
	scriptsDir := resolved.ScriptsDir
	if !filepath.IsAbs(scriptsDir) {
		scriptsDir = filepath.Join(filepath.Dir(absConfig), scriptsDir)
	}

	registry, err := loadScripts(scriptsDir)
	if err != nil {
		return nil, err
	}
	return &Loaded{
		ConfigPath: absConfig,
		Server:     &server,
		Resolved:   resolved,
		ScriptsDir: scriptsDir,
		Registry:   registry,
	}, nil
}

// loadScripts 按字典序加载全部剧本并检查 id 唯一性。
func loadScripts(scriptsDir string) (*model.Registry, error) {
	files, err := ScanScriptFiles(scriptsDir)
	if err != nil {
		return nil, err
	}
	entries := make([]*model.Entry, 0, len(files))
	seen := make(map[string]*model.Entry, len(files))
	for _, rel := range files {
		absPath := filepath.Join(scriptsDir, filepath.FromSlash(rel))
		data, err := os.ReadFile(absPath)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", absPath, err)
		}
		doc, err := utils.DecodeSingleDocument(data, absPath)
		if err != nil {
			return nil, err
		}
		// 先做结构校验（字段集合、互斥关系），它能给出比通用未知字段更准确的诊断；
		// 再做严格解码，捕获未知字段与无效类型。
		if err := ValidateScriptNode(absPath, doc); err != nil {
			return nil, err
		}
		var script model.Script
		if err := utils.DecodeStrictInto(data, absPath, doc, &script); err != nil {
			return nil, err
		}
		if err := NewNullRejecter(&script).Check(absPath, doc, ""); err != nil {
			return nil, err
		}
		if err := ValidateScriptSemantics(absPath, rel, &script); err != nil {
			return nil, err
		}
		if prev, ok := seen[script.ID]; ok {
			return nil, fmt.Errorf("%s: duplicate script id %q (already defined in %s)", absPath, script.ID, prev.RelPath)
		}
		entry := &model.Entry{RelPath: rel, AbsPath: absPath, Script: &script}
		seen[script.ID] = entry
		entries = append(entries, entry)
	}
	return model.NewRegistry(entries), nil
}
