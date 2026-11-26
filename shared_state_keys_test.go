package traefikllmgateway

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func freshBreakerKeys(kind string, ids []string) []string {
	var out []string
	for _, id := range ids {
		out = append(out, sharedBreakerKey(kind, id, "open"), sharedBreakerKey(kind, id, "fails"), sharedBreakerKey(kind, id, "opens"))
	}
	return out
}

func freshInflightKeys(s *sharedInflight, now time.Time) []string {
	e := s.epochOf(now)
	var out []string
	for _, id := range s.ids {
		for i := 0; i < s.epochs; i++ {
			out = append(out, s.epochKey(id, e-int64(i), "s"), s.epochKey(id, e-int64(i), "f"))
		}
		out = append(out, sharedInflightKey(id, "limit"))
	}
	return out
}

func TestSharedBreakerKeys_MatchFreshBuild(t *testing.T) {
	ids := []string{"a", "b", "c"}
	b := newSharedBreaker(nil, sharedKindRequest, ids, 3, time.Second, time.Minute, time.Minute)
	want := freshBreakerKeys(sharedKindRequest, ids)
	assert.Equal(t, want, b.keys(time.Unix(0, 0)))
	assert.Equal(t, want, b.keys(time.Unix(1e9, 0)))
	assert.Equal(t, want, b.keys(time.Unix(0, 0)))
}

func TestSharedInflightKeys_MatchFreshBuild(t *testing.T) {
	s := newSharedInflight(nil, []string{"a", "b"}, 15*time.Minute)
	t0 := time.Unix(1_700_000_000, 0)
	sameEpoch := t0.Add(10 * time.Second)
	assert.Equal(t, s.epochOf(t0), s.epochOf(sameEpoch))
	next := t0.Add(sharedInflightEpoch)
	for _, now := range []time.Time{t0, sameEpoch, t0, next, next, t0, next.Add(time.Hour)} {
		assert.Equal(t, freshInflightKeys(s, now), s.keys(now))
	}
}
