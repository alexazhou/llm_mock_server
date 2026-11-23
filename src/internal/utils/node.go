package utils

import "gopkg.in/yaml.v3"

// Root 返回 YAML 文档的根节点（跳过 DocumentNode）。
func Root(doc *yaml.Node) *yaml.Node {
	if doc == nil {
		return nil
	}
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		return doc.Content[0]
	}
	return doc
}

// Field 返回 mapping 节点中指定 key 的值节点。
func Field(n *yaml.Node, key string) (*yaml.Node, bool) {
	if n == nil || n.Kind != yaml.MappingNode {
		return nil, false
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if n.Content[i].Value == key {
			return n.Content[i+1], true
		}
	}
	return nil, false
}

// Item 返回 sequence 节点的第 i 项，越界返回 nil。
func Item(n *yaml.Node, i int) *yaml.Node {
	if n == nil || n.Kind != yaml.SequenceNode || i < 0 || i >= len(n.Content) {
		return nil
	}
	return n.Content[i]
}

// Lookup 沿节点树按路径查找：string 段按 mapping key，int 段按 sequence 下标。
// 任一层缺失返回 nil，调用方可据此回退到父节点位置。
func Lookup(n *yaml.Node, path ...any) *yaml.Node {
	current := n
	for _, segment := range path {
		if current == nil {
			return nil
		}
		switch v := segment.(type) {
		case string:
			next, ok := Field(current, v)
			if !ok {
				return nil
			}
			current = next
		case int:
			current = Item(current, v)
		default:
			return nil
		}
	}
	return current
}
