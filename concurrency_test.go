package traefikllmgateway

import (
	"net/http"
	"testing"
)

// TestConcurrencyLearner_OverloadStaysBelowCeiling pins that an overload
// observation on an unknown limit never learns a value at or above the
// ceiling, however many requests were in flight.
func TestConcurrencyLearner_OverloadStaysBelowCeiling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		inFlight int
	}{
		{name: "one in flight", inFlight: 1},
		{name: "typical load", inFlight: 8},
		{name: "exactly at ceiling", inFlight: 2 * learnedCeiling},
		{name: "far above ceiling", inFlight: 1000},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			c := newConcurrencyLearner(0)
			for range tt.inFlight {
				_ = c.begin("p")
			}
			c.observe("p", &http.Response{StatusCode: http.StatusTooManyRequests}, nil)
			got := c.limit("p")
			if got < 1 || got > learnedCeiling-1 {
				t.Errorf("limit = %d, want in [1, %d]", got, learnedCeiling-1)
			}
		})
	}
}
