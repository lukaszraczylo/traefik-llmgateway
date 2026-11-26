package traefikllmgateway

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// Fleet metrics: with a limiter store, /metrics renders these families from
// the shared counters instead of this pod's memory, because the route that
// serves /metrics handles no chat traffic itself.

const (
	kindProvenance      = "provenance"
	metricProvenanceReq = "vreq"
	metricProvenanceTok = "vtok"

	// Latency sums are in milliseconds. The "s" variants count streaming
	// observations only; the non-streaming series is the difference, so the
	// existing ld/lt counters keep their meaning.
	metricLatDurSum        = "ldsum"
	metricLatTTFBSum       = "ltsum"
	metricLatStreamDurSum  = "lsdsum"
	metricLatStreamTTFBSum = "lstsum"
	latencyStreamDurFmt    = "lsd%02d"
	latencyStreamTTFBFmt   = "lst%02d"
)

var provenanceOutcomes = []string{provenanceReported, provenanceEstimated, provenanceUnbilled}

// latencyHistKeys is how many store keys one (kind, id) latency block reads:
// per histogram, every bucket plus overflow and one sum, for all and streaming.
var latencyHistKeys = 2 * (len(latencyBucketBounds) + 2)

func durationMillis(d time.Duration) int64 {
	return int64((d + time.Millisecond/2) / time.Millisecond)
}

// fleetLatencyEntries returns the day-window sum and streaming counters that
// ride beside the ld/lt bucket counters.
func fleetLatencyEntries(kind, id string, lat latencySample, wb windowBuckets, et enforceTTLSet) []counterIncr {
	out := make([]counterIncr, 0, 8)
	add := func(metric string, delta int64) {
		if delta <= 0 {
			return
		}
		out = append(out, counterIncr{key: windowKeyForBucket(kind, id, metric, windowDay, wb.day), delta: delta, ttl: dayWindowTTL, enforceTTL: et.day})
	}
	durIdx, _ := latencyBucketIndex(lat.duration)
	durMs := durationMillis(lat.duration)
	add(metricLatDurSum, durMs)
	if lat.streaming {
		add(fmt.Sprintf(latencyStreamDurFmt, durIdx), 1)
		add(metricLatStreamDurSum, durMs)
	}
	if lat.hasTTFB {
		ttfbIdx, _ := latencyBucketIndex(lat.ttfb)
		ttfbMs := durationMillis(lat.ttfb)
		add(metricLatTTFBSum, ttfbMs)
		if lat.streaming {
			add(fmt.Sprintf(latencyStreamTTFBFmt, ttfbIdx), 1)
			add(metricLatStreamTTFBSum, ttfbMs)
		}
	}
	return out
}

// fleetStore reports whether this Gateway has a shared limiter store.
func (g *Gateway) fleetStore() bool {
	return g.limiter != nil && g.limiter.store != nil
}

func (g *Gateway) fleetLatencyEnabled() bool {
	return g.fleetStore() && g.limiter.statsLatency
}

// latencyBlockKeys lists one (kind, id) block in a fixed order: duration then
// TTFB; within each, all-bucket counters, all-sum, streaming buckets,
// streaming-sum.
func latencyBlockKeys(kind, id, bucket string) []string {
	n := len(latencyBucketBounds) + 1
	keys := make([]string, 0, 2*latencyHistKeys)
	key := func(metric string) string {
		return windowKeyForBucket(kind, id, metric, windowDay, bucket)
	}
	for i := 0; i < n; i++ {
		keys = append(keys, key(latencyDurationMetric(i)))
	}
	keys = append(keys, key(metricLatDurSum))
	for i := 0; i < n; i++ {
		keys = append(keys, key(fmt.Sprintf(latencyStreamDurFmt, i)))
	}
	keys = append(keys, key(metricLatStreamDurSum))
	for i := 0; i < n; i++ {
		keys = append(keys, key(latencyTTFBMetric(i)))
	}
	keys = append(keys, key(metricLatTTFBSum))
	for i := 0; i < n; i++ {
		keys = append(keys, key(fmt.Sprintf(latencyStreamTTFBFmt, i)))
	}
	keys = append(keys, key(metricLatStreamTTFBSum))
	return keys
}

func clampNonNegative(v int64) int64 {
	if v < 0 {
		return 0
	}
	return v
}

// latencyHalf is the dense bucket counts (overflow last), their sum in
// milliseconds, and their total for one histogram of one stream class.
type latencyHalf struct {
	buckets []int64
	sumMs   int64
	count   int64
}

