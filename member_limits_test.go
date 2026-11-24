package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const memberLimitsUpstreamBody = `{"id":"c1","choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":30,"completion_tokens":30}}`

// memberLimitsGateway builds a gateway whose upstream answers every chat call
// with 60 total tokens, and pins its limiter clock inside one minute window.
func memberLimitsGateway(t *testing.T, groups map[string]*GroupConfig, users []*UserConfig) (http.Handler, *Gateway) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(memberLimitsUpstreamBody))
	}))
	t.Cleanup(srv.Close)

	cfg := CreateConfig()
	cfg.Admin = &AdminConfig{Enabled: true}
	cfg.Providers = map[string]*ProviderConfig{
		"openai": {Type: "openai", BaseURL: srv.URL, APIKey: "k", Models: []string{"gpt-test"}},
	}
	cfg.Groups = groups
	cfg.Users = &UsersConfig{Inline: users}
	h, gw := newAdminGatewayHandle(t, cfg)
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	gw.limiter.nowFn = func() time.Time { return now }
	return h, gw
}

func memberLimitsChat(t *testing.T, h http.Handler, key string) *httptest.ResponseRecorder {
	t.Helper()
	body := map[string]any{"model": "gpt-test", "messages": []any{map[string]any{"role": "user", "content": "hi"}}}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newUnifiedRequest(t, http.MethodPost, "/v1/chat/completions", key, body))
	return rec
}

func TestMemberLimits_PerMemberCounter(t *testing.T) {
	h, _ := memberLimitsGateway(t,
		map[string]*GroupConfig{"friends": {MemberLimits: &LimitsConfig{RequestsPerMinute: 2}}},
		[]*UserConfig{
			{Name: "a", Group: "friends", APIKey: "sk-a"},
			{Name: "b", Group: "friends", APIKey: "sk-b"},
		})

	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code)
	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code)
	rec := memberLimitsChat(t, h, "sk-a")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.NotEmpty(t, rec.Header().Get("Retry-After"))
	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-b").Code, "user B has its own counter")
}

func TestMemberLimits_GroupAggregateStillEnforced(t *testing.T) {
	h, _ := memberLimitsGateway(t,
		map[string]*GroupConfig{"friends": {
			Limits:       &LimitsConfig{RequestsPerMinute: 3},
			MemberLimits: &LimitsConfig{RequestsPerMinute: 2},
		}},
		[]*UserConfig{
			{Name: "a", Group: "friends", APIKey: "sk-a"},
			{Name: "b", Group: "friends", APIKey: "sk-b"},
		})

	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code)
	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code)
	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-b").Code)
	assert.Equal(t, http.StatusTooManyRequests, memberLimitsChat(t, h, "sk-b").Code, "group aggregate of 3 is spent")
}

func TestMemberLimits_UserExplicitLimitOverrides(t *testing.T) {
	h, _ := memberLimitsGateway(t,
		map[string]*GroupConfig{"friends": {MemberLimits: &LimitsConfig{RequestsPerMinute: 1}}},
		[]*UserConfig{
			{Name: "a", Group: "friends", APIKey: "sk-a", Limits: &LimitsConfig{RequestsPerMinute: 3}},
			{Name: "b", Group: "friends", APIKey: "sk-b"},
		})

	for i := 0; i < 3; i++ {
		assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code, "request %d", i+1)
	}
	assert.Equal(t, http.StatusTooManyRequests, memberLimitsChat(t, h, "sk-a").Code)
	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-b").Code)
	assert.Equal(t, http.StatusTooManyRequests, memberLimitsChat(t, h, "sk-b").Code)
}

func TestMemberLimits_UserLimitSetsOtherFieldOnly(t *testing.T) {
	a, err := newAuthStore(&Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"g": {MemberLimits: &LimitsConfig{RequestsPerMinute: 5, RequestsPerDay: 50}}},
		Users: &UsersConfig{Inline: []*UserConfig{
			{Name: "a", Group: "g", APIKey: "sk-a", Limits: &LimitsConfig{RequestsPerMinute: 9}},
		}},
	})
	require.NoError(t, err)
	u, _, ok := identifyWithKey(a, "sk-a")
	require.True(t, ok)
	assert.EqualValues(t, 9, u.limits.RequestsPerMinute, "user value wins per field")
	assert.EqualValues(t, 50, u.limits.RequestsPerDay, "unset user field falls back to memberLimits")
	assert.Equal(t, map[string]string{"requestsPerMinute": "user", "requestsPerDay": "memberLimits:g"}, u.limitSources)
}

