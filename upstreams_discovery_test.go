package traefikllmgateway

import (
	"context"
	"encoding/json"
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

func statusServer(t *testing.T, code int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(code)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestOpenAIAdapter_ModelBaseURLs_FailingUpstream(t *testing.T) {
	t.Parallel()
	good := modelsServer(t, `{"data":[{"id":"main-model","max_model_len":1000}]}`, nil)
	bad := statusServer(t, http.StatusInternalServerError)

	ad := newOpenAIAdapter("d1", good.URL, "")
	ad.modelBases = map[string]string{"glm": bad.URL}
	ad.metadataPath = "/v1/models"

	ids, err := ad.listModels(context.Background())
	require.NoError(t, err, "a failing modelBaseUrls upstream must not fail discovery")
	assert.Equal(t, []string{"main-model"}, ids)
	errs := ad.upstreamErrors()
	require.Len(t, errs, 1)
	assert.Equal(t, bad.URL, errs[0].Upstream)

	got, err := ad.fetchModelMetadata(context.Background())
	require.NoError(t, err)
	assert.Equal(t, map[string]int{"main-model": 1000}, got, "metadata keeps the healthy upstream's ids")
}

func TestOpenAIAdapter_BaseURLFailure_FailsDiscovery(t *testing.T) {
	t.Parallel()
	good := modelsServer(t, `{"data":[{"id":"glm"}]}`, nil)
	bad := statusServer(t, http.StatusBadGateway)

	ad := newOpenAIAdapter("d1", bad.URL, "")
	ad.modelBases = map[string]string{"glm": good.URL}

	_, err := ad.listModels(context.Background())
	assert.Error(t, err)
	assert.Empty(t, ad.upstreamErrors())
}

// Mirrors uni on d1: a TEI upstream answering 404 and a scaled-to-zero
// reranker answering 502 must not degrade the provider.
func TestGateway_ModelBaseURLsFailures_ProviderStaysHealthy(t *testing.T) {
	t.Parallel()
	main := modelsServer(t, `{"data":[{"id":"main-model"}]}`, nil)
	other := modelsServer(t, `{"data":[{"id":"glm"},{"id":"main-model"}]}`, nil)
	tei := statusServer(t, http.StatusNotFound)
	rerank := statusServer(t, http.StatusBadGateway)

	cfg := newAdminTestConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"uni": {
			Type: "openai", BaseURL: main.URL, APIKey: "sk", Discovery: true,
			Models: []string{"e5-tei", "reranker"},
			ModelBaseURLs: map[string]string{
				"glm": other.URL, "e5-tei": tei.URL, "reranker": rerank.URL,
			},
		},
	}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got adminOverviewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Providers, 1)
	p := got.Providers[0]

	assert.Empty(t, p.LastErr)
	assert.Equal(t, "closed", p.HealthState)
	assert.Equal(t, []string{"e5-tei", "glm", "main-model", "reranker"}, p.Models)

	require.Len(t, p.UpstreamErrors, 2)
	byUp := map[string]string{}
	for _, e := range p.UpstreamErrors {
		byUp[e.Upstream] = e.Error
	}
	assert.Contains(t, byUp[tei.URL], "404")
	assert.Contains(t, byUp[rerank.URL], "502")
	assert.Contains(t, rec.Body.String(), `"upstreamErrors"`)
}

func TestGateway_BaseURLFailure_ProviderDegradedAsBefore(t *testing.T) {
	t.Parallel()
	bad := statusServer(t, http.StatusNotFound)
	good := modelsServer(t, `{"data":[{"id":"glm"}]}`, nil)

	cfg := newAdminTestConfig()
	cfg.Providers = map[string]*ProviderConfig{
		"uni": {
			Type: "openai", BaseURL: bad.URL, APIKey: "sk", Discovery: true,
			Models:        []string{"glm"},
			ModelBaseURLs: map[string]string{"glm": good.URL},
		},
	}
	h, _ := newAdminGatewayHandle(t, cfg)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminOverviewPath, "sk-admin1"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got adminOverviewResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))
	require.Len(t, got.Providers, 1)
	assert.Contains(t, got.Providers[0].LastErr, "404")
	assert.Empty(t, got.Providers[0].UpstreamErrors)
}
