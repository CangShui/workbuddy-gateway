package main

import (
	"log"
	"net/http"
	"strings"
)

type toolSequenceRepairReport struct {
	OriginalMessages   int
	FinalMessages      int
	ParallelBatches    int
	MovedMessages      int
	MergedCallMessages int
	DroppedCalls       int
	DroppedOutputs     int
	TopologyBefore     string
	TopologyAfter      string
}

func (r toolSequenceRepairReport) changed() bool {
	return r.OriginalMessages != r.FinalMessages || r.MovedMessages > 0 || r.MergedCallMessages > 0 ||
		r.DroppedCalls > 0 || r.DroppedOutputs > 0
}

// repairToolMessageSequence 修复发往上游的现代 tool_calls/tool 序列。
//
// 规则：
//   - 先按 tool call ID 做对称裁剪：无结果的调用、无调用的结果、重复项全部删除；
//   - 仅当同一批次至少有两个调用时，才把夹在调用和结果之间的普通消息移到结果之后；
//   - Responses API 转换产生的连续/交错单调用 assistant 消息会合并为一个并行批次；
//   - 结果按原始出现顺序保留，不按调用顺序重排。
func repairToolMessageSequence(obj map[string]any) toolSequenceRepairReport {
	messages, ok := obj["messages"].([]any)
	report := toolSequenceRepairReport{OriginalMessages: len(messages), FinalMessages: len(messages)}
	if !ok || len(messages) == 0 {
		return report
	}
	// 拓扑签名必须在就地修改前抓取（过滤阶段会改写 assistant 消息的 tool_calls）。
	topologyBefore := toolMessageTopology(messages)

	// 每个 ID 只保留第一条调用，以及位于该调用之后的第一条结果。
	callMessageIndex := make(map[string]int)
	callEntryIndex := make(map[string]int)
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok || roleOfMessage(message) != "assistant" {
			continue
		}
		calls, _ := message["tool_calls"].([]any)
		for entryIndex, callAny := range calls {
			id := toolCallID(callAny)
			if id == "" {
				continue
			}
			if _, exists := callMessageIndex[id]; !exists {
				callMessageIndex[id] = messageIndex
				callEntryIndex[id] = entryIndex
			}
		}
	}

	outputMessageIndex := make(map[string]int)
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok || roleOfMessage(message) != "tool" {
			continue
		}
		id := toolOutputID(message)
		callIndex, exists := callMessageIndex[id]
		if !exists || messageIndex <= callIndex {
			continue
		}
		if _, exists := outputMessageIndex[id]; !exists {
			outputMessageIndex[id] = messageIndex
		}
	}

	pairedIDs := make(map[string]bool, len(outputMessageIndex))
	for id := range outputMessageIndex {
		pairedIDs[id] = true
	}

	filtered := make([]any, 0, len(messages))
	for messageIndex, messageAny := range messages {
		message, ok := messageAny.(map[string]any)
		if !ok {
			filtered = append(filtered, messageAny)
			continue
		}

		switch roleOfMessage(message) {
		case "assistant":
			calls, hasCalls := message["tool_calls"].([]any)
			if !hasCalls || len(calls) == 0 {
				filtered = append(filtered, messageAny)
				continue
			}
			kept := make([]any, 0, len(calls))
			for entryIndex, callAny := range calls {
				id := toolCallID(callAny)
				if id != "" && pairedIDs[id] && callMessageIndex[id] == messageIndex && callEntryIndex[id] == entryIndex {
					kept = append(kept, callAny)
				} else {
					report.DroppedCalls++
				}
			}
			if len(kept) > 0 {
				message["tool_calls"] = kept
				filtered = append(filtered, messageAny)
				continue
			}
			delete(message, "tool_calls")
			if assistantMessageHasPayload(message) {
				filtered = append(filtered, messageAny)
			}

		case "tool":
			id := toolOutputID(message)
			if id != "" && pairedIDs[id] && outputMessageIndex[id] == messageIndex {
				filtered = append(filtered, messageAny)
			} else {
				report.DroppedOutputs++
			}

		default:
			filtered = append(filtered, messageAny)
		}
	}

	repaired := repairParallelToolBatches(filtered, &report)
	obj["messages"] = repaired
	report.FinalMessages = len(repaired)
	if report.changed() {
		report.TopologyBefore = topologyBefore
		report.TopologyAfter = toolMessageTopology(repaired)
	}
	return report
}

// toolMessageTopology 生成有界的消息拓扑签名：仅角色与工具调用 ID，不含任何正文内容。
// 最多记录前 40 条，用于在真实请求上核对工具序列结构而不泄露提示词或工具结果。
func toolMessageTopology(messages []any) string {
	const maxEntries = 200
	parts := make([]string, 0, maxEntries)
	for i, messageAny := range messages {
		if i >= maxEntries {
			parts = append(parts, "...")
			break
		}
		message, ok := messageAny.(map[string]any)
		if !ok {
			parts = append(parts, "?")
			continue
		}
		role := roleOfMessage(message)
		if role == "assistant" {
			if calls, ok := message["tool_calls"].([]any); ok && len(calls) > 0 {
				ids := make([]string, 0, len(calls))
				for _, callAny := range calls {
					ids = append(ids, toolCallID(callAny))
				}
				parts = append(parts, "assistant["+strings.Join(ids, ",")+"]")
				continue
			}
		}
		if role == "tool" {
			parts = append(parts, "tool["+toolOutputID(message)+"]")
			continue
		}
		parts = append(parts, role)
	}
	return strings.Join(parts, " > ")
}

