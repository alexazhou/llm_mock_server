package utils

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// allowedTags 是 DSL 允许出现的 YAML tag 集合。除此之外的 tag（包括自定义
// tag、!!binary、!!timestamp 等）都属于配置错误，必须拒绝而不是静默转换。
var allowedTags = map[string]bool{
	"!!null":  true,
	"!!bool":  true,
	"!!int":   true,
	"!!float": true,
	"!!str":   true,
	"!!seq":   true,
	"!!map":   true,
}

// DecodeStrict 严格解码单文档 YAML：
//   - 拒绝空文件与多文档文件；
//   - 拒绝 anchor / alias / merge key / 自定义 tag；
//   - 拒绝同一个 mapping 内的重复 key；
//   - 以 KnownFields 模式解码，未知字段与无效类型直接报错。
//
// 所有错误都转换为带文件与行号的 PositionError。
func DecodeStrict(data []byte, file string, out any) error {
	if len(bytes.TrimSpace(data)) == 0 {
		return &PositionError{File: file, Line: 1, Column: 1, Msg: "empty YAML file is not a valid config"}
	}

	doc, err := DecodeSingleDocument(data, file)
	if err != nil {
		return err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return NewPositionError(file, doc, "", "empty YAML document is not a valid config")
	}

	return DecodeStrictInto(data, file, doc, out)
}

// DecodeStrictInto 在已解析的文档上以 KnownFields 模式解码到 out。
// 调用方已经通过 DecodeSingleDocument 完成结构检查时使用，避免重复解析。
func DecodeStrictInto(data []byte, file string, doc *yaml.Node, out any) error {
	if doc == nil || doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return &PositionError{File: file, Line: 1, Column: 1, Msg: "empty YAML document is not a valid config"}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return WrapYAMLError(file, err)
	}
	return nil
}

// DecodeNodeStrict 严格解码一个已经解析好的节点（KnownFields 模式），
// 用于自定义 UnmarshalYAML 内部需要完整结构校验的场景。
func DecodeNodeStrict(n *yaml.Node, out any) error {
	buf := &bytes.Buffer{}
	enc := yaml.NewEncoder(buf)
	enc.SetIndent(2)
	if err := enc.Encode(n); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	dec := yaml.NewDecoder(bytes.NewReader(buf.Bytes()))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil {
		return err
	}
	return nil
}

// DecodeSingleDocument 解析首个 YAML 文档，确认文件里没有第二个文档，
// 并对整棵节点树执行严格结构检查。
func DecodeSingleDocument(data []byte, file string) (*yaml.Node, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return nil, &PositionError{File: file, Line: 1, Column: 1, Msg: "empty YAML file is not a valid config"}
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, &PositionError{File: file, Line: 1, Column: 1, Msg: "empty YAML file is not a valid config"}
		}
		return nil, WrapYAMLError(file, err)
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); err == nil {
		return nil, NewPositionError(file, &extra, "",
			"file contains more than one YAML document; each file must define exactly one script")
	} else if !errors.Is(err, io.EOF) {
		return nil, WrapYAMLError(file, err)
	}
	if err := CheckNode(file, &doc, ""); err != nil {
		return nil, err
	}
	return &doc, nil
}

// CheckNode 递归检查节点树，拒绝 anchor、alias、merge key 与自定义 tag。
func CheckNode(file string, n *yaml.Node, path string) error {
	if n == nil {
		return nil
	}
	if n.Kind == yaml.AliasNode {
		return NewPositionError(file, n, path,
			fmt.Sprintf("YAML alias (*%s) is not supported; scripts must be self-contained", n.Value))
	}
	if n.Anchor != "" {
		return NewPositionError(file, n, path,
			fmt.Sprintf("YAML anchor (&%s) is not supported; scripts must be self-contained", n.Anchor))
	}
	if n.Tag != "" && !allowedTags[n.Tag] {
		return NewPositionError(file, n, path,
			fmt.Sprintf("custom YAML tag %q is not supported", n.Tag))
	}

	switch n.Kind {
	case yaml.MappingNode:
		seen := make(map[string]*yaml.Node, len(n.Content)/2)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key, val := n.Content[i], n.Content[i+1]
			if key.Value == "<<" || key.Tag == "!!merge" {
				return NewPositionError(file, key, path, "YAML merge key (<<) is not supported")
			}
			if key.Kind != yaml.ScalarNode {
				return NewPositionError(file, key, path, "non-scalar mapping keys are not supported")
			}
			childPath := JoinPath(path, key.Value)
			if prev, ok := seen[key.Value]; ok {
				return NewPositionError(file, key, childPath,
					fmt.Sprintf("duplicate key %q (first defined at line %d)", key.Value, prev.Line))
			}
			seen[key.Value] = key
			if err := CheckNode(file, val, childPath); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for i, item := range n.Content {
			if err := CheckNode(file, item, fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case yaml.DocumentNode:
		for _, item := range n.Content {
			if err := CheckNode(file, item, path); err != nil {
				return err
			}
		}
	}
	return nil
}

// JoinPath 拼接配置节点路径，用于错误诊断。
func JoinPath(parent, key string) string {
	if parent == "" {
		return key
	}
	return parent + "." + key
}
