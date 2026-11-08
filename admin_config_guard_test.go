package traefikllmgateway

import (
	"reflect"
	"strings"
	"testing"
)

// nonSecretConfigKeys lists every string-valued JSON key in Config reviewed as
// non-secret. A new key must be classified here or in the redaction sets.
var nonSecretConfigKeys = map[string]bool{
	"agents": true, "allowedCIDRs": true, "backoff": true, "cacheTTL": true,
	"card": true, "discoveryInterval": true, "file": true, "group": true,
	"groups": true, "maxOpenDuration": true, "mcpServers": true,
	"metadataPath": true, "modelAliases": true, "modelBaseUrls": true, "models": true, "name": true,
	"openDuration": true, "passthroughPaths": true, "path": true, "pathBaseUrls": true,
	"probeInterval": true, "providers": true, "requestTimeout": true,
	"ttl": true, "type": true,
}

func collectStringKeys(t reflect.Type, seen map[reflect.Type]bool, out map[string]bool) {
	for t.Kind() == reflect.Pointer || t.Kind() == reflect.Slice || t.Kind() == reflect.Array || t.Kind() == reflect.Map {
		t = t.Elem()
	}
	if t.Kind() != reflect.Struct || seen[t] {
		return
	}
	seen[t] = true
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		tag := strings.Split(f.Tag.Get("json"), ",")[0]
		if tag == "-" || !f.IsExported() {
			continue
		}
		if tag == "" {
			tag = f.Name
		}
		ft := f.Type
		for ft.Kind() == reflect.Pointer || ft.Kind() == reflect.Slice || ft.Kind() == reflect.Array || ft.Kind() == reflect.Map {
			ft = ft.Elem()
		}
		if ft.Kind() == reflect.String {
			out[tag] = true
		}
		collectStringKeys(ft, seen, out)
	}
}

func TestRedaction_EveryStringKeyClassified(t *testing.T) {
	keys := map[string]bool{}
	collectStringKeys(reflect.TypeOf(Config{}), map[reflect.Type]bool{}, keys)
	for k := range keys {
		if !redactedConfigKeys[k] && !hiddenConfigKeys[k] && !baseURLLikeConfigKeys[k] && !nonSecretConfigKeys[k] {
			t.Errorf("Config string key %q is unclassified: add it to the redaction sets or nonSecretConfigKeys", k)
		}
	}
	for k := range nonSecretConfigKeys {
		if redactedConfigKeys[k] || hiddenConfigKeys[k] || baseURLLikeConfigKeys[k] {
			t.Errorf("key %q is both redacted and declared non-secret", k)
		}
	}
}