// splitLatencyBlock turns one block of values into the non-streaming and
// streaming halves for duration and TTFB. A counter that lags its streaming
// twin is clamped at zero instead of going negative.
func splitLatencyBlock(vals []int64) (latencyHalf, latencyHalf, latencyHalf, latencyHalf) {
	n := len(latencyBucketBounds) + 1
	half := func(off int) (latencyHalf, latencyHalf) {
		all := vals[off : off+n]
		allSum := vals[off+n]
		str := vals[off+n+1 : off+2*n+1]
		strSum := vals[off+2*n+1]
		plain := latencyHalf{buckets: make([]int64, n), sumMs: clampNonNegative(allSum - strSum)}
		stream := latencyHalf{buckets: make([]int64, n), sumMs: clampNonNegative(strSum)}
		for i := 0; i < n; i++ {
			stream.buckets[i] = clampNonNegative(str[i])
			plain.buckets[i] = clampNonNegative(all[i] - str[i])
			plain.count += plain.buckets[i]
			stream.count += stream.buckets[i]
		}
		return plain, stream
	}
	step := 2*n + 2
	durPlain, durStream := half(0)
	ttfbPlain, ttfbStream := half(step)
	return durPlain, durStream, ttfbPlain, ttfbStream
}

func latencySnapshotFrom(key latencyKey, dur, ttfb latencyHalf) latencySnapshot {
	n := len(latencyBucketBounds)
	return latencySnapshot{
		key:              key,
		ttfbBuckets:      ttfb.buckets[:n],
		durationBuckets:  dur.buckets[:n],
		ttfbSum:          float64(ttfb.sumMs) / 1000,
		durationSum:      float64(dur.sumMs) / 1000,
		ttfbCount:        ttfb.count,
		ttfbOverflow:     ttfb.buckets[n],
		durationCount:    dur.count,
		durationOverflow: dur.buckets[n],
	}
}

type latencyScope struct {
	kind string
	id   string
	key  latencyKey
}

// fleetLatencySnapshots reads every provider's (and, with model labels, every
// catalogued model's) latency counters. ok is false when the store read fails.
func (g *Gateway) fleetLatencySnapshots() ([]latencySnapshot, bool) {
	snaps := g.registry.snapshot()
	includeModel := g.cfg.Metrics != nil && g.cfg.Metrics.ModelLabel
	scopes := make([]latencyScope, 0, len(snaps))
	for _, s := range snaps {
		scopes = append(scopes, latencyScope{kind: kindProvider, id: s.name, key: latencyKey{provider: s.name}})
	}
	if includeModel {
		for _, s := range snaps {
			for _, model := range s.models {
				scopes = append(scopes, latencyScope{kind: kindModel, id: providerModelScopeID(s.name, model), key: latencyKey{provider: s.name, model: model}})
			}
		}
	}
	bucket := bucketFor(g.limiter.now(), windowDay)
	keys := make([]string, 0, len(scopes)*latencyHistKeys*2)
	for _, sc := range scopes {
		keys = append(keys, latencyBlockKeys(sc.kind, sc.id, bucket)...)
	}
	if len(keys) == 0 {
		return nil, true
	}
	vals, ok := g.limiter.chunkedGetMulti(keys, usageModelsChunkKeys)
	if !ok {
		return nil, false
	}
	out := make([]latencySnapshot, 0, len(scopes)*2)
	block := 2 * latencyHistKeys
	for i, sc := range scopes {
		durPlain, durStream, ttfbPlain, ttfbStream := splitLatencyBlock(vals[i*block : (i+1)*block])
		plainKey, streamKey := sc.key, sc.key
		streamKey.streaming = true
		out = append(out, latencySnapshotFrom(plainKey, durPlain, ttfbPlain), latencySnapshotFrom(streamKey, durStream, ttfbStream))
	}
	return out, true
}

// fleetPromptCache reads the model-scope prompt-cache counters for the whole
// catalog. uncached is the model's input tokens minus read and write.
func (g *Gateway) fleetPromptCache() (map[promptCacheKey]int64, bool) {
	ids := g.catalogModelScopeIDs()
	bucket := bucketFor(g.limiter.now(), windowMonth)
	const perID = 3
	keys := make([]string, 0, len(ids)*perID)
	for _, id := range ids {
		for _, metric := range [...]string{metricTokIn, metricPcRead, metricPcWrite} {
			keys = append(keys, windowKeyForBucket(kindModel, id, metric, windowMonth, bucket))
		}
	}
	out := make(map[promptCacheKey]int64)
	if len(keys) == 0 {
		return out, true
	}
	vals, ok := g.limiter.chunkedGetMulti(keys, usageModelsChunkKeys)
	if !ok {
		return nil, false
	}
	for i, id := range ids {
		provider, model, _ := strings.Cut(id, "/")
		tokIn, read, write := vals[i*perID], vals[i*perID+1], vals[i*perID+2]
		if read > 0 {
			out[promptCacheKey{provider, model, promptCacheKindRead}] = read
		}
		if write > 0 {
			out[promptCacheKey{provider, model, promptCacheKindWrite}] = write
		}
		if rest := tokIn - read - write; rest > 0 {
			out[promptCacheKey{provider, model, promptCacheKindUncached}] = rest
		}
	}
	return out, true
}

