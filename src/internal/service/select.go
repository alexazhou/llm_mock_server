package service

import (
	"fmt"

	"github.com/alexazhou/llm_mock_server/internal/model"
	"github.com/alexazhou/llm_mock_server/internal/utils"
)

// CandidateResult 是一个 outputs 候选的求值结果（含每个条件的结果）。
type CandidateResult struct {
	Index      int
	Fallback   bool
	Matched    bool
	Conditions []AssertionResult
}

// LocateRound 用 rounds[output_count] 定位唯一 round：不存游标、不按调用次数推进、
// 越界不循环也不回退。
func LocateRound(script *CompiledScript, outputCount int) (*CompiledRound, *model.FrameworkError) {
	if outputCount < 0 || outputCount >= len(script.Rounds) {
		ferr := model.NewFrameworkError(model.CodeRoundNotFound,
			fmt.Sprintf("no round for output_count %d (script has %d rounds)", outputCount, len(script.Rounds)))
		ferr.WithDetails("output_count", outputCount)
		ferr.WithDetails("round_count", len(script.Rounds))
		return nil, ferr
	}
	return &script.Rounds[outputCount], nil
}

// MatchOutput 按 YAML 顺序匹配 outputs：条件列表 AND、取首个匹配项；
// 没有 when 的候选是 fallback。返回已求值的候选结果用于诊断。
func MatchOutput(ctx *model.RequestContext, round *CompiledRound) (*CompiledOutput, []CandidateResult, *model.FrameworkError) {
	results := make([]CandidateResult, 0, len(round.Outputs))
	for i := range round.Outputs {
		candidate := &round.Outputs[i]
		result := CandidateResult{Index: candidate.Index, Fallback: candidate.Fallback,
			Conditions: make([]AssertionResult, 0, len(candidate.When))}
		matched := true
		for _, condition := range candidate.When {
			conditionResult := condition.Evaluate(ctx)
			result.Conditions = append(result.Conditions, conditionResult)
			if conditionResult.EvaluatorError {
				results = append(results, result)
				ferr := model.NewFrameworkError(model.CodeOutputConditionError,
					"output condition evaluation failed")
				ferr.WithDetails("candidates", CandidatesDetails(results))
				return nil, results, ferr
			}
			if !conditionResult.Matched {
				matched = false
				break
			}
		}
		result.Matched = matched
		results = append(results, result)
		if matched {
			return candidate, results, nil
		}
	}
	ferr := model.NewFrameworkError(model.CodeOutputNotFound, "no output candidate matched the request")
	ferr.WithDetails("candidates", CandidatesDetails(results))
	return nil, results, ferr
}

// BaseDetails 汇总 B 阶段错误的公共诊断字段。request_count 只在已分配后出现。
func BaseDetails(ctx *model.RequestContext) map[string]any {
	details := map[string]any{
		"script_id":    ctx.ScriptID,
		"protocol":     ctx.Protocol,
		"output_count": ctx.OutputCount,
	}
	if ctx.RoundIndex > 0 {
		details["round_index"] = ctx.RoundIndex
	}
	if ctx.Count != nil {
		details["request_count"] = *ctx.Count
	}
	return details
}

// AssertionDetails 把一条断言/条件结果转成稳定的 JSON 结构（任意值走 4096 字节预览）。
func AssertionDetails(result AssertionResult) map[string]any {
	out := map[string]any{
		"index":   result.Index,
		"scope":   result.Scope,
		"key":     result.Key,
		"op":      result.Op,
		"matched": result.Matched,
	}
	if result.Name != "" {
		out["name"] = result.Name
	}
	if result.Reason != "" {
		out["reason"] = result.Reason
	}
	// actual 始终出现：内部 missing 用 null 表示，便于验收区分「缺失」与「值为 null」。
	out["actual"] = utils.Preview(result.Actual)
	if result.HasExpected {
		out["expected"] = utils.Preview(result.Expected)
	}
	if result.DefaultApplied {
		out["default_applied"] = true
	}
	return out
}

// CandidatesDetails 把候选求值结果转成 JSON 结构。
func CandidatesDetails(results []CandidateResult) []any {
	out := make([]any, 0, len(results))
	for _, candidate := range results {
		item := map[string]any{
			"index":    candidate.Index,
			"fallback": candidate.Fallback,
			"matched":  candidate.Matched,
		}
		if len(candidate.Conditions) > 0 {
			conditions := make([]any, 0, len(candidate.Conditions))
			for _, condition := range candidate.Conditions {
				conditions = append(conditions, AssertionDetails(condition))
			}
			item["conditions"] = conditions
		}
		out = append(out, item)
	}
	return out
}
