package traefikllmgateway

import "bytes"

const (
	// routerBadLengthMinTokens is the completion size from which a length cut counts as bad;
	// a small client max_tokens cutting a short answer is the client's choice.
	routerBadLengthMinTokens = 512

	finishReasonLength        = "length"
	finishReasonContentFilter = "content_filter"
)

var (
	finishKey    = []byte(`"finish_reason":`)
	contentKey   = []byte(`"content":`)
	toolCallsKey = []byte(`"tool_calls":`)
)

// withAnswerFlags sets the answer-quality flags from a finish reason and whether any
// text or tool call came back. An unknown finish reason sets nothing: the stream may have been cut.
func withAnswerFlags(u usage, finish string, hasOutput bool) usage {
	if finish == "" {
		return u
	}
	u.finishLength = finish == finishReasonLength
	u.emptyOutput = !hasOutput && finish != finishReasonContentFilter
	return u
}

// anthropicHasOutput reports whether any content block carries text or a tool call.
func anthropicHasOutput(blocks []anthropicContentBlock) bool {
	for i := 0; i < len(blocks); i++ {
		if blocks[i].Type == "tool_use" || (blocks[i].Type == "text" && blocks[i].Text != "") {
			return true
		}
	}
	return false
}

// smartJSONEmpty reports whether a raw JSON value is absent, null, or an empty string, array or object.
func smartJSONEmpty(raw []byte) bool {
	v := bytes.TrimSpace(raw)
	switch string(v) {
	case "", "null", `""`, "[]", "{}":
		return true
	}
	return false
}

// skipJSONSpace returns the index of the first byte at or after i that is not JSON whitespace.
func skipJSONSpace(data []byte, i int) int {
	for i < len(data) && (data[i] == ' ' || data[i] == '\n' || data[i] == '\t' || data[i] == '\r') {
		i++
	}
	return i
}

// chunkFinishReason returns the finish_reason of one stream chunk, or "" when it is absent or null.
func chunkFinishReason(data []byte) string {
	at := bytes.Index(data, finishKey)
	if at < 0 {
		return ""
	}
	i := skipJSONSpace(data, at+len(finishKey))
	if i >= len(data) || data[i] != '"' {
		return ""
	}
	end := bytes.IndexByte(data[i+1:], '"')
	if end < 0 {
		return ""
	}
	return string(data[i+1 : i+1+end])
}

// keyHasValue reports whether key occurs with a value other than null, an empty string or an empty array.
func keyHasValue(data, key []byte) bool {
	from := 0
	for from < len(data) {
		at := bytes.Index(data[from:], key)
		if at < 0 {
			return false
		}
		i := skipJSONSpace(data, from+at+len(key))
		if i < len(data) {
			switch data[i] {
			case 'n':
			case '"':
				if i+1 < len(data) && data[i+1] != '"' {
					return true
				}
			case '[':
				if j := skipJSONSpace(data, i+1); j < len(data) && data[j] != ']' {
					return true
				}
			default:
				return true
			}
		}
		from += at + len(key)
	}
	return false
}

// chunkHasOutput reports whether one stream chunk carries answer text or a tool call.
func chunkHasOutput(data []byte) bool {
	return keyHasValue(data, contentKey) || keyHasValue(data, toolCallsKey)
}

// badResult reports whether a finished attempt counts as a bad outcome: a mid-stream failure,
// an empty answer, or a length cut that is not the client's small max_tokens.
func (p *routerPlan) badResult(u usage, callErr error, wroteHeader bool) bool {
	if callErr != nil {
		return wroteHeader
	}
	if u.emptyOutput {
		return true
	}
	return u.finishLength && (u.completion >= routerBadLengthMinTokens || !p.maxTokensSet)
}
