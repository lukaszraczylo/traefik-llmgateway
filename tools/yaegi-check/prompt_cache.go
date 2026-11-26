// This file probes the prompt-caching paths under Yaegi: a cached-price cost, cache_control injection and the
// hashed prompt_cache_key. They call the interpreted package's own functions, so a result that differs from
// native Go fails the run.
package main

import (
	"encoding/json"
	"fmt"
	"reflect"
	"regexp"
	"strings"

	"github.com/traefik/yaegi/interp"
)

const (
	// pcWantCostMicros is native Go's result for pcCostExpr: 100000*1 + 800000*0.1 + 100000*1.25 + 500000*2.
	pcWantCostMicros = 1305000
	pcWantMarkers    = 4
	pcKeyHexLen      = 32
)

const (
	pcCachedInVar   = "pcCachedIn"
	pcCacheWriteVar = "pcCacheWrite"
	pcCostExpr      = "%[1]s.computeCostMicros(%[1]s.ModelPricing{InputPerM: 1, OutputPerM: 2, CachedInputPerM: &" + pcCachedInVar +
		", CacheWritePerM: &" + pcCacheWriteVar + "}, %[1]s.usage{prompt: 1000000, completion: 500000, cached: 800000, cacheWrite: 100000})"
)

var pcKeyPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

func pcBody() map[string]any {
	return map[string]any{
		"model":  "claude-x",
		"system": "sys",
		"tools":  []any{map[string]any{"name": "a"}, map[string]any{"name": "b"}},
		"messages": []any{
			map[string]any{"role": "user", "content": "u0"},
			map[string]any{"role": "assistant", "content": "a1"},
			map[string]any{"role": "user", "content": "u2"},
			map[string]any{"role": "assistant", "content": "a3"},
			map[string]any{"role": "user", "content": []any{map[string]any{"type": "text", "text": "u4"}}},
		},
	}
}

func pcJSON(v any) (string, error) {
	b, err := json.Marshal(v)
	return string(b), err
}

// exercisePromptCache checks cost with a cached price, cache_control injection and the prompt_cache_key hash.
func exercisePromptCache(i *interp.Interpreter, pkgName string) error {
	for _, decl := range []string{pcCachedInVar + " := 0.1", pcCacheWriteVar + " := 1.25"} {
		if _, err := i.Eval(decl); err != nil {
			return fmt.Errorf("prompt cache probe: declare %q under yaegi: %w", decl, err)
		}
	}
	costVal, err := i.Eval(fmt.Sprintf(pcCostExpr, pkgName))
	if err != nil {
		return fmt.Errorf("prompt cache probe: evaluate computeCostMicros under yaegi: %w", err)
	}
	got := costVal.Interface()
	if n, ok := got.(int64); !ok || n != pcWantCostMicros {
		return fmt.Errorf("prompt cache probe: computeCostMicros with a cached price = %v, want %d", got, pcWantCostMicros)
	}
	if err = exerciseCacheInjection(i, pkgName); err != nil {
		return err
	}
	if err = exerciseCacheKey(i, pkgName); err != nil {
		return err
	}
	fmt.Println("yaegi-check: cached-price cost, cache_control injection and prompt_cache_key matched native Go under the interpreter")
	return nil
}

func exerciseCacheInjection(i *interp.Interpreter, pkgName string) error {
	fn, err := i.Eval(pkgName + ".injectCacheControl")
	if err != nil {
		return fmt.Errorf("prompt cache probe: resolve injectCacheControl under yaegi: %w", err)
	}
	if fn.Kind() != reflect.Func {
		return fmt.Errorf("prompt cache probe: injectCacheControl is not a function (kind %s)", fn.Kind())
	}
	body := pcBody()
	before, err := pcJSON(body)
	if err != nil {
		return err
	}
	outs := make([]string, 0, 2)
	for run := 0; run < 2; run++ {
		res := fn.Call([]reflect.Value{reflect.ValueOf(body)})
		if len(res) != 2 {
			return fmt.Errorf("prompt cache probe: injectCacheControl returned %d values, want 2", len(res))
		}
		out, encErr := pcJSON(res[0].Interface())
		if encErr != nil {
			return encErr
		}
		if added := res[1].Int(); added != pcWantMarkers {
			return fmt.Errorf("prompt cache probe: run %d injected %d markers, want %d", run, added, pcWantMarkers)
		}
		if n := strings.Count(out, `"cache_control"`); n != pcWantMarkers {
			return fmt.Errorf("prompt cache probe: run %d output carries %d cache_control keys, want %d", run, n, pcWantMarkers)
		}
		outs = append(outs, out)
	}
	if outs[0] != outs[1] {
		return fmt.Errorf("prompt cache probe: two injections of one body differ:\n%s\n%s", outs[0], outs[1])
	}
	after, err := pcJSON(body)
	if err != nil {
		return err
	}
	if after != before {
		return fmt.Errorf("prompt cache probe: injectCacheControl mutated its input:\n%s\n%s", before, after)
	}
	return nil
}

func exerciseCacheKey(i *interp.Interpreter, pkgName string) error {
	var keys [2]string
	for run := range keys {
		v, err := i.Eval(pkgName + `.hashPromptCacheKey("caller-1", "session-1")`)
		if err != nil {
			return fmt.Errorf("prompt cache probe: evaluate hashPromptCacheKey under yaegi: %w", err)
		}
		keys[run] = v.String()
	}
	if keys[0] != keys[1] || !pcKeyPattern.MatchString(keys[0]) {
		return fmt.Errorf("prompt cache probe: prompt_cache_key %q then %q, want one stable %d-hex value", keys[0], keys[1], pcKeyHexLen)
	}
	other, err := i.Eval(pkgName + `.hashPromptCacheKey("caller-2", "session-1")`)
	if err != nil {
		return fmt.Errorf("prompt cache probe: evaluate hashPromptCacheKey under yaegi: %w", err)
	}
	if other.String() == keys[0] {
		return fmt.Errorf("prompt cache probe: two callers share prompt_cache_key %q", keys[0])
	}
	return nil
}
