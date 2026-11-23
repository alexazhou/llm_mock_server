// Package utils 提供与业务无关的通用能力：严格 YAML 解码、节点位置错误和
// 错误信息规范化。这里不允许依赖 model / service 等上层包。
package utils

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// PositionError 表示一条带文件与配置节点位置的配置错误。
// Path 是配置树中的节点路径（例如 rounds[0].outputs[1]），可为空。
type PositionError struct {
	File   string
	Line   int
	Column int
	Path   string
	Msg    string
}

func (e *PositionError) Error() string {
	loc := e.File
	if loc == "" {
		loc = "<config>"
	}
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, e.Line)
		if e.Column > 0 {
			loc = fmt.Sprintf("%s:%d", loc, e.Column)
		}
	}
	if e.Path != "" {
		return fmt.Sprintf("%s: %s: %s", loc, e.Path, e.Msg)
	}
	return fmt.Sprintf("%s: %s", loc, e.Msg)
}

// NewPositionError 构造一条带位置的配置错误；n 为 nil 时省略行列。
func NewPositionError(file string, n *yaml.Node, path, msg string) *PositionError {
	err := &PositionError{File: file, Path: path, Msg: msg}
	if n != nil {
		err.Line = n.Line
		err.Column = n.Column
	}
	return err
}

var linePattern = regexp.MustCompile(`line (\d+):`)
var unknownFieldPattern = regexp.MustCompile(`field (\S+) not found in type \S+`)
var cannotUnmarshalPattern = regexp.MustCompile(`cannot unmarshal (!!\w+|\[)`)
var goTypePattern = regexp.MustCompile(`\b(?:model|service|utils|constants)\.[A-Za-z0-9_]+`)

// WrapYAMLError 把 yaml 库返回的错误统一转换成带文件与行号的 PositionError，
// 并去掉对外部无意义的 Go 类型名。
func WrapYAMLError(file string, err error) error {
	if err == nil {
		return nil
	}
	msg := err.Error()
	line := 0
	if m := linePattern.FindStringSubmatch(msg); m != nil {
		if v, convErr := strconv.Atoi(m[1]); convErr == nil {
			line = v
		}
	}
	msg = strings.TrimPrefix(msg, "yaml: ")
	msg = strings.TrimPrefix(msg, "unmarshal errors:\n")
	msg = unknownFieldPattern.ReplaceAllString(msg, `unknown field "$1"`)
	msg = goTypePattern.ReplaceAllString(msg, "config value")
	msg = strings.Join(strings.Fields(msg), " ")
	msg = strings.TrimSpace(strings.Trim(msg, "-"))
	return &PositionError{File: file, Line: line, Msg: msg}
}
