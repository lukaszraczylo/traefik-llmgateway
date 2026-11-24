package traefikllmgateway

const (
	// smartSizeMaxDepth and smartSizeMaxNodes bound the walk so a hostile
	// payload cannot make the size estimate expensive.
	smartSizeMaxDepth = 12
	smartSizeMaxNodes = 4096
	// smartScalarSize approximates a serialized number, bool or null.
	smartScalarSize = 6
)

// smartValueSize estimates the JSON-serialized byte length of a decoded
// value without allocating. Past the bounds it returns a lower bound.
func smartValueSize(v any) int {
	budget := smartSizeMaxNodes
	return smartValueSizeWalk(v, 0, &budget)
}

func smartValueSizeWalk(v any, depth int, budget *int) int {
	switch x := v.(type) {
	case string:
		return len(x) + 2
	case map[string]any:
		n := 2
		if depth >= smartSizeMaxDepth {
			return n
		}
		for k, val := range x {
			if *budget <= 0 {
				break
			}
			*budget--
			n += len(k) + 4 + smartValueSizeWalk(val, depth+1, budget)
		}
		return n
	case []any:
		n := 2
		if depth >= smartSizeMaxDepth {
			return n
		}
		for i := 0; i < len(x); i++ {
			if *budget <= 0 {
				break
			}
			*budget--
			n += 1 + smartValueSizeWalk(x[i], depth+1, budget)
		}
		return n
	}
	return smartScalarSize
}

// smartToolCallsSize sums the argument payloads of OpenAI tool_calls.
func smartToolCallsSize(calls any) int {
	list, ok := calls.([]any)
	if !ok {
		return 0
	}
	n := 0
	for i := 0; i < len(list); i++ {
		call, isMap := list[i].(map[string]any)
		if !isMap {
			continue
		}
		fn, hasFn := call["function"].(map[string]any)
		if !hasFn {
			continue
		}
		if args, isStr := fn["arguments"].(string); isStr {
			n += len(args)
		} else {
			n += smartValueSize(fn["arguments"])
		}
	}
	return n
}
