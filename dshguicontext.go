package main

import "strings"

const (
	dshGUIContextSentence = "You are interacting with the user through the DeepSeek Harness Web GUI whose URL the runtime context names."
	dshGUIContextNeutral  = "You are interacting with the user through a Web GUI whose URL the runtime context names."
	// The known two-sentence opening also occurs in historical messages and in
	// longer/customized prompt sections. Their surrounding instructions are not
	// part of the match and must be retained byte for byte.
	dshGUIContextOpening = dshGUIContextSentence + " When the user refers to \"this page\", \"this GUI\", or \"this app\" without naming another target, they mean this GUI."
)

// normalizeDSHGUIContextText changes only the environment sentence of the known
// two-sentence opening, outside code. All surrounding instructions are retained.
// It does not require an exact version of the whole paragraph or a DSH header.
func normalizeDSHGUIContextText(text string) (string, int) {
	if !strings.Contains(text, dshGUIContextOpening) {
		return text, 0
	}
	var out strings.Builder
	copied, changed := 0, 0
	var fence byte
	fenceLength := 0
	codeTicks := 0
	for start := 0; start < len(text); {
		end := start + strings.IndexByte(text[start:], '\n')
		if end < start {
			end = len(text)
		}
		line := strings.TrimSuffix(text[start:end], "\r")
		next := end
		if next < len(text) {
			next++
		}
		trimmed := strings.TrimLeft(line, " ")
		isFence := false
		if codeTicks == 0 && len(line)-len(trimmed) <= 3 && len(trimmed) >= 3 && (trimmed[0] == '`' || trimmed[0] == '~') {
			n := 1
			for n < len(trimmed) && trimmed[n] == trimmed[0] {
				n++
			}
			if n >= 3 {
				if fence == 0 {
					fence, fenceLength, isFence = trimmed[0], n, true
				} else if fence == trimmed[0] && n >= fenceLength && strings.TrimSpace(trimmed[n:]) == "" {
					fence, fenceLength, isFence = 0, 0, true
				}
			}
		}
		indentedCode := len(line)-len(trimmed) >= 4 || strings.HasPrefix(line, "\t")
		if !isFence && fence == 0 && (!indentedCode || codeTicks > 0) {
			if normalized, n := normalizeDSHGUIContextLine(line, &codeTicks); n > 0 {
				out.WriteString(text[copied:start])
				out.WriteString(normalized)
				if len(line) != end-start {
					out.WriteByte('\r')
				}
				copied, changed = end, changed+n
			}
		}
		start = next
	}
	if changed == 0 {
		return text, 0
	}
	out.WriteString(text[copied:])
	return out.String(), changed
}

// The delimiter state belongs to the whole text: code spans can cross lines.
func normalizeDSHGUIContextLine(line string, codeTicks *int) (string, int) {
	var out strings.Builder
	copied, changed := 0, 0
	for pos := 0; pos < len(line); {
		if line[pos] == '`' {
			n := 1
			for pos+n < len(line) && line[pos+n] == '`' {
				n++
			}
			if *codeTicks == 0 {
				*codeTicks = n
			} else if *codeTicks == n {
				*codeTicks = 0
			}
			pos += n
			continue
		}
		if *codeTicks == 0 && strings.HasPrefix(line[pos:], dshGUIContextOpening) {
			out.WriteString(line[copied:pos])
			out.WriteString(dshGUIContextNeutral)
			out.WriteString(dshGUIContextOpening[len(dshGUIContextSentence):])
			pos += len(dshGUIContextOpening)
			copied, changed = pos, changed+1
		} else {
			pos++
		}
	}
	if changed == 0 {
		return line, 0
	}
	out.WriteString(line[copied:])
	return out.String(), changed
}

func normalizeDSHGUIContexts(obj map[string]any) int {
	messages, _ := obj["messages"].([]any)
	changed := 0
	for _, raw := range messages {
		message, ok := raw.(map[string]any)
		if !ok || (roleOfMessage(message) != "system" && roleOfMessage(message) != "assistant") {
			continue
		}
		switch content := message["content"].(type) {
		case string:
			normalized, n := normalizeDSHGUIContextText(content)
			if n > 0 {
				message["content"] = normalized
				changed += n
			}
		case []any:
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				if block["type"] != "text" {
					continue
				}
				if text, ok := block["text"].(string); ok {
					normalized, n := normalizeDSHGUIContextText(text)
					if n > 0 {
						block["text"] = normalized
						changed += n
					}
				}
			}
		}
	}
	return changed
}
