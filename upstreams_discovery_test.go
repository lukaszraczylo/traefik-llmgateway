package traefikllmgateway

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func modelsServer(t *testing.T, body string, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			hits.Add(1)
		}
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIAdapter_ModelBaseURLs_DiscoveryAndMetadataQueryEveryUpstream(t *testing.T) {
	t.Parallel()
	var hitsA, hitsB atomic.Int32
	// Upstream B also lists "only-on-main", which is routed to baseURL, so
	// B's claim on it must be ignored.
	a := modelsServer(t, `{"data":[{"id":"main-model","max_model_len":1000},{"id":"only-on-main","max_model_len":2000}]}`, &hitsA)
	b := modelsServer(t, `{"data":[{"id":"glm","max_model_len":524288},{"id":"qwen","max_model_len":262144,"loaded_context_length":100000},{"id":"only-on-main","max_model_len":999}]}`, &hitsB)

	ad := newOpenAIAdapter("d1", a.URL, "")
	ad.modelBases = map[string]string{"glm": b.URL, "qwen": b.URL}
	ad.metadataPath = "/v1/models"

	ids, err := ad.listModels(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"main-model", "only-on-main", "glm", "qwen"}, ids)

	got, err := ad.fetchModelMetadata(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]int{
		"main-model":   1000,
		"only-on-main": 2000,
		"glm":          524288,
		"qwen":         100000,
	}, got)
	assert.EqualValues(t, 2, hitsA.Load(), "baseURL queried once per call")
	assert.EqualValues(t, 2, hitsB.Load(), "shared modelBaseUrls upstream queried once per call, not once per model")
}

func TestOpenAIAdapter_ModelBaseURLs_FailingUpstream(t *testing.T) {
	t.Parallel()
	good := modelsServer(t, `{"data":[{"id":"main-model","max_model_len":1000}]}`, nil)
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(bad.Close)

	ad := newOpenAIAdapter("d1", good.URL, "")
	ad.modelBases = map[string]string{"glm": bad.URL}
	ad.metadataPath = "/v1/models"

	_, err := ad.listModels(context.Background())
	assert.Error(t, err, "discovery must fail rather than drop the failing upstream's models")

	got, err := ad.fetchModelMetadata(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"main-model": 1000}, got, "metadata keeps the healthy upstream's ids")
}