func TestMemberLimits_StrictestAcrossGroups(t *testing.T) {
	a, err := newAuthStore(&Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups: map[string]*GroupConfig{
			"g1": {MemberLimits: &LimitsConfig{RequestsPerMinute: 10, TokensPerDay: 500}},
			"g2": {MemberLimits: &LimitsConfig{RequestsPerMinute: 4, RequestsPerDay: 100}},
			"g3": {},
		},
		Users: &UsersConfig{Inline: []*UserConfig{
			{Name: "a", Group: "g1", Groups: []string{"g2", "g3"}, APIKey: "sk-a"},
		}},
	})
	require.NoError(t, err)
	u, _, ok := identifyWithKey(a, "sk-a")
	require.True(t, ok)
	assert.EqualValues(t, 4, u.limits.RequestsPerMinute, "strictest non-zero value wins")
	assert.EqualValues(t, 100, u.limits.RequestsPerDay, "zero in g1 means unlimited, not strictest")
	assert.EqualValues(t, 500, u.limits.TokensPerDay)
	assert.Equal(t, "memberLimits:g2", u.limitSources["requestsPerMinute"])
	assert.Equal(t, "memberLimits:g1", u.limitSources["tokensPerDay"])
}

func TestMemberLimits_ZeroIsUnlimited(t *testing.T) {
	h, _ := memberLimitsGateway(t,
		map[string]*GroupConfig{"friends": {MemberLimits: &LimitsConfig{}}},
		[]*UserConfig{{Name: "a", Group: "friends", APIKey: "sk-a"}})
	for i := 0; i < 5; i++ {
		assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code)
	}
}

func TestMemberLimits_NoMemberLimitsKeepsUserPointer(t *testing.T) {
	own := &LimitsConfig{RequestsPerMinute: 7}
	a, err := newAuthStore(&Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"g": {}},
		Users:     &UsersConfig{Inline: []*UserConfig{{Name: "a", Group: "g", APIKey: "sk-a", Limits: own}}},
	})
	require.NoError(t, err)
	u, _, ok := identifyWithKey(a, "sk-a")
	require.True(t, ok)
	assert.Same(t, own, u.limits)
}

func TestMemberLimits_UsersFileReloadRecomputesEffectiveLimits(t *testing.T) {
	dir := t.TempDir()
	fp := writeUsersFile(t, dir, "users.json", []*UserConfig{{Name: "f", Group: "friends", APIKey: "sk-f"}})
	a, err := newAuthStore(&Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"friends": {MemberLimits: &LimitsConfig{RequestsPerMinute: 2}}},
		Users:     &UsersConfig{Inline: []*UserConfig{}},
	})
	require.NoError(t, err)
	uf := newUsersFile(fp)
	users, err := uf.load()
	require.NoError(t, err)
	require.NoError(t, a.replaceFileUsers(users))
	u, _, ok := identifyWithKey(a, "sk-f")
	require.True(t, ok)
	assert.EqualValues(t, 2, u.limits.RequestsPerMinute)

	writeUsersDoc(t, fp, []*UserConfig{
		{Name: "f", Group: "friends", APIKey: "sk-f", Limits: &LimitsConfig{RequestsPerMinute: 8}},
		{Name: "g", Group: "friends", APIKey: "sk-g"},
	}, time.Now().Add(time.Hour))
	users, err = uf.load()
	require.NoError(t, err)
	require.NoError(t, a.replaceFileUsers(users))

	u, _, ok = identifyWithKey(a, "sk-f")
	require.True(t, ok)
	assert.EqualValues(t, 8, u.limits.RequestsPerMinute, "reloaded explicit limit wins")
	assert.Equal(t, "user", u.limitSources["requestsPerMinute"])
	u, _, ok = identifyWithKey(a, "sk-g")
	require.True(t, ok)
	assert.EqualValues(t, 2, u.limits.RequestsPerMinute, "new file user inherits memberLimits")
}

