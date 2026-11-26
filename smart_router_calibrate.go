package traefikllmgateway

import (
	"sync"
	"time"
)

const (
	// smartDefaultBytesPerToken is the uncalibrated estimate.
	smartDefaultBytesPerToken = 4
	// smartCalibMinSamples keeps a few odd requests from steering the estimate.
	smartCalibMinSamples = 50
	// smartCalibMinBytes skips small prompts, where fixed provider overhead dominates the token count.
	smartCalibMinBytes = 256
	// smartCalibMinRatio and smartCalibMaxRatio bound the learned bytes per token against bad usage data.
	smartCalibMinRatio = 1.5
	smartCalibMaxRatio = 6.0
	// smartCalibAlpha weights the newest sample; 0.1 follows drift within tens of requests.
	smartCalibAlpha = 0.1
	// smartCalibMaxModels bounds the map; models come from config and discovery, so it is a safety cap.
	smartCalibMaxModels = 1024

	// Fleet counters per model and day: prompt bytes, prompt tokens and sample count.
	calibKind        = "cal"
	calibMetricBytes = "b"
	calibMetricToken = "t"
	calibMetricCount = "n"
	// calibFleetDays is how many day buckets the fleet ratio spans, so it does not reset at midnight.
	calibFleetDays = 2
)

type smartCalibKey struct {
	provider, model string
}

type smartCalibStat struct {
	ratio   float64
	samples int
}

// smartTokenCalibrator learns bytes per prompt token for each upstream model
// from reported usage. It keeps two numbers per model and no request data.
type smartTokenCalibrator struct {
	stats map[smartCalibKey]*smartCalibStat
	// fleet holds the ratios the router-learning loop read from the shared counters; nil without sharedState.
	fleet map[smartCalibKey]smartCalibStat
	count func([]counterIncr)
	now   func() time.Time
	mu    sync.Mutex
}

// share makes observe also add to the fleet counters through count. Call it before the calibrator serves requests.
func (c *smartTokenCalibrator) share(count func([]counterIncr), now func() time.Time) {
	c.count, c.now = count, now
	c.fleet = make(map[smartCalibKey]smartCalibStat)
}

func (c *smartTokenCalibrator) sharing() bool {
	return c != nil && c.count != nil
}

// setFleet replaces the shared ratios with those of the latest learn refresh.
func (c *smartTokenCalibrator) setFleet(m map[smartCalibKey]smartCalibStat) {
	c.mu.Lock()
	c.fleet = m
	c.mu.Unlock()
}

func calibFleetKey(k smartCalibKey, metric, bucket string) string {
	return windowKeyForBucket(calibKind, k.provider+"/"+k.model, metric, windowDay, bucket)
}

// countFleet adds one usable sample to the day's shared counters. Dropped writes only blur an estimate.
func (c *smartTokenCalibrator) countFleet(key smartCalibKey, promptBytes, promptTokens int64) {
	if c.count == nil {
		return
	}
	bucket := bucketFor(c.now(), windowDay)
	c.count([]counterIncr{
		{key: calibFleetKey(key, calibMetricBytes, bucket), delta: promptBytes, ttl: dayWindowTTL, enforceTTL: enforceTTLDayWindow},
		{key: calibFleetKey(key, calibMetricToken, bucket), delta: promptTokens, ttl: dayWindowTTL, enforceTTL: enforceTTLDayWindow},
		{key: calibFleetKey(key, calibMetricCount, bucket), delta: 1, ttl: dayWindowTTL, enforceTTL: enforceTTLDayWindow},
	})
}

func newSmartTokenCalibrator() *smartTokenCalibrator {
	return &smartTokenCalibrator{stats: make(map[smartCalibKey]*smartCalibStat)}
}

// observe records one request's estimated prompt bytes against the upstream's reported prompt tokens.
func (c *smartTokenCalibrator) observe(provider, model string, promptBytes, promptTokens int64) {
	if c == nil || promptTokens <= 0 || promptBytes < smartCalibMinBytes {
		return
	}
	sample := float64(promptBytes) / float64(promptTokens)
	key := smartCalibKey{provider: provider, model: model}
	c.countFleet(key, promptBytes, promptTokens)
	c.mu.Lock()
	defer c.mu.Unlock()
	st := c.stats[key]
	if st == nil {
		if len(c.stats) >= smartCalibMaxModels {
			return
		}
		c.stats[key] = &smartCalibStat{ratio: sample, samples: 1}
		return
	}
	st.ratio += smartCalibAlpha * (sample - st.ratio)
	if st.samples < smartCalibMinSamples {
		st.samples++
	}
}

// promptTokens estimates the prompt tokens for promptBytes on a model: the
// clamped learned ratio once enough samples exist, else one token per four bytes.
func (c *smartTokenCalibrator) promptTokens(provider, model string, promptBytes int64) int64 {
	if c != nil {
		key := smartCalibKey{provider: provider, model: model}
		c.mu.Lock()
		st := c.stats[key]
		ratio, ready := 0.0, false
		if st != nil && st.samples >= smartCalibMinSamples {
			ratio, ready = st.ratio, true
		}
		// The fleet ratio covers every pod's samples, so it wins when it has enough of them.
		if fs, ok := c.fleet[key]; ok && fs.samples >= smartCalibMinSamples {
			ratio, ready = fs.ratio, true
		}
		c.mu.Unlock()
		if ready {
			if ratio < smartCalibMinRatio {
				ratio = smartCalibMinRatio
			}
			if ratio > smartCalibMaxRatio {
				ratio = smartCalibMaxRatio
			}
			tokens := int64(float64(promptBytes)/ratio + 0.999999)
			return tokens
		}
	}
	return (promptBytes + smartDefaultBytesPerToken - 1) / smartDefaultBytesPerToken
}

func (c *smartTokenCalibrator) size() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.stats)
}