func repairParallelToolBatches(messages []any, report *toolSequenceRepairReport) []any {
	out := make([]any, 0, len(messages))
	for i := 0; i < len(messages); {
		first, firstCalls, ok := assistantCalls(messages[i])
		if !ok {
			out = append(out, messages[i])
			i++
			continue
		}

		batchCalls := append([]any(nil), firstCalls...)
		deferred := make([]any, 0)
		mergedMessages := 0
		j := i + 1
		for j < len(messages) {
			if next, calls, hasCalls := assistantCalls(messages[j]); hasCalls {
				if !assistantCallOnly(next) {
					break
				}
				batchCalls = append(batchCalls, calls...)
				mergedMessages++
				j++
				continue
			}
			if message, ok := messages[j].(map[string]any); ok && roleOfMessage(message) == "tool" {
				break
			}
			deferred = append(deferred, messages[j])
			j++
		}

		if len(batchCalls) < 2 {
			out = append(out, messages[i])
			i++
			continue
		}

		ids := make(map[string]bool, len(batchCalls))
		for _, callAny := range batchCalls {
			if id := toolCallID(callAny); id != "" {
				ids[id] = true
			}
		}
		outputs := make([]any, 0, len(ids))
		found := make(map[string]bool, len(ids))
		k := j
		for k < len(messages) && len(found) < len(ids) {
			if _, _, nextBatch := assistantCalls(messages[k]); nextBatch {
				break
			}
			if message, ok := messages[k].(map[string]any); ok && roleOfMessage(message) == "tool" {
				id := toolOutputID(message)
				if ids[id] && !found[id] {
					outputs = append(outputs, messages[k])
					found[id] = true
					k++
					continue
				}
			}
			deferred = append(deferred, messages[k])
			k++
		}

		if len(found) != len(ids) {
			out = append(out, messages[i])
			i++
			continue
		}

		first["tool_calls"] = batchCalls
		out = append(out, first)
		out = append(out, outputs...)
		out = append(out, deferred...)
		report.ParallelBatches++
		report.MovedMessages += len(deferred)
		report.MergedCallMessages += mergedMessages
		i = k
	}
	return out
}

func assistantCalls(messageAny any) (map[string]any, []any, bool) {
	message, ok := messageAny.(map[string]any)
	if !ok || roleOfMessage(message) != "assistant" {
		return nil, nil, false
	}
	calls, ok := message["tool_calls"].([]any)
	if !ok || len(calls) == 0 {
		return nil, nil, false
	}
	return message, calls, true
}

func assistantCallOnly(message map[string]any) bool {
	if assistantMessageHasPayload(message) {
		return false
	}
	for key := range message {
		switch key {
		case "role", "content", "tool_calls":
		default:
			return false
		}
	}
	return true
}

func assistantMessageHasPayload(message map[string]any) bool {
	content, exists := message["content"]
	if !exists || content == nil {
		return false
	}
	switch value := content.(type) {
	case string:
		return strings.TrimSpace(value) != ""
	case []any:
		return len(value) > 0
	default:
		return true
	}
}

func toolCallID(callAny any) string {
	call, ok := callAny.(map[string]any)
	if !ok {
		return ""
	}
	id, _ := call["id"].(string)
	return strings.TrimSpace(id)
}

func toolOutputID(message map[string]any) string {
	id, _ := message["tool_call_id"].(string)
	return strings.TrimSpace(id)
}

func logToolSequenceRepair(r *http.Request, traceID string, requestID uint64, model string, report toolSequenceRepairReport) {
	result := "unchanged"
	if report.changed() {
		result = "repaired"
		log.Printf("[ToolSequenceRepair] traceId=%s requestId=%d 模型=%s 结果=已修复 原消息=%d 修复后=%d 并行批次=%d 移出插入消息=%d 合并调用消息=%d 删除无结果调用=%d 删除孤儿或重复结果=%d 业务影响=避免上游11148工具调用序列断裂",
			traceID, requestID, model, report.OriginalMessages, report.FinalMessages, report.ParallelBatches,
			report.MovedMessages, report.MergedCallMessages, report.DroppedCalls, report.DroppedOutputs)
	}
	if !debugLoggingEnabled() {
		return
	}
	fields := map[string]any{
		"result":                       result,
		"original_messages":            report.OriginalMessages,
		"final_messages":               report.FinalMessages,
		"parallel_batches":             report.ParallelBatches,
		"moved_messages":               report.MovedMessages,
		"merged_call_messages":         report.MergedCallMessages,
		"dropped_calls_without_output": report.DroppedCalls,
		"dropped_orphan_outputs":       report.DroppedOutputs,
		"business_impact":              "出站工具调用序列已完成校验，防止上游11148错误",
	}
	if report.changed() {
		fields["topology_before"] = report.TopologyBefore
		fields["topology_after"] = report.TopologyAfter
	}
	debugEvent(r, "debug", "tool_sequence_checked", fields)
}