func TestMemberLimits_ValidateRejectsNegative(t *testing.T) {
	_, err := newAuthStore(&Config{
		Providers: map[string]*ProviderConfig{"openai": {Type: "openai", APIKey: "k"}},
		Groups:    map[string]*GroupConfig{"g": {MemberLimits: &LimitsConfig{RequestsPerMinute: -1}}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "memberLimits")
	assert.Contains(t, err.Error(), "requestsPerMinute")
}

func TestMemberLimits_AdminUsageShowsEffectiveLimitsAndSource(t *testing.T) {
	h, _ := memberLimitsGateway(t,
		map[string]*GroupConfig{
			"friends": {MemberLimits: &LimitsConfig{RequestsPerMinute: 2, TokensPerMinute: 100}},
			"admins":  {},
		},
		[]*UserConfig{
			{Name: "a", Group: "friends", APIKey: "sk-a", Limits: &LimitsConfig{RequestsPerMinute: 9}},
			{Name: "b", Group: "friends", APIKey: "sk-b"},
			{Name: "root", Group: "admins", APIKey: "sk-root", Admin: true},
		})

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, adminRequest(http.MethodGet, adminUsagePath, "sk-root"))
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var got adminUsageResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got))

	a := findUsageEntry(t, got.Users, "a")
	require.NotNil(t, a.Limits)
	assert.EqualValues(t, 9, a.Limits.RequestsPerMinute)
	assert.EqualValues(t, 100, a.Limits.TokensPerMinute)
	assert.Equal(t, map[string]string{"requestsPerMinute": "user", "tokensPerMinute": "memberLimits:friends"}, a.LimitSources)

	b := findUsageEntry(t, got.Users, "b")
	require.NotNil(t, b.Limits)
	assert.EqualValues(t, 2, b.Limits.RequestsPerMinute)
	assert.Equal(t, "memberLimits:friends", b.LimitSources["requestsPerMinute"])

	root := findUsageEntry(t, got.Users, "root")
	assert.Nil(t, root.Limits)
	assert.Empty(t, root.LimitSources)

	friends := findUsageEntry(t, got.Groups, "friends")
	require.NotNil(t, friends.MemberLimits)
	assert.EqualValues(t, 2, friends.MemberLimits.RequestsPerMinute)
}

func TestTokensPerMinute_BlocksNextRequestAfterExceeding(t *testing.T) {
	now := time.Date(2026, 8, 20, 10, 0, 30, 0, time.UTC)
	l := newLimiter(nil, true)
	l.nowFn = func() time.Time { return now }
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{TokensPerMinute: 100}}}

	require.Nil(t, l.checkAndCount(scopes), "admitted before any tokens are known")
	l.account(scopes, usage{prompt: 60, completion: 50}, 0) // 110 > 100
	v := l.checkAndCount(scopes)
	require.NotNil(t, v, "next request after exceeding must be refused")
	assert.Contains(t, v.message, "tokens-per-minute")
	assert.Equal(t, 30, v.retryAfter)

	now = now.Add(time.Minute)
	assert.Nil(t, l.checkAndCount(scopes), "new minute window passes")
}

func TestTokensPerMinute_ExactlyAtLimitBlocksLikeDay(t *testing.T) {
	l := newLimiter(nil, true)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{TokensPerMinute: 100}}}
	l.account(scopes, usage{prompt: 50, completion: 49}, 0)
	assert.Nil(t, l.checkAndCount(scopes), "99 < 100 passes")
	l.account(scopes, usage{prompt: 1}, 0)
	assert.NotNil(t, l.checkAndCount(scopes), "100 >= 100 blocks")
}

func TestTokensPerMinute_ZeroIsUnlimitedAndWritesNoMinuteCounters(t *testing.T) {
	l := newLimiter(nil, true)
	scopes := []limitScope{{kind: "user", id: "u", limits: &LimitsConfig{RequestsPerDay: 1000}}}
	l.account(scopes, usage{prompt: 1_000_000}, 0)
	assert.Nil(t, l.checkAndCount(scopes))
	got, ok := l.getCounter("user", "u", metricTokIn, windowMin, l.nowFn())
	require.True(t, ok)
	assert.EqualValues(t, 0, got, "no per-minute token counter without a tokensPerMinute limit")
}

func TestTokensPerMinute_ValidateRejectsNegative(t *testing.T) {
	assert.Error(t, (&LimitsConfig{TokensPerMinute: -1}).validate())
	assert.NoError(t, (&LimitsConfig{TokensPerMinute: 1}).validate())
}

func TestTokensPerMinute_EndToEndMemberLimit(t *testing.T) {
	h, _ := memberLimitsGateway(t,
		map[string]*GroupConfig{"friends": {MemberLimits: &LimitsConfig{TokensPerMinute: 100}}},
		[]*UserConfig{
			{Name: "a", Group: "friends", APIKey: "sk-a"},
			{Name: "b", Group: "friends", APIKey: "sk-b"},
		})
	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code) // 60 tokens
	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code) // 120 tokens, admitted at 60
	rec := memberLimitsChat(t, h, "sk-a")
	assert.Equal(t, http.StatusTooManyRequests, rec.Code, rec.Body.String())
	assert.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-b").Code)
}
