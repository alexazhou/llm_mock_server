package service

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"

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

	doc, err := utils.DecodeSingleDocument(data, absConfig)
	if err != nil {
		return nil, err
	}
	if err := ValidateServerNode(absConfig, doc); err != nil {
		return nil, err
	}
	var server model.ServerConfig
	if err := utils.DecodeStrictInto(data, absConfig, doc, &server); err != nil {
		return nil, err
	}
	if err := NewNullRejecter(nil).Check(absConfig, doc, ""); err != nil {
		return nil, err
	}

	resolved, err := server.Resolve()
	if err != nil {
		return nil, locateFieldError(absConfig, utils.Root(doc), "", err)
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

// ValidateServerNode 在原始节点上校验 server.yaml 的必填项与标量类型，
// 保证「listen 写成整数」这类错误也能给出文件、行列号与字段名。
func ValidateServerNode(file string, doc *yaml.Node) error {
	root := utils.Root(doc)
	if root == nil || root.Kind != yaml.MappingNode {
		return utils.NewPositionError(file, root, "", "server.yaml root must be a mapping")
	}
	versionNode, ok := utils.Field(root, "version")
	if !ok {
		return utils.NewPositionError(file, root, "version", "version is required")
	}
	if versionNode.Tag != "!!int" {
		return utils.NewPositionError(file, versionNode, "version", "version must be an integer (1)")
	}
	for _, field := range []string{"listen", "scripts_dir"} {
		if n, ok := utils.Field(root, field); ok && n.Tag != "!!str" {
			return utils.NewPositionError(file, n, field, fmt.Sprintf("%s must be a string", field))
		}
	}
	for _, section := range []string{"config", "transport", "runtime", "capture"} {
		n, ok := utils.Field(root, section)
		if !ok {
			continue
		}
		if n.Tag == "!!null" {
			return utils.NewPositionError(file, n, section, fmt.Sprintf("field %q must not be null", section))
		}
		if n.Kind != yaml.MappingNode {
			return utils.NewPositionError(file, n, section, fmt.Sprintf("%s must be a mapping", section))
		}
	}
	intFields := []struct{ section, key string }{
		{"runtime", "max_active_requests"},
		{"runtime", "max_output_bytes"},
		{"runtime", "max_fixture_bytes"},
		{"runtime", "max_generated_events"},
		{"runtime", "max_write_units"},
		{"capture", "capacity"},
		{"capture", "max_body_bytes"},
		{"capture", "max_total_bytes"},
	}
	for _, field := range intFields {
		if n := utils.Lookup(root, field.section, field.key); n != nil && n.Tag != "!!int" {
			return utils.NewPositionError(file, n, field.section+"."+field.key,
				fmt.Sprintf("%s.%s must be an integer", field.section, field.key))
		}
	}
	durationFields := []struct{ section, key string }{
		{"transport", "read_header_timeout"},
		{"runtime", "shutdown_timeout"},
	}
	for _, field := range durationFields {
		if n := utils.Lookup(root, field.section, field.key); n != nil && n.Tag != "!!str" {
			return utils.NewPositionError(file, n, field.section+"."+field.key,
				fmt.Sprintf("%s.%s must be a duration string such as 5s", field.section, field.key))
		}
	}
	return nil
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
		if err := ValidateScriptSemantics(absPath, doc, &script); err != nil {
			return nil, err
		}
		if prev, ok := seen[script.ID]; ok {
			return nil, DuplicateIDError(absPath, doc, script.ID, prev.RelPath)
		}
		entry := &model.Entry{RelPath: rel, AbsPath: absPath, Script: &script}
		seen[script.ID] = entry
		entries = append(entries, entry)
	}
	return model.NewRegistry(entries), nil
}
