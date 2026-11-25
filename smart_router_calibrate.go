package traefikllmgateway

import "sync"

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
	mu    sync.Mutex
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
		c.mu.Lock()
		st := c.stats[smartCalibKey{provider: provider, model: model}]
		ratio, ready := 0.0, false
		if st != nil && st.samples >= smartCalibMinSamples {
			ratio, ready = st.ratio, true
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
