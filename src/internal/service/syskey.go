package service

import (
	"fmt"
	"strings"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// 第一版系统 key 的只读集合（配置参考 §3）。剧本不能创建、修改或覆盖。
const (
	KeyRequestMethod      = "request.method"
	KeyRequestPath        = "request.path"
	KeyRequestHeader      = "request.header"
	KeyRequestHeaders     = "request.headers"
	KeyRequestQuery       = "request.query"
	KeyRequestBody        = "request.body"
	KeyRequestJSONBody    = "request.json_body"
	KeyRequestRoundInput  = "request.round_input"
	KeyRequestModel       = "request.model"
	KeyRequestStream      = "request.stream"
	KeyRequestOutputCount = "request.output_count"
	KeyRequestCount       = "request.count"
	KeyScriptID           = "script.id"
	KeyRequestProtocol    = "request.protocol"
	KeyRoundIndex         = "round.index"
)

// SystemKey 是编译后的系统 key 读取器：语法与 key 参数在加载时校验，
// 运行期只做只读读取。
type SystemKey struct {
	Raw   string
	Name  string
	Field string
	Mode  string
	Path  *utils.JSONPath
}

// CompileSystemKey 校验并编译一个系统 key 引用。
// allowCount=false 用于 assert / input.assert：这些位置禁止引用 request.count。
func CompileSystemKey(raw, key, field, mode, path string, allowCount bool) (*SystemKey, error) {
	if key == "" {
		return nil, fmt.Errorf("%s: key is required", raw)
	}
	sk := &SystemKey{Raw: raw, Name: key, Field: field, Mode: mode}
	switch key {
	case KeyRequestMethod, KeyRequestPath, KeyRequestBody, KeyRequestModel,
		KeyRequestStream, KeyRequestOutputCount, KeyScriptID, KeyRequestProtocol, KeyRoundIndex:
		if field != "" || mode != "" || path != "" {
			return nil, fmt.Errorf("%s: %s does not accept field/mode/path arguments", raw, key)
		}
	case KeyRequestCount:
		if !allowCount {
			return nil, fmt.Errorf("%s: %s is only available after input assertions pass; it cannot be used in assert or input.assert", raw, key)
		}
		if field != "" || mode != "" || path != "" {
			return nil, fmt.Errorf("%s: %s does not accept field/mode/path arguments", raw, key)
		}
	case KeyRequestHeaders:
		if field != "" || mode != "" || path != "" {
			return nil, fmt.Errorf("%s: %s does not accept field/mode/path arguments", raw, key)
		}
	case KeyRequestHeader, KeyRequestQuery:
		if field == "" {
			return nil, fmt.Errorf("%s: %s requires a field argument", raw, key)
		}
		if mode == "" {
			sk.Mode = "single"
		}
		if sk.Mode != "single" && sk.Mode != "values" {
			return nil, fmt.Errorf("%s: mode must be single or values, got %q", raw, mode)
		}
		if path != "" {
			return nil, fmt.Errorf("%s: %s does not accept a path argument", raw, key)
		}
	case KeyRequestJSONBody, KeyRequestRoundInput:
		if field != "" || mode != "" {
			return nil, fmt.Errorf("%s: %s does not accept field/mode arguments", raw, key)
		}
	default:
		return nil, fmt.Errorf("%s: unknown system key %q", raw, key)
	}
	if path != "" {
		compiled, err := utils.CompileJSONPath(path)
		if err != nil {
			return nil, fmt.Errorf("%s: %v", raw, err)
		}
		sk.Path = compiled
	}
	return sk, nil
}

// String 返回用于诊断的 key 描述：key 名，必要时附带 key 参数。
func (k *SystemKey) String() string {
	if k == nil {
		return ""
	}
	if k.Field != "" {
		return k.Name + "(" + k.Field + ")"
	}
	if k.Path != nil && len(k.Path.Raw()) > 0 {
		return k.Name + "(" + k.Path.Raw() + ")"
	}
	return k.Name
}

// Read 读取 key 的当前值。found=false 表示内部 missing（与 JSON null 严格区分）；
// err 表示 key 读取失败（例如 header 出现多值但 mode=single）。
func (k *SystemKey) Read(ctx *model.RequestContext) (value any, found bool, err error) {
	switch k.Name {
	case KeyRequestMethod:
		return ctx.Method, true, nil
	case KeyRequestPath:
		return ctx.Path, true, nil
	case KeyRequestBody:
		return ctx.BodyText, true, nil
	case KeyRequestHeaders:
		out := make(map[string]any, len(ctx.Header))
		for name, values := range ctx.Header {
			items := make([]any, len(values))
			for i, v := range values {
				items[i] = v
			}
			out[strings.ToLower(name)] = items
		}
		return out, true, nil
	case KeyRequestHeader:
		return readMultiValue("header", ctx.Header.Values(k.Field), k.Mode)
	case KeyRequestQuery:
		return readMultiValue("query", ctx.Query[k.Field], k.Mode)
	case KeyRequestModel:
		return ctx.Model, true, nil
	case KeyRequestStream:
		return ctx.Stream, true, nil
	case KeyRequestOutputCount:
		return utils.Number(fmt.Sprintf("%d", ctx.OutputCount)), true, nil
	case KeyRequestCount:
		if ctx.Count == nil {
			return nil, false, nil
		}
		return utils.Number(fmt.Sprintf("%d", *ctx.Count)), true, nil
	case KeyScriptID:
		if ctx.ScriptID == "" {
			return nil, false, nil
		}
		return ctx.ScriptID, true, nil
	case KeyRequestProtocol:
		return ctx.Protocol, true, nil
	case KeyRoundIndex:
		if ctx.RoundIndex == 0 {
			return nil, false, nil
		}
		return utils.Number(fmt.Sprintf("%d", ctx.RoundIndex)), true, nil
	case KeyRequestJSONBody:
		return k.evalPath(ctx.JSONBody, true)
	case KeyRequestRoundInput:
		return k.evalPath(ctx.RoundInput, ctx.RoundInput != nil)
	}
	return nil, false, fmt.Errorf("unknown system key %q", k.Name)
}

func (k *SystemKey) evalPath(root any, present bool) (any, bool, error) {
	if !present {
		return nil, false, nil
	}
	if k.Path == nil {
		return root, true, nil
	}
	value, found := k.Path.Eval(root)
	if !found {
		return nil, false, nil
	}
	return value, true, nil
}

func readMultiValue(kind string, values []string, mode string) (any, bool, error) {
	if len(values) == 0 {
		return nil, false, nil
	}
	if mode == "values" {
		items := make([]any, len(values))
		for i, v := range values {
			items[i] = v
		}
		return items, true, nil
	}
	if len(values) > 1 {
		return nil, false, fmt.Errorf("%s has %d values but mode is single", kind, len(values))
	}
	return values[0], true, nil
}
