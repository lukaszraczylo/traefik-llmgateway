package traefikllmgateway

import (
	"encoding/json"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func tpmUsage(t *testing.T, limits *LimitsConfig) adminUsageEntryView {
	t.Helper()
	h, gw := exemptGateway(t, nil, limits, nil)
	require.Equal(t, http.StatusOK, memberLimitsChat(t, h, "sk-a").Code)
	return findUsageEntry(t, gw.buildAdminUsage().Users, "a")
}

func TestAdminUsage_TokensPerMinuteLive(t *testing.T) {
	e := tpmUsage(t, &LimitsConfig{TokensPerMinute: 1000})
	assert.GreaterOrEqual(t, e.TokensPerMinute, int64(1), "minute token counter must surface")
	raw, err := json.Marshal(e)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	assert.Contains(t, m, "tokensPerMinute")
}

func TestAdminUsage_TokensPerMinuteZeroWithoutLimit(t *testing.T) {
	e := tpmUsage(t, &LimitsConfig{RequestsPerDay: 100})
	assert.EqualValues(t, 0, e.TokensPerMinute)
}

func TestAdminUsageChunk_FitsKeyBudget(t *testing.T) {
	assert.LessOrEqual(t, adminUsageChunkScopes*usageKeysPerScope, usageModelsChunkKeys)
	assert.Equal(t, 13, usageKeysPerScope)
}