// provenanceEntries returns the month-window shared counters for one
// provenance observation; they ride accountWith's own batch.
func provenanceEntries(provider, provenance string, tokens int64, monthBucket string) []counterIncr {
	out := []counterIncr{{key: provenanceKeyFor(provider, metricProvenanceReq, provenance, monthBucket), delta: 1, ttl: monthWindowTTL, enforceTTL: enforceTTLMonthWindow}}
	if tokens > 0 {
		out = append(out, counterIncr{key: provenanceKeyFor(provider, metricProvenanceTok, provenance, monthBucket), delta: tokens, ttl: monthWindowTTL, enforceTTL: enforceTTLMonthWindow})
	}
	return out
}

func provenanceKeyFor(provider, metric, provenance, bucket string) string {
	return windowKeyForBucket(kindProvenance, provider, metric+routerKeySep+provenance, windowMonth, bucket)
}

// fleetProvenance reads the shared provenance counters for every provider.
func (g *Gateway) fleetProvenance() ([]provenanceSnapshot, bool) {
	snaps := g.registry.snapshot()
	bucket := bucketFor(g.limiter.now(), windowMonth)
	keys := make([]string, 0, len(snaps)*len(provenanceOutcomes)*2)
	for _, s := range snaps {
		for _, p := range provenanceOutcomes {
			keys = append(keys, provenanceKeyFor(s.name, metricProvenanceReq, p, bucket), provenanceKeyFor(s.name, metricProvenanceTok, p, bucket))
		}
	}
	if len(keys) == 0 {
		return nil, true
	}
	vals, ok := g.limiter.chunkedGetMulti(keys, usageModelsChunkKeys)
	if !ok {
		return nil, false
	}
	var out []provenanceSnapshot
	pos := 0
	for _, s := range snaps {
		for _, p := range provenanceOutcomes {
			reqs, toks := vals[pos], vals[pos+1]
			pos += 2
			if reqs > 0 {
				out = append(out, provenanceSnapshot{key: provenanceKey{provider: s.name, provenance: p}, requests: reqs, tokens: toks})
			}
		}
	}
	return out, true
}

// fleetRejections reads the shared month rejection counters for every user
// and group.
func (g *Gateway) fleetRejections() ([]rejectionSnapshot, bool) {
	userSummaries, groupSummaries := g.auth.snapshot()
	scopes := make([]limitScope, 0, len(userSummaries)+len(groupSummaries))
	for _, us := range userSummaries {
		scopes = append(scopes, limitScope{kind: "user", id: us.name})
	}
	for _, gs := range groupSummaries {
		scopes = append(scopes, limitScope{kind: "group", id: gs.name})
	}
	if len(scopes) == 0 {
		return nil, true
	}
	now := g.limiter.now()
	keys := make([]string, len(scopes))
	for i, sc := range scopes {
		keys[i] = windowKey(sc.kind, sc.id, metricRej, windowMonth, now)
	}
	vals, ok := g.limiter.chunkedGetMulti(keys, usageModelsChunkKeys)
	if !ok {
		return nil, false
	}
	var out []rejectionSnapshot
	for i, sc := range scopes {
		if vals[i] > 0 {
			out = append(out, rejectionSnapshot{kind: sc.kind, id: sc.id, count: vals[i]})
		}
	}
	return out, true
}

func (g *Gateway) writeFleetRejectionMetrics(m *metricWriter) {
	snaps, ok := g.fleetRejections()
	if !ok {
		return
	}
	sort.Slice(snaps, func(i, j int) bool {
		if snaps[i].kind != snaps[j].kind {
			return snaps[i].kind < snaps[j].kind
		}
		return snaps[i].id < snaps[j].id
	})
	m.family("llmgateway_rate_limit_rejections_total", "counter",
		"Total requests refused by a rate or budget limit this UTC month (resets at the UTC month start), by scope."+aggregationNoteStoreBacked)
	for _, s := range snaps {
		m.sampleInt("llmgateway_rate_limit_rejections_total",
			[]metricLabel{{"scope_kind", s.kind}, {"scope_id", s.id}}, s.count)
	}
}
