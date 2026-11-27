package service

import (
	"context"
	"errors"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	rktypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

// Claude Sonnet-style tiered expression: standard vs long-context
const sonnetTieredExpr = `p <= 200000 ? tier("standard", p * 1.5 + c * 7.5) : tier("long_context", p * 3 + c * 11.25)`

// Simple flat expression
const flatExpr = `tier("default", p * 2 + c * 10)`

// Expression with cache tokens
const cacheExpr = `tier("default", p * 2 + c * 10 + cr * 0.2 + cc * 2.5 + cc1h * 4)`

// Expression with request probes
const probeExpr = `param("service_tier") == "fast" ? tier("fast", p * 4 + c * 20) : tier("normal", p * 2 + c * 10)`

const testQuotaPerUnit = 500_000.0

func makeSnapshot(expr string, groupRatio float64, estPrompt, estCompletion int) *billingexpr.BillingSnapshot {
	return &billingexpr.BillingSnapshot{
		BillingMode:               "tiered_expr",
		ExprString:                expr,
		ExprHash:                  billingexpr.ExprHashString(expr),
		GroupRatio:                groupRatio,
		EstimatedPromptTokens:     estPrompt,
		EstimatedCompletionTokens: estCompletion,
		QuotaPerUnit:              testQuotaPerUnit,
	}
}

func makeRelayInfo(expr string, groupRatio float64, estPrompt, estCompletion int) *relaycommon.RelayInfo {
	snap := makeSnapshot(expr, groupRatio, estPrompt, estCompletion)
	cost, trace, _ := billingexpr.RunExpr(expr, billingexpr.TokenParams{P: float64(estPrompt), C: float64(estCompletion)})
	quotaBeforeGroup := cost / 1_000_000 * testQuotaPerUnit
	snap.EstimatedQuotaBeforeGroup = quotaBeforeGroup
	snap.EstimatedQuotaAfterGroup = billingexpr.QuotaRound(quotaBeforeGroup * groupRatio)
	snap.EstimatedTier = trace.MatchedTier
	return &relaycommon.RelayInfo{
		TieredBillingSnapshot: snap,
		FinalPreConsumedQuota: snap.EstimatedQuotaAfterGroup,
	}
}

// ---------------------------------------------------------------------------
// Existing tests (preserved)
// ---------------------------------------------------------------------------

func TestTryTieredSettleUsesFrozenRequestInput(t *testing.T) {
	exprStr := `param("service_tier") == "fast" ? tier("fast", p * 2) : tier("normal", p)`
	relayInfo := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:               "tiered_expr",
			ExprString:                exprStr,
			ExprHash:                  billingexpr.ExprHashString(exprStr),
			GroupRatio:                1.0,
			EstimatedPromptTokens:     100,
			EstimatedCompletionTokens: 0,
			EstimatedQuotaAfterGroup:  50,
			QuotaPerUnit:              testQuotaPerUnit,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"service_tier":"fast"}`),
		},
	}

	ok, quota, result := TryTieredSettle(relayInfo, billingexpr.TokenParams{P: 100})
	if !ok {
		t.Fatal("expected tiered settle to apply")
	}
	// fast: p*2 = 200; quota = 200 / 1M * 500K = 100
	if quota != 100 {
		t.Fatalf("quota = %d, want 100", quota)
	}
	if result == nil || result.MatchedTier != "fast" {
		t.Fatalf("matched tier = %v, want fast", result)
	}
}

func TestTryTieredSettleFallsBackToFrozenPreConsumeOnExprError(t *testing.T) {
	relayInfo := &relaycommon.RelayInfo{
		FinalPreConsumedQuota: 321,
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:              "tiered_expr",
			ExprString:               `invalid +-+ expr`,
			ExprHash:                 billingexpr.ExprHashString(`invalid +-+ expr`),
			GroupRatio:               1.0,
			EstimatedQuotaAfterGroup: 123,
		},
	}

	ok, quota, result := TryTieredSettle(relayInfo, billingexpr.TokenParams{P: 100})
	if !ok {
		t.Fatal("expected tiered settle to apply")
	}
	if quota != 321 {
		t.Fatalf("quota = %d, want 321", quota)
	}
	if result != nil {
		t.Fatalf("result = %#v, want nil", result)
	}
}

// ---------------------------------------------------------------------------
// Pre-consume vs Post-consume consistency
// ---------------------------------------------------------------------------

func TestTryTieredSettle_PreConsumeMatchesPostConsume(t *testing.T) {
	info := makeRelayInfo(flatExpr, 1.0, 1000, 500)
	params := billingexpr.TokenParams{P: 1000, C: 500}

	ok, quota, _ := TryTieredSettle(info, params)
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// p*2 + c*10 = 7000; quota = 7000 / 1M * 500K = 3500
	if quota != 3500 {
		t.Fatalf("quota = %d, want 3500", quota)
	}
	if quota != info.FinalPreConsumedQuota {
		t.Fatalf("pre-consume %d != post-consume %d", info.FinalPreConsumedQuota, quota)
	}
}

func TestTryTieredSettle_PostConsumeOverPreConsume(t *testing.T) {
	info := makeRelayInfo(flatExpr, 1.0, 1000, 500)
	preConsumed := info.FinalPreConsumedQuota // 3500

	// Actual usage is higher than estimated
	params := billingexpr.TokenParams{P: 2000, C: 1000}
	ok, quota, _ := TryTieredSettle(info, params)
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// p*2 + c*10 = 14000; quota = 14000 / 1M * 500K = 7000
	if quota != 7000 {
		t.Fatalf("quota = %d, want 7000", quota)
	}
	if quota <= preConsumed {
		t.Fatalf("expected supplement: actual %d should > pre-consumed %d", quota, preConsumed)
	}
}

func TestTryTieredSettle_PostConsumeUnderPreConsume(t *testing.T) {
	info := makeRelayInfo(flatExpr, 1.0, 1000, 500)
	preConsumed := info.FinalPreConsumedQuota // 3500

	// Actual usage is lower than estimated
	params := billingexpr.TokenParams{P: 100, C: 50}
	ok, quota, _ := TryTieredSettle(info, params)
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// p*2 + c*10 = 700; quota = 700 / 1M * 500K = 350
	if quota != 350 {
		t.Fatalf("quota = %d, want 350", quota)
	}
	if quota >= preConsumed {
		t.Fatalf("expected refund: actual %d should < pre-consumed %d", quota, preConsumed)
	}
}

// ---------------------------------------------------------------------------
// Tiered boundary conditions
// ---------------------------------------------------------------------------

func TestTryTieredSettle_ExactBoundary(t *testing.T) {
	info := makeRelayInfo(sonnetTieredExpr, 1.0, 200000, 1000)

	// p == 200000 => standard tier (p <= 200000)
	ok, quota, result := TryTieredSettle(info, billingexpr.TokenParams{P: 200000, C: 1000})
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// standard: p*1.5 + c*7.5 = 307500; quota = 307500 / 1M * 500K = 153750
	if quota != 153750 {
		t.Fatalf("quota = %d, want 153750", quota)
	}
	if result.MatchedTier != "standard" {
		t.Fatalf("tier = %s, want standard", result.MatchedTier)
	}
}

func TestTryTieredSettle_BoundaryPlusOne(t *testing.T) {
	info := makeRelayInfo(sonnetTieredExpr, 1.0, 200000, 1000)

	// p == 200001 => crosses to long_context tier
	ok, quota, result := TryTieredSettle(info, billingexpr.TokenParams{P: 200001, C: 1000})
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// long_context: p*3 + c*11.25 = 611253; quota = round(611253 / 1M * 500K) = 305627
	if quota != 305627 {
		t.Fatalf("quota = %d, want 305627", quota)
	}
	if result.MatchedTier != "long_context" {
		t.Fatalf("tier = %s, want long_context", result.MatchedTier)
	}
	if !result.CrossedTier {
		t.Fatal("expected CrossedTier = true")
	}
}

func TestTryTieredSettle_ZeroTokens(t *testing.T) {
	info := makeRelayInfo(flatExpr, 1.0, 0, 0)

	ok, quota, result := TryTieredSettle(info, billingexpr.TokenParams{P: 0, C: 0})
	if !ok {
		t.Fatal("expected tiered settle")
	}
	if quota != 0 {
		t.Fatalf("quota = %d, want 0", quota)
	}
	if result == nil {
		t.Fatal("result should not be nil")
	}
}

func TestTryTieredSettle_HugeTokens(t *testing.T) {
	info := makeRelayInfo(flatExpr, 1.0, 10000000, 5000000)

	ok, quota, _ := TryTieredSettle(info, billingexpr.TokenParams{P: 10000000, C: 5000000})
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// p*2 + c*10 = 70000000; quota = 70000000 / 1M * 500K = 35000000
	if quota != 35000000 {
		t.Fatalf("quota = %d, want 35000000", quota)
	}
}

func TestTryTieredSettle_CacheTokensAffectSettlement(t *testing.T) {
	info := makeRelayInfo(cacheExpr, 1.0, 1000, 500)

	// Without cache tokens
	ok1, quota1, _ := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
	if !ok1 {
		t.Fatal("expected tiered settle")
	}
	// p*2 + c*10 = 7000; quota = 7000 / 1M * 500K = 3500

	// With cache tokens
	ok2, quota2, _ := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500, CR: 10000, CC: 5000, CC1h: 2000})
	if !ok2 {
		t.Fatal("expected tiered settle")
	}
	// 2000 + 5000 + 2000 + 12500 + 8000 = 29500; quota = 29500 / 1M * 500K = 14750

	if quota2 <= quota1 {
		t.Fatalf("cache tokens should increase quota: without=%d, with=%d", quota1, quota2)
	}
	if quota1 != 3500 {
		t.Fatalf("no-cache quota = %d, want 3500", quota1)
	}
	if quota2 != 14750 {
		t.Fatalf("cache quota = %d, want 14750", quota2)
	}
}

// ---------------------------------------------------------------------------
// Request probe tests
// ---------------------------------------------------------------------------

func TestTryTieredSettle_RequestProbeInfluencesBilling(t *testing.T) {
	info := makeRelayInfo(probeExpr, 1.0, 1000, 500)
	info.BillingRequestInput = &billingexpr.RequestInput{
		Body: []byte(`{"service_tier":"fast"}`),
	}

	ok, quota, result := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// fast: p*4 + c*20 = 14000; quota = 14000 / 1M * 500K = 7000
	if quota != 7000 {
		t.Fatalf("quota = %d, want 7000", quota)
	}
	if result.MatchedTier != "fast" {
		t.Fatalf("tier = %s, want fast", result.MatchedTier)
	}
}

func TestTryTieredSettle_NoRequestInput_FallsBackToDefault(t *testing.T) {
	info := makeRelayInfo(probeExpr, 1.0, 1000, 500)
	// No BillingRequestInput set — param("service_tier") returns nil, not "fast"

	ok, quota, result := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// normal: p*2 + c*10 = 7000; quota = 7000 / 1M * 500K = 3500
	if quota != 3500 {
		t.Fatalf("quota = %d, want 3500", quota)
	}
	if result.MatchedTier != "normal" {
		t.Fatalf("tier = %s, want normal", result.MatchedTier)
	}
}

// ---------------------------------------------------------------------------
// Group ratio tests
// ---------------------------------------------------------------------------

type recordingBillingSettler struct {
	preConsumedQuota int
	reserveTargets   []int
}

func (*recordingBillingSettler) Settle(int) error { return nil }

func (*recordingBillingSettler) Refund(*gin.Context) {}

func (*recordingBillingSettler) NeedsRefund() bool { return false }

func (s *recordingBillingSettler) GetPreConsumedQuota() int {
	return s.preConsumedQuota
}

func (s *recordingBillingSettler) Reserve(targetQuota int) error {
	s.reserveTargets = append(s.reserveTargets, targetQuota)
	if targetQuota > s.preConsumedQuota {
		s.preConsumedQuota = targetQuota
	}
	return nil
}

func (s *recordingBillingSettler) ReserveBeforeSubmit(targetQuota int) error {
	return s.Reserve(targetQuota)
}

func TestPrepareTieredBillingForSelectedGroupUpdatesReservation(t *testing.T) {
	const expr = `tier("base", p)`
	billing := &recordingBillingSettler{preConsumedQuota: 50_000}
	relayInfo := &relaycommon.RelayInfo{
		Billing:               billing,
		FinalPreConsumedQuota: 50_000,
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:               "tiered_expr",
			ExprString:                expr,
			ExprHash:                  billingexpr.ExprHashString(expr),
			GroupRatio:                0.10,
			EstimatedQuotaBeforeGroup: 500_000,
			EstimatedQuotaAfterGroup:  50_000,
			QuotaPerUnit:              testQuotaPerUnit,
		},
		PriceData: types.PriceData{
			GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 0.20},
		},
	}

	require.Nil(t, PrepareTieredBillingForSelectedGroup(nil, relayInfo))
	require.Equal(t, []int{100_000}, billing.reserveTargets)
	assert.Equal(t, 100_000, billing.preConsumedQuota)
	assert.Equal(t, 100_000, relayInfo.FinalPreConsumedQuota)
	assert.Equal(t, 0.20, relayInfo.TieredBillingSnapshot.GroupRatio)
	assert.Equal(t, 100_000, relayInfo.TieredBillingSnapshot.EstimatedQuotaAfterGroup)
}

func TestPrepareTieredBillingForSelectedGroupStartsBillingAfterFreeGroup(t *testing.T) {
	truncate(t)
	gin.SetMode(gin.TestMode)

	const userID = 700
	seedUser(t, userID, 500_000)

	relayInfo := &relaycommon.RelayInfo{
		UserId:          userID,
		IsPlayground:    true,
		ForcePreConsume: true,
		OriginModelName: "gpt-test",
		UserSetting: dto.UserSetting{
			BillingPreference: "wallet_only",
		},
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:               "tiered_expr",
			ExprString:                `tier("base", p)`,
			ExprHash:                  billingexpr.ExprHashString(`tier("base", p)`),
			GroupRatio:                0,
			EstimatedQuotaBeforeGroup: 500_000,
			QuotaPerUnit:              testQuotaPerUnit,
		},
		PriceData: types.PriceData{
			FreeModel:      true,
			GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 0.20},
		},
	}
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)

	require.Nil(t, PrepareTieredBillingForSelectedGroup(ctx, relayInfo))
	require.NotNil(t, relayInfo.Billing)
	assert.False(t, relayInfo.PriceData.FreeModel, "FreeModel must be cleared after switching to a paid group")
	assert.Equal(t, 100_000, relayInfo.FinalPreConsumedQuota)
	assert.Equal(t, 0.20, relayInfo.TieredBillingSnapshot.GroupRatio)
	assert.Equal(t, 100_000, relayInfo.TieredBillingSnapshot.EstimatedQuotaAfterGroup)

	userQuota, err := model.GetUserQuota(userID, false)
	require.NoError(t, err)
	assert.Equal(t, 400_000, userQuota)
}

func TestPrepareTieredBillingForSelectedGroupPaidToFreeKeepsFreeModelFalse(t *testing.T) {
	const expr = `tier("base", p)`
	billing := &recordingBillingSettler{preConsumedQuota: 50_000}
	relayInfo := &relaycommon.RelayInfo{
		Billing:               billing,
		FinalPreConsumedQuota: 50_000,
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:               "tiered_expr",
			ExprString:                expr,
			ExprHash:                  billingexpr.ExprHashString(expr),
			GroupRatio:                0.10,
			EstimatedQuotaBeforeGroup: 500_000,
			EstimatedQuotaAfterGroup:  50_000,
			QuotaPerUnit:              testQuotaPerUnit,
		},
		PriceData: types.PriceData{
			GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 0},
		},
	}

	require.Nil(t, PrepareTieredBillingForSelectedGroup(nil, relayInfo))

	// Pre-consume did happen under the paid group, so FreeModel stays false;
	// settlement already yields 0 for GroupRatio == 0 and the session refunds.
	assert.False(t, relayInfo.PriceData.FreeModel)
	assert.Empty(t, billing.reserveTargets)
	assert.Equal(t, 50_000, relayInfo.FinalPreConsumedQuota)
}

func TestPrepareTieredBillingForSelectedGroupTopUpArrearsAllowsNegativeBalance(t *testing.T) {
	truncate(t)

	const userID = 701
	// Balance covers the initial 50k pre-consume (already deducted before this
	// test's seed) but not the 50k top-up to the more expensive retry group.
	// The top-up must NOT abort the request: the full delta is deducted, the
	// uncovered 30k becomes arrears (negative balance), mirroring how
	// settlement charges a positive delta unconditionally.
	seedUser(t, userID, 20_000)

	relayInfo := &relaycommon.RelayInfo{
		UserId:                userID,
		IsPlayground:          true,
		FinalPreConsumedQuota: 50_000,
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:               "tiered_expr",
			ExprString:                `tier("base", p)`,
			ExprHash:                  billingexpr.ExprHashString(`tier("base", p)`),
			GroupRatio:                0.10,
			EstimatedQuotaBeforeGroup: 500_000,
			EstimatedQuotaAfterGroup:  50_000,
			QuotaPerUnit:              testQuotaPerUnit,
		},
		PriceData: types.PriceData{
			GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 0.20},
		},
	}
	session := &BillingSession{
		relayInfo:        relayInfo,
		funding:          &WalletFunding{userId: userID, consumed: 50_000},
		preConsumedQuota: 50_000,
	}
	relayInfo.Billing = session

	require.Nil(t, PrepareTieredBillingForSelectedGroup(nil, relayInfo))

	// Full reservation recorded; wallet charged the full delta into arrears.
	assert.Equal(t, 100_000, session.GetPreConsumedQuota())
	assert.Equal(t, 100_000, relayInfo.FinalPreConsumedQuota)
	assert.Equal(t, 100_000, relayInfo.TieredBillingSnapshot.EstimatedQuotaAfterGroup)
	userQuota, err := model.GetUserQuota(userID, false)
	require.NoError(t, err)
	assert.Equal(t, -30_000, userQuota)

	// Settlement still reconciles against the full reservation: actual 80k
	// refunds the 20k over-reserve, landing at seed - (actual - initial) = -10k.
	require.NoError(t, session.Settle(80_000))
	userQuota, err = model.GetUserQuota(userID, false)
	require.NoError(t, err)
	assert.Equal(t, -10_000, userQuota)
}

func TestBillingSessionReserveWalletTopUpDecrementsBalance(t *testing.T) {
	truncate(t)

	const userID = 702
	seedUser(t, userID, 500_000)

	relayInfo := &relaycommon.RelayInfo{
		UserId:       userID,
		IsPlayground: true,
	}
	session := &BillingSession{
		relayInfo:        relayInfo,
		funding:          &WalletFunding{userId: userID, consumed: 50_000},
		preConsumedQuota: 50_000,
	}

	require.NoError(t, session.Reserve(100_000))

	assert.Equal(t, 100_000, session.GetPreConsumedQuota())
	assert.Equal(t, 100_000, relayInfo.FinalPreConsumedQuota)
	userQuota, err := model.GetUserQuota(userID, false)
	require.NoError(t, err)
	assert.Equal(t, 450_000, userQuota)
}

func TestTryTieredSettleUsesFinalGroupAfterRetry(t *testing.T) {
	const expr = `tier("base", p)`
	tests := []struct {
		name            string
		finalGroupRatio float64
		wantQuota       int
	}{
		{name: "more expensive final group", finalGroupRatio: 0.20, wantQuota: 100_000},
		{name: "free final group", finalGroupRatio: 0, wantQuota: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			relayInfo := &relaycommon.RelayInfo{
				Billing:               &recordingBillingSettler{preConsumedQuota: 50_000},
				FinalPreConsumedQuota: 50_000,
				TieredBillingSnapshot: &billingexpr.BillingSnapshot{
					BillingMode:               "tiered_expr",
					ExprString:                expr,
					ExprHash:                  billingexpr.ExprHashString(expr),
					GroupRatio:                0.10,
					EstimatedQuotaBeforeGroup: 500_000,
					EstimatedQuotaAfterGroup:  50_000,
					QuotaPerUnit:              testQuotaPerUnit,
				},
				PriceData: types.PriceData{
					GroupRatioInfo: types.GroupRatioInfo{GroupRatio: tt.finalGroupRatio},
				},
			}

			require.Nil(t, PrepareTieredBillingForSelectedGroup(nil, relayInfo))
			ok, quota, result := TryTieredSettle(relayInfo, billingexpr.TokenParams{P: 1_000_000})

			require.True(t, ok)
			require.NotNil(t, result)
			assert.Equal(t, tt.wantQuota, quota)
			assert.Equal(t, tt.finalGroupRatio, relayInfo.TieredBillingSnapshot.GroupRatio)
			assert.Equal(t, tt.wantQuota, relayInfo.TieredBillingSnapshot.EstimatedQuotaAfterGroup)
		})
	}
}

func TestTryTieredSettle_GroupRatioScaling(t *testing.T) {
	info := makeRelayInfo(flatExpr, 1.5, 1000, 500)

	ok, quota, _ := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
	if !ok {
		t.Fatal("expected tiered settle")
	}
	// exprCost = 7000, quotaBeforeGroup = 3500, afterGroup = round(3500 * 1.5) = 5250
	if quota != 5250 {
		t.Fatalf("quota = %d, want 5250", quota)
	}
}

func TestTryTieredSettle_GroupRatioZero(t *testing.T) {
	info := makeRelayInfo(flatExpr, 0, 1000, 500)

	ok, quota, _ := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
	if !ok {
		t.Fatal("expected tiered settle")
	}
	if quota != 0 {
		t.Fatalf("quota = %d, want 0 (group ratio = 0)", quota)
	}
}

// ---------------------------------------------------------------------------
// Ratio mode (negative tests) — TryTieredSettle must return false
// ---------------------------------------------------------------------------

func TestTryTieredSettle_RatioMode_NilSnapshot(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: nil,
	}

	ok, _, _ := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
	if ok {
		t.Fatal("expected TryTieredSettle to return false when snapshot is nil")
	}
}

func TestTryTieredSettle_RatioMode_WrongBillingMode(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "ratio",
			ExprString:  flatExpr,
			ExprHash:    billingexpr.ExprHashString(flatExpr),
			GroupRatio:  1.0,
		},
	}

	ok, _, _ := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
	if ok {
		t.Fatal("expected TryTieredSettle to return false for ratio billing mode")
	}
}

func TestTryTieredSettle_RatioMode_EmptyBillingMode(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "",
			ExprString:  flatExpr,
			ExprHash:    billingexpr.ExprHashString(flatExpr),
			GroupRatio:  1.0,
		},
	}

	ok, _, _ := TryTieredSettle(info, billingexpr.TokenParams{P: 1000, C: 500})
	if ok {
		t.Fatal("expected TryTieredSettle to return false for empty billing mode")
	}
}

// ---------------------------------------------------------------------------
// Fallback tests
// ---------------------------------------------------------------------------

func TestTryTieredSettle_ErrorFallbackToEstimatedQuotaAfterGroup(t *testing.T) {
	info := &relaycommon.RelayInfo{
		FinalPreConsumedQuota: 0,
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:              "tiered_expr",
			ExprString:               `invalid expr!!!`,
			ExprHash:                 billingexpr.ExprHashString(`invalid expr!!!`),
			GroupRatio:               1.0,
			EstimatedQuotaAfterGroup: 999,
		},
	}

	ok, quota, result := TryTieredSettle(info, billingexpr.TokenParams{P: 100})
	if !ok {
		t.Fatal("expected tiered settle to apply")
	}
	// FinalPreConsumedQuota is 0, should fall back to EstimatedQuotaAfterGroup
	if quota != 999 {
		t.Fatalf("quota = %d, want 999", quota)
	}
	if result != nil {
		t.Fatal("result should be nil on error fallback")
	}
}

// ---------------------------------------------------------------------------
// BuildTieredTokenParams: token normalization and ratio parity tests
// ---------------------------------------------------------------------------

func tieredQuota(exprStr string, usage *dto.Usage, isClaudeSemantic bool, groupRatio float64) float64 {
	usedVars := billingexpr.UsedVars(exprStr)
	params := BuildTieredTokenParams(usage, isClaudeSemantic, usedVars)
	cost, _, _ := billingexpr.RunExpr(exprStr, params)
	return cost / 1_000_000 * testQuotaPerUnit * groupRatio
}

func ratioQuota(usage *dto.Usage, isClaudeSemantic bool, modelRatio, completionRatio, cacheRatio, imageRatio, groupRatio float64) float64 {
	dPromptTokens := decimal.NewFromInt(int64(usage.PromptTokens))
	dCacheTokens := decimal.NewFromInt(int64(usage.PromptTokensDetails.CachedTokens))
	dCcTokens := decimal.NewFromInt(int64(usage.PromptTokensDetails.CachedCreationTokens))
	dImgTokens := decimal.NewFromInt(int64(usage.PromptTokensDetails.ImageTokens))
	dCompletionTokens := decimal.NewFromInt(int64(usage.CompletionTokens))
	dModelRatio := decimal.NewFromFloat(modelRatio)
	dCompletionRatio := decimal.NewFromFloat(completionRatio)
	dCacheRatio := decimal.NewFromFloat(cacheRatio)
	dImageRatio := decimal.NewFromFloat(imageRatio)
	dGroupRatio := decimal.NewFromFloat(groupRatio)

	baseTokens := dPromptTokens
	if !isClaudeSemantic {
		baseTokens = baseTokens.Sub(dCacheTokens)
		baseTokens = baseTokens.Sub(dCcTokens)
		baseTokens = baseTokens.Sub(dImgTokens)
	}

	cachedTokensWithRatio := dCacheTokens.Mul(dCacheRatio)
	imageTokensWithRatio := dImgTokens.Mul(dImageRatio)
	promptQuota := baseTokens.Add(cachedTokensWithRatio).Add(imageTokensWithRatio)
	completionQuota := dCompletionTokens.Mul(dCompletionRatio)
	ratio := dModelRatio.Mul(dGroupRatio)

	result := promptQuota.Add(completionQuota).Mul(ratio)
	f, _ := result.Float64()
	return f
}

func TestBuildTieredTokenParams_GPT_WithCache(t *testing.T) {
	usage := &dto.Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 200,
			TextTokens:   800,
		},
	}
	expr := `tier("base", p * 2.5 + c * 15 + cr * 0.25)`
	got := tieredQuota(expr, usage, false, 1.0)
	// P=800, C=500, CR=200 → (800*2.5 + 500*15 + 200*0.25) * 0.5 = 4775
	want := 4775.0
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("quota = %f, want %f", got, want)
	}
}

func TestBuildTieredTokenParams_GPT_NoCacheVar(t *testing.T) {
	usage := &dto.Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 200,
			TextTokens:   800,
		},
	}
	expr := `tier("base", p * 2.5 + c * 15)`
	got := tieredQuota(expr, usage, false, 1.0)
	// No cr → P=1000 (cache stays in P), C=500 → (1000*2.5 + 500*15) * 0.5 = 5000
	want := 5000.0
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("quota = %f, want %f", got, want)
	}
}

func TestBuildTieredTokenParams_GPT_WithImage(t *testing.T) {
	usage := &dto.Usage{
		PromptTokens:     1000,
		CompletionTokens: 500,
		PromptTokensDetails: dto.InputTokenDetails{
			ImageTokens: 200,
			TextTokens:  800,
		},
	}
	expr := `tier("base", p * 2 + c * 8 + img * 2.5)`
	got := tieredQuota(expr, usage, false, 1.0)
	// P=800, C=500, Img=200 → (800*2 + 500*8 + 200*2.5) * 0.5 = 3050
	want := 3050.0
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("quota = %f, want %f", got, want)
	}
}

func TestImageCacheBilling(t *testing.T) {
	const expression = `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 2 + c * 30)`
	for _, tc := range []struct {
		name              string
		details           *dto.CachedTokenDetails
		expression        string
		p, cr, img, imgCR float64
		quota             int
	}{
		{"mixed cache", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200), TextTokens: common.GetPointer(100)}, expression, 300, 100, 400, 200, 4113},
		{"explicit free image cache price", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)}, `tier("standard", p * 5 + cr * 1.25 + img * 8 + img_cr * 0 + c * 30)`, 300, 100, 400, 200, 3913},
		{"explicit zero", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(0)}, expression, 100, 300, 600, 0, 4338},
		{"missing breakdown", nil, expression, 100, 300, 600, 0, 4338},
		{"missing image modality", &dto.CachedTokenDetails{TextTokens: common.GetPointer(100)}, expression, 100, 300, 600, 0, 4338},
		{"negative image count", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(-1)}, expression, 100, 300, 600, 0, 4338},
		{"image count exceeds cache", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(301)}, expression, 100, 300, 600, 0, 4338},
		{"modality sum exceeds cache", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200), TextTokens: common.GetPointer(101)}, expression, 100, 300, 600, 0, 4338},
		{"old expression unchanged", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)}, `p * 5 + cr * 1.25 + img * 8 + c * 30`, 100, 300, 600, 0, 4338},
		{"only image cache separately priced", &dto.CachedTokenDetails{ImageTokens: common.GetPointer(200)}, `p * 5 + img_cr * 2 + c * 30`, 800, 100, 400, 200, 3700},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &dto.Usage{PromptTokens: 1000, CompletionTokens: 100,
				PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 300, ImageTokens: 600, CachedTokensDetails: tc.details}}
			params := BuildTieredTokenParams(usage, false, billingexpr.UsedVars(tc.expression))
			assert.Equal(t, tc.p, params.P)
			assert.Equal(t, tc.cr, params.CR)
			assert.Equal(t, tc.img, params.Img)
			assert.Equal(t, tc.imgCR, params.ImgCR)
			assert.Equal(t, float64(1000), params.Len)
			result, err := billingexpr.ComputeTieredQuota(makeSnapshot(tc.expression, 1, 1000, 100), params)
			require.NoError(t, err)
			assert.Equal(t, tc.quota, result.ActualQuotaAfterGroup)
		})
	}
}

func TestBuildTieredTokenParams_Claude_WithCache(t *testing.T) {
	usage := &dto.Usage{
		PromptTokens:     800,
		CompletionTokens: 500,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 200,
			TextTokens:   800,
		},
	}
	expr := `tier("base", p * 3 + c * 15 + cr * 0.3)`
	got := tieredQuota(expr, usage, true, 1.0)
	// Claude: P=800 (no subtraction), C=500, CR=200 → (800*3 + 500*15 + 200*0.3) * 0.5 = 4980
	want := 4980.0
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("quota = %f, want %f", got, want)
	}
}

func TestBuildTieredTokenParams_GPT_AudioOutput(t *testing.T) {
	usage := &dto.Usage{
		PromptTokens:     1000,
		CompletionTokens: 600,
		CompletionTokenDetails: dto.OutputTokenDetails{
			AudioTokens: 100,
			TextTokens:  500,
		},
	}
	expr := `tier("base", p * 2 + c * 10 + ao * 50)`
	got := tieredQuota(expr, usage, false, 1.0)
	// C=600-100=500, AO=100 → (1000*2 + 500*10 + 100*50) * 0.5 = 6000
	want := 6000.0
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("quota = %f, want %f", got, want)
	}
}

func TestBuildTieredTokenParams_GPT_AudioOutputNoVar(t *testing.T) {
	usage := &dto.Usage{
		PromptTokens:     1000,
		CompletionTokens: 600,
		CompletionTokenDetails: dto.OutputTokenDetails{
			AudioTokens: 100,
			TextTokens:  500,
		},
	}
	expr := `tier("base", p * 2 + c * 10)`
	got := tieredQuota(expr, usage, false, 1.0)
	// No ao → C=600 (audio stays in C) → (1000*2 + 600*10) * 0.5 = 4000
	want := 4000.0
	if math.Abs(got-want) > 0.01 {
		t.Fatalf("quota = %f, want %f", got, want)
	}
}

func TestBuildTieredTokenParams_ParityWithRatio(t *testing.T) {
	// GPT-5.4 prices: input=$2.5, output=$15, cacheRead=$0.25
	// Ratio equivalents: modelRatio=1.25, completionRatio=6, cacheRatio=0.1
	usage := &dto.Usage{
		PromptTokens:     10000,
		CompletionTokens: 2000,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 3000,
			TextTokens:   7000,
		},
	}
	expr := `tier("base", p * 2.5 + c * 15 + cr * 0.25)`

	for _, gr := range []float64{1.0, 1.5, 2.0, 0.5} {
		tq := tieredQuota(expr, usage, false, gr)
		rq := ratioQuota(usage, false, 1.25, 6, 0.1, 0, gr)

		if math.Abs(tq-rq) > 0.01 {
			t.Fatalf("groupRatio=%v: tiered=%f ratio=%f (mismatch)", gr, tq, rq)
		}
	}
}

func TestBuildTieredTokenParams_ParityWithRatio_Image(t *testing.T) {
	// gpt-image-1-mini prices: input=$2, output=$8, image=$2.5
	// Ratio equivalents: modelRatio=1, completionRatio=4, imageRatio=1.25
	usage := &dto.Usage{
		PromptTokens:     5000,
		CompletionTokens: 4000,
		PromptTokensDetails: dto.InputTokenDetails{
			ImageTokens: 1000,
			TextTokens:  4000,
		},
	}
	expr := `tier("base", p * 2 + c * 8 + img * 2.5)`

	tq := tieredQuota(expr, usage, false, 1.0)
	rq := ratioQuota(usage, false, 1.0, 4, 0, 1.25, 1.0)

	if math.Abs(tq-rq) > 0.01 {
		t.Fatalf("tiered=%f ratio=%f (mismatch)", tq, rq)
	}
}

// ---------------------------------------------------------------------------
// BuildTieredTokenParams: Len computation tests
// ---------------------------------------------------------------------------

func TestBuildTieredTokenParams_Len_GPT(t *testing.T) {
	usage := &dto.Usage{
		PromptTokens:     10000,
		CompletionTokens: 2000,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 3000,
			TextTokens:   7000,
		},
	}
	expr := `tier("base", p * 2.5 + c * 15 + cr * 0.25)`
	usedVars := billingexpr.UsedVars(expr)
	params := BuildTieredTokenParams(usage, false, usedVars)

	// Non-Claude: Len = raw PromptTokens
	if params.Len != 10000 {
		t.Fatalf("Len = %f, want 10000 (raw PromptTokens)", params.Len)
	}
	// P should be reduced by cache
	if params.P != 7000 {
		t.Fatalf("P = %f, want 7000 (PromptTokens - CachedTokens)", params.P)
	}
}

func TestBuildTieredTokenParams_Len_Claude(t *testing.T) {
	usage := &dto.Usage{
		PromptTokens:     5000,
		CompletionTokens: 2000,
		UsageSemantic:    "anthropic",
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 3000,
			TextTokens:   5000,
		},
		ClaudeCacheCreation5mTokens: 1000,
		ClaudeCacheCreation1hTokens: 500,
	}
	expr := `tier("base", p * 3 + c * 15 + cr * 0.3 + cc * 3.75 + cc1h * 6)`
	usedVars := billingexpr.UsedVars(expr)
	params := BuildTieredTokenParams(usage, true, usedVars)

	// Claude: Len = PromptTokens + CachedTokens + CacheCreation5m + CacheCreation1h
	wantLen := float64(5000 + 3000 + 1000 + 500)
	if params.Len != wantLen {
		t.Fatalf("Len = %f, want %f (text + cache read + cache creation)", params.Len, wantLen)
	}
	// Claude: P is not reduced (isClaudeUsageSemantic = true)
	if params.P != 5000 {
		t.Fatalf("P = %f, want 5000 (no subtraction for Claude)", params.P)
	}
}

func TestBuildTieredTokenParams_ClaudeUnsplitCacheWrites(t *testing.T) {
	expr := `tier("base", p * 3 + c * 15 + cr * 0.3 + cc * 3.75 + cc1h * 6)`
	for _, tc := range []struct {
		name             string
		split5m, split1h int
		wantCC, wantCC1h float64
	}{
		{"total without split", 0, 0, 10000, 0},
		{"split missing part of total", 0, 4000, 6000, 4000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			usage := &dto.Usage{
				PromptTokens:                100,
				CompletionTokens:            50,
				UsageSemantic:               "anthropic",
				PromptTokensDetails:         dto.InputTokenDetails{CachedCreationTokens: 10000},
				ClaudeCacheCreation5mTokens: tc.split5m,
				ClaudeCacheCreation1hTokens: tc.split1h,
			}
			params := BuildTieredTokenParams(usage, true, billingexpr.UsedVars(expr))
			assert.Equal(t, tc.wantCC, params.CC)
			assert.Equal(t, tc.wantCC1h, params.CC1h)
			assert.Equal(t, float64(100+10000), params.Len, "len counts every cache write")
		})
	}
}

// A long-lived cache write without its own price costs a fixed multiple of the
// short write price: 2x for OpenAI 12h, 6/3.75x for Claude 1h. The short price
// is cc, or p when cc has no price either.
func TestBuildTieredTokenParams_UnpricedLongTTLCacheWrites(t *testing.T) {
	// The OpenAI prompt caching guide example: 12000 input tokens with 4000
	// cache reads and 7000 writes, 3000 of them 30m and 4000 12h.
	openai := dto.Usage{PromptTokens: 12000, PromptTokensDetails: dto.InputTokenDetails{
		CachedTokens: 4000, CacheWriteTokens: 7000,
		CacheWriteTokensDetails: &dto.CacheWriteTokenDetails{TTL30mTokens: 3000, TTL12hTokens: 4000},
	}}
	unsplit := openai
	unsplit.PromptTokensDetails.CacheWriteTokensDetails = nil
	claude := func(split5m, split1h int) dto.Usage {
		return dto.Usage{PromptTokens: 100, UsageSemantic: "anthropic",
			ClaudeCacheCreation5mTokens: split5m, ClaudeCacheCreation1hTokens: split1h,
			PromptTokensDetails: dto.InputTokenDetails{CachedCreationTokens: split5m + split1h}}
	}
	for _, tc := range []struct {
		name               string
		usage              dto.Usage
		expr               string
		p, cc, cc1h, cc12h float64
		cost               float64
	}{
		{"openai 30m and 12h priced", openai, `tier("base", p * 10 + cr * 1 + cc * 12.5 + cc12h * 25)`, 1000, 3000, 0, 4000, 151500},
		{"openai 12h joins cc at twice its count", openai, `tier("base", p * 10 + cr * 1 + cc * 12.5)`, 1000, 11000, 0, 0, 151500},
		{"openai 30m stays in p", openai, `tier("base", p * 10 + cr * 1 + cc12h * 25)`, 4000, 0, 0, 4000, 144000},
		{"openai 12h bills twice in p", openai, `tier("base", p * 10 + cr * 1)`, 12000, 0, 0, 0, 124000},
		{"openai writes without a ttl split", unsplit, `tier("base", p * 10 + cr * 1 + cc * 12.5 + cc12h * 25)`, 1000, 7000, 0, 0, 101500},
		{"claude 1h joins cc at 1.6x", claude(0, 10000), `tier("base", p * 3 + c * 15 + cr * 0.3 + cc * 3.75)`, 100, 16000, 0, 0, 60300},
		{"claude 5m bills as input", claude(10000, 0), `tier("base", p * 3 + c * 15)`, 10100, 0, 0, 0, 30300},
		{"claude 5m bills as input beside priced 1h", claude(2000, 10000), `tier("base", p * 3 + cc1h * 6)`, 2100, 0, 10000, 0, 66300},
		{"claude 1h bills as input at 1.6x", claude(2000, 10000), `tier("base", p * 3)`, 18100, 0, 0, 0, 54300},
	} {
		t.Run(tc.name, func(t *testing.T) {
			isClaude := tc.usage.UsageSemantic == "anthropic"
			params := BuildTieredTokenParams(&tc.usage, isClaude, billingexpr.UsedVars(tc.expr))
			assert.Equal(t, tc.p, params.P)
			assert.Equal(t, tc.cc, params.CC)
			assert.Equal(t, tc.cc1h, params.CC1h)
			assert.Equal(t, tc.cc12h, params.CC12h)
			wantLen := float64(tc.usage.PromptTokens)
			if isClaude {
				wantLen += float64(tc.usage.PromptTokensDetails.CacheCreationTokensTotal())
			}
			assert.Equal(t, wantLen, params.Len, "len keeps the unweighted counts")
			cost, _, err := billingexpr.RunExpr(tc.expr, params)
			require.NoError(t, err)
			assert.Equal(t, tc.cost, cost)
		})
	}
}

func TestBuildTieredTokenParams_Len_TierCondition(t *testing.T) {
	// Test that len-based tier conditions work correctly when p is reduced by cache
	usage := &dto.Usage{
		PromptTokens:     300000,
		CompletionTokens: 5000,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 250000,
			TextTokens:   50000,
		},
	}
	expr := `len <= 200000 ? tier("standard", p * 3 + c * 15 + cr * 0.3) : tier("long_context", p * 6 + c * 22.5 + cr * 0.6)`
	usedVars := billingexpr.UsedVars(expr)
	params := BuildTieredTokenParams(usage, false, usedVars)

	// Len = 300000 (raw prompt), P = 50000 (300000 - 250000 cache)
	if params.Len != 300000 {
		t.Fatalf("Len = %f, want 300000", params.Len)
	}
	if params.P != 50000 {
		t.Fatalf("P = %f, want 50000", params.P)
	}

	// Run expression: len=300000 > 200000, so long_context tier
	cost, trace, err := billingexpr.RunExpr(expr, params)
	if err != nil {
		t.Fatal(err)
	}
	if trace.MatchedTier != "long_context" {
		t.Fatalf("tier = %s, want long_context (len=300000 but p=50000)", trace.MatchedTier)
	}
	// long_context: 50000*6 + 5000*22.5 + 250000*0.6
	wantCost := 50000.0*6 + 5000*22.5 + 250000*0.6
	if math.Abs(cost-wantCost) > 1e-6 {
		t.Fatalf("cost = %f, want %f", cost, wantCost)
	}
}

const complexTieredExpr = `p <= 200000 ? tier("standard", p * 3 + c * 15 + cr * 0.3 + cc * 3.75 + cc1h * 6 + img * 3 + img_o * 30 + ai * 10 + ao * 40) : tier("long_context", p * 6 + c * 22.5 + cr * 0.6 + cc * 7.5 + cc1h * 12 + img * 6 + img_o * 60 + ai * 20 + ao * 80)`

func randomUsage(rng *rand.Rand) *dto.Usage {
	cacheRead := int(rng.Float64() * 50000)
	cacheCreate := int(rng.Float64() * 10000)
	imgIn := int(rng.Float64() * 5000)
	audioIn := int(rng.Float64() * 3000)
	prompt := int(rng.Float64()*300000) + cacheRead + cacheCreate + imgIn + audioIn

	imgOut := int(rng.Float64() * 2000)
	audioOut := int(rng.Float64() * 1000)
	completion := int(rng.Float64()*50000) + imgOut + audioOut

	return &dto.Usage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens:         cacheRead,
			CachedCreationTokens: cacheCreate,
			ImageTokens:          imgIn,
			AudioTokens:          audioIn,
			TextTokens:           prompt - cacheRead - cacheCreate - imgIn - audioIn,
		},
		CompletionTokenDetails: dto.OutputTokenDetails{
			ImageTokens: imgOut,
			AudioTokens: audioOut,
			TextTokens:  completion - imgOut - audioOut,
		},
	}
}

func BenchmarkTieredBilling_ComplexExpr(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	usedVars := billingexpr.UsedVars(complexTieredExpr)
	usages := make([]*dto.Usage, 1000)
	for i := range usages {
		usages[i] = randomUsage(rng)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		usage := usages[i%len(usages)]
		params := BuildTieredTokenParams(usage, false, usedVars)
		billingexpr.RunExpr(complexTieredExpr, params)
	}
}

func BenchmarkRatioBilling_Equivalent(b *testing.B) {
	rng := rand.New(rand.NewSource(42))
	usages := make([]*dto.Usage, 1000)
	for i := range usages {
		usages[i] = randomUsage(rng)
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		usage := usages[i%len(usages)]
		ratioQuota(usage, false, 1.5, 5.0, 0.1, 1.0, 1.5)
	}
}

func BenchmarkTieredBilling_Parallel(b *testing.B) {
	usedVars := billingexpr.UsedVars(complexTieredExpr)

	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(rand.Int63()))
		for pb.Next() {
			usage := randomUsage(rng)
			params := BuildTieredTokenParams(usage, false, usedVars)
			billingexpr.RunExpr(complexTieredExpr, params)
		}
	})
}

func BenchmarkRatioBilling_Parallel(b *testing.B) {
	b.RunParallel(func(pb *testing.PB) {
		rng := rand.New(rand.NewSource(rand.Int63()))
		for pb.Next() {
			usage := randomUsage(rng)
			ratioQuota(usage, false, 1.5, 5.0, 0.1, 1.0, 1.5)
		}
	})
}

func TestSamePriceCacheReadsRemainInInput(t *testing.T) {
	for _, tc := range []struct {
		name        string
		usage       dto.Usage
		extra       string
		input, cost float64
	}{
		{"openai", dto.Usage{PromptTokens: 1000, CompletionTokens: 100, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200}}, "", 1000, 72000},
		{"anthropic", dto.Usage{PromptTokens: 800, CompletionTokens: 100, UsageSemantic: "anthropic", PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200}}, "", 1000, 72000},
		{"anthropic mixed cache TTLs", dto.Usage{PromptTokens: 750, CompletionTokens: 100, UsageSemantic: "anthropic", ClaudeCacheCreation5mTokens: 30, ClaudeCacheCreation1hTokens: 20, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 200, CachedCreationTokens: 50}}, " + cc * 75 + cc1h * 120", 950, 73650},
	} {
		t.Run(tc.name, func(t *testing.T) {
			merged := `tier("base", p * 60 + c * 120` + tc.extra + `)`
			separate := `tier("base", p * 60 + c * 120 + cr * 60` + tc.extra + `)`
			params := BuildTieredTokenParams(&tc.usage, tc.usage.UsageSemantic == "anthropic", billingexpr.UsedVars(merged))
			assert.Equal(t, tc.input, params.P)
			cost, _, err := billingexpr.RunExpr(merged, params)
			require.NoError(t, err)
			assert.Equal(t, tc.cost, cost)
			separateParams := BuildTieredTokenParams(&tc.usage, tc.usage.UsageSemantic == "anthropic", billingexpr.UsedVars(separate))
			previousCost, _, err := billingexpr.RunExpr(separate, separateParams)
			require.NoError(t, err)
			assert.Equal(t, previousCost, cost, "folding cache reads into input must preserve the charge")
			assert.Equal(t, separateParams.Len, params.Len, "tier conditions keep the full context length")
		})
	}
}

func TestBillingSessionRefundFailureRecoveryAndPartialIdempotency(t *testing.T) {
	truncate(t)

	// 1. 精确复现 R06：钱包 100，预扣 50，首次 Refund 遇瞬时数据库错误 ->
	//    NeedsRefund 保持 true，恢复后再次 Refund 余额恢复为 100 且 NeedsRefund=false；第三次 Refund 不多退。
	const walletUID = 801
	seedUser(t, walletUID, 50) // 100 - 50 pre-consumed
	tokWallet := &model.Token{
		Id: 801, UserId: walletUID, Key: "sk-r06-wallet-token", Name: "r06-w",
		RemainQuota: 50, UsedQuota: 50, Status: common.TokenStatusEnabled,
	}
	require.NoError(t, model.DB.Create(tokWallet).Error)

	session := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			RequestId: "req-r06-wallet-1",
			UserId:    walletUID,
			TokenId:   tokWallet.Id,
			TokenKey:  tokWallet.Key,
		},
		funding:          &WalletFunding{userId: walletUID, consumed: 50},
		preConsumedQuota: 50,
		tokenConsumed:    50,
	}

	const failWalletCallback = "r06_inject_wallet_refund_fail"
	failWallet := true
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(failWalletCallback, func(tx *gorm.DB) {
		if failWallet && tx.Statement.Table == "users" {
			_ = tx.AddError(errors.New("transient database failure on user quota refund"))
		}
	}))
	t.Cleanup(func() { _ = model.DB.Callback().Update().Remove(failWalletCallback) })

	session.Refund(nil)
	assert.True(t, session.NeedsRefund(), "failed refund must keep NeedsRefund=true for retry")
	q1, err := model.GetUserQuota(walletUID, true)
	require.NoError(t, err)
	assert.Equal(t, 50, q1, "balance stays 50 during transient failure")

	failWallet = false
	session.Refund(nil)
	assert.False(t, session.NeedsRefund(), "recovered refund marks session completed")
	q2, err := model.GetUserQuota(walletUID, true)
	require.NoError(t, err)
	assert.Equal(t, 100, q2, "balance must recover to 100 after second Refund call")

	var reloadedTok model.Token
	require.NoError(t, model.DB.First(&reloadedTok, tokWallet.Id).Error)
	assert.Equal(t, 100, reloadedTok.RemainQuota)

	// 第三次调用幂等不重复退额
	session.Refund(nil)
	q3, err := model.GetUserQuota(walletUID, true)
	require.NoError(t, err)
	assert.Equal(t, 100, q3)

	// 2. 部分成功恢复：钱包退款已成功，令牌退款遇瞬时 DB 故障 -> 恢复后钱包绝不二次增额，仅补退令牌额度。
	const partialUID = 802
	require.NoError(t, model.DB.Create(&model.User{Id: partialUID, Username: "r06_partial", AffCode: "aff_r06_p", Quota: 50, Status: common.UserStatusEnabled}).Error)
	tokPartial := &model.Token{
		Id: 802, UserId: partialUID, Key: "sk-r06-partial-token", Name: "r06-p",
		RemainQuota: 50, UsedQuota: 50, Status: common.TokenStatusEnabled,
	}
	require.NoError(t, model.DB.Create(tokPartial).Error)

	partialSession := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			RequestId: "req-r06-partial-1",
			UserId:    partialUID,
			TokenId:   tokPartial.Id,
			TokenKey:  tokPartial.Key,
		},
		funding:          &WalletFunding{userId: partialUID, consumed: 50},
		preConsumedQuota: 50,
		tokenConsumed:    50,
	}
	failToken := true
	const failTokenCallback = "r06_inject_token_refund_fail"
	require.NoError(t, model.DB.Callback().Update().Before("gorm:update").Register(failTokenCallback, func(tx *gorm.DB) {
		if failToken && tx.Statement.Table == "tokens" {
			_ = tx.AddError(errors.New("transient database failure on token refund"))
		}
	}))
	t.Cleanup(func() { _ = model.DB.Callback().Update().Remove(failTokenCallback) })

	partialSession.Refund(nil)
	assert.True(t, partialSession.NeedsRefund())
	qp1, err := model.GetUserQuota(partialUID, true)
	require.NoError(t, err)
	assert.Equal(t, 100, qp1, "wallet step committed once")

	// Simulate process restart and background recovery via RecoverPendingBillingRefunds
	failToken = false
	require.NoError(t, model.DB.Model(&model.NeedTokenBillingRefund{}).
		Where("operation_id = ?", partialSession.operationId).
		Update("next_retry_at", time.Now().Unix()-1).Error)

	recovered, err := RecoverPendingBillingRefunds(context.Background(), 20)
	require.NoError(t, err)
	assert.Equal(t, 1, recovered)
	qp2, err := model.GetUserQuota(partialUID, true)
	require.NoError(t, err)
	assert.Equal(t, 100, qp2, "wallet must NEVER be credited twice on partial recovery")
	var reloadedTokPartial model.Token
	require.NoError(t, model.DB.First(&reloadedTokPartial, tokPartial.Id).Error)
	assert.Equal(t, 100, reloadedTokPartial.RemainQuota)

	// 3. 订阅资金 + 补预扣(ExtraReserved) + 令牌，同时开启 BatchUpdateEnabled=true 验证退额直接落 DB 不丢。
	prevBatch := common.BatchUpdateEnabled
	common.BatchUpdateEnabled = true
	t.Cleanup(func() { common.BatchUpdateEnabled = prevBatch })

	const subUID = 803
	require.NoError(t, model.DB.Create(&model.User{Id: subUID, Username: "r06_sub", AffCode: "aff_r06_s", Quota: 10, Status: common.UserStatusEnabled}).Error)
	tokSub := &model.Token{
		Id: 803, UserId: subUID, Key: "sk-r06-sub-token", Name: "r06-s",
		RemainQuota: 100, UsedQuota: 0, Status: common.TokenStatusEnabled,
	}
	require.NoError(t, model.DB.Create(tokSub).Error)
	now := time.Now().Unix()
	sub := &model.UserSubscription{
		Id: 803, UserId: subUID, PlanId: 1, Status: "active", AllowedGroups: "default",
		AmountTotal: 1000, AmountUsed: 100, StartTime: now - 100, EndTime: now + 3600,
	}
	require.NoError(t, model.DB.Create(sub).Error)
	require.NoError(t, model.DB.Create(&model.SubscriptionPreConsumeRecord{
		RequestId: "req-r06-sub-1", UserId: subUID, UserSubscriptionId: sub.Id,
		PreConsumed: 40, Status: "consumed",
	}).Error)
	// Also pre-deduct 20 extra reserved + 60 token consumed (total 100 on sub, 60 on token)
	require.NoError(t, model.DB.Model(tokSub).Updates(map[string]any{"remain_quota": 40, "used_quota": 60}).Error)

	subSession := &BillingSession{
		relayInfo: &relaycommon.RelayInfo{
			RequestId:      "req-r06-sub-1",
			UserId:         subUID,
			TokenId:        tokSub.Id,
			TokenKey:       tokSub.Key,
			SubscriptionId: sub.Id,
		},
		funding: &SubscriptionFunding{
			requestId:      "req-r06-sub-1",
			userId:         subUID,
			subscriptionId: sub.Id,
			preConsumed:    40,
		},
		preConsumedQuota: 60,
		tokenConsumed:    60,
		extraReserved:    20,
	}

	subSession.Refund(nil)
	assert.False(t, subSession.NeedsRefund())
	var reloadedSub model.UserSubscription
	require.NoError(t, model.DB.First(&reloadedSub, sub.Id).Error)
	assert.Equal(t, int64(40), reloadedSub.AmountUsed, "both initial 40 + extra 20 refunded from 100 -> 40")
	var reloadedTokSub model.Token
	require.NoError(t, model.DB.First(&reloadedTokSub, tokSub.Id).Error)
	assert.Equal(t, 100, reloadedTokSub.RemainQuota, "token quota refunded directly to DB even with BatchUpdateEnabled=true")
}

func TestHybridSubscriptionWalletPreConsumeSettleAndRefund(t *testing.T) {
	truncate(t)
	now := time.Now().Unix()
	newTestGinCtx := func() *gin.Context {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
		return c
	}

	// 1. 60 套餐 + 40 现金可以承担 100 预扣，并正确退款还原。
	plan := &model.SubscriptionPlan{
		Title:   "Hybrid Test Plan",
		Enabled: true,
	}
	require.NoError(t, model.DB.Create(plan).Error)
	const u1 = 811
	seedUser(t, u1, 40)
	require.NoError(t, model.DB.Create(&model.Token{Id: 811, UserId: u1, Key: "sk-r08-1", Name: "r08-1", RemainQuota: 1000, Status: common.TokenStatusEnabled}).Error)
	sub1 := &model.UserSubscription{
		Id: 811, UserId: u1, PlanId: plan.Id, Status: "active", AllowedGroups: "default", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 40, StartTime: now - 100, EndTime: now + 3600,
	}
	require.NoError(t, model.DB.Create(sub1).Error)

	info1 := &relaycommon.RelayInfo{
		RequestId: "req-r08-1", UserId: u1, TokenId: 811, TokenKey: "sk-r08-1", UsingGroup: "default",
		ForcePreConsume: true,
		UserSetting:     dto.UserSetting{BillingPreference: "subscription_first"},
	}
	sess1, apiErr := NewBillingSession(newTestGinCtx(), info1, 100)
	require.Nil(t, apiErr, "60 sub + 40 wallet must cover 100 pre-consume")
	assert.Equal(t, BillingSourceSubscription, info1.BillingSource)
	assert.Equal(t, int64(60), info1.SubscriptionPreConsumed)
	assert.Equal(t, int64(40), info1.SubscriptionWalletOverflow)

	var afterUser1 model.User
	require.NoError(t, model.DB.First(&afterUser1, u1).Error)
	assert.Equal(t, 0, afterUser1.Quota)
	var afterSub1 model.UserSubscription
	require.NoError(t, model.DB.First(&afterSub1, sub1.Id).Error)
	assert.Equal(t, int64(100), afterSub1.AmountUsed)

	// 失败退款必须同时恢复 60 套餐与 40 钱包。
	sess1.Refund(nil)
	assert.False(t, sess1.NeedsRefund())
	require.NoError(t, model.DB.First(&afterUser1, u1).Error)
	assert.Equal(t, 40, afterUser1.Quota)
	require.NoError(t, model.DB.First(&afterSub1, sub1.Id).Error)
	assert.Equal(t, int64(40), afterSub1.AmountUsed)

	// 2. 钱包本身足够 100 时，也必须优先用完 60 套餐 + 40 钱包，实际 70 结算时先退 30 现金。
	const u2 = 812
	require.NoError(t, model.DB.Create(&model.User{Id: u2, Username: "r08_u2", AffCode: "aff_r08_2", Quota: 100, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 812, UserId: u2, Key: "sk-r08-2", Name: "r08-2", RemainQuota: 1000, Status: common.TokenStatusEnabled}).Error)
	sub2 := &model.UserSubscription{
		Id: 812, UserId: u2, PlanId: plan.Id, Status: "active", AllowedGroups: "default", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 40, StartTime: now - 100, EndTime: now + 3600,
	}
	require.NoError(t, model.DB.Create(sub2).Error)

	info2 := &relaycommon.RelayInfo{
		RequestId: "req-r08-2", UserId: u2, TokenId: 812, TokenKey: "sk-r08-2", UsingGroup: "default",
		ForcePreConsume: true,
		UserSetting:     dto.UserSetting{BillingPreference: "subscription_first"},
	}
	sess2, apiErr2 := NewBillingSession(newTestGinCtx(), info2, 100)
	require.Nil(t, apiErr2)
	assert.Equal(t, BillingSourceSubscription, info2.BillingSource)
	assert.Equal(t, int64(60), info2.SubscriptionPreConsumed)
	require.NoError(t, sess2.Settle(70))
	var afterUser2 model.User
	require.NoError(t, model.DB.First(&afterUser2, u2).Error)
	assert.Equal(t, 90, afterUser2.Quota, "net wallet charge=10 (60 sub + 10 wallet = 70), leaving 90 in wallet")
	var afterSub2 model.UserSubscription
	require.NoError(t, model.DB.First(&afterSub2, sub2.Id).Error)
	assert.Equal(t, int64(100), afterSub2.AmountUsed)
	assert.Equal(t, int64(10), info2.SubscriptionWalletOverflow)

	// 3. 合计不足（60 套餐 + 30 钱包 请求 100）必须整体失败，零残留。
	const u3 = 813
	require.NoError(t, model.DB.Create(&model.User{Id: u3, Username: "r08_u3", AffCode: "aff_r08_3", Quota: 30, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 813, UserId: u3, Key: "sk-r08-3", Name: "r08-3", RemainQuota: 1000, Status: common.TokenStatusEnabled}).Error)
	sub3 := &model.UserSubscription{
		Id: 813, UserId: u3, PlanId: plan.Id, Status: "active", AllowedGroups: "default", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 40, StartTime: now - 100, EndTime: now + 3600,
	}
	require.NoError(t, model.DB.Create(sub3).Error)
	info3 := &relaycommon.RelayInfo{
		RequestId: "req-r08-3", UserId: u3, TokenId: 813, TokenKey: "sk-r08-3", UsingGroup: "default",
		ForcePreConsume: true,
		UserSetting:     dto.UserSetting{BillingPreference: "subscription_first"},
	}
	_, apiErr3 := NewBillingSession(newTestGinCtx(), info3, 100)
	require.NotNil(t, apiErr3)
	assert.Equal(t, rktypes.ErrorCodeInsufficientUserQuota, apiErr3.GetErrorCode())
	var afterUser3 model.User
	require.NoError(t, model.DB.First(&afterUser3, u3).Error)
	assert.Equal(t, 30, afterUser3.Quota)
	var afterSub3 model.UserSubscription
	require.NoError(t, model.DB.First(&afterSub3, sub3.Id).Error)
	assert.Equal(t, int64(40), afterSub3.AmountUsed)

	// 4. subscription_only 即使钱包有 100 也不得擅自扣现金。
	info4 := &relaycommon.RelayInfo{
		RequestId: "req-r08-4", UserId: u2, TokenId: 812, TokenKey: "sk-r08-2", UsingGroup: "default",
		ForcePreConsume: true,
		UserSetting:     dto.UserSetting{BillingPreference: "subscription_only"},
	}
	require.NoError(t, model.DB.Model(&model.UserSubscription{}).Where("id = ?", sub2.Id).Update("amount_used", 40).Error)
	_, apiErr4 := NewBillingSession(newTestGinCtx(), info4, 100)
	require.NotNil(t, apiErr4)
	assert.Equal(t, rktypes.ErrorCodeInsufficientUserQuota, apiErr4.GetErrorCode())
	require.NoError(t, model.DB.First(&afterUser2, u2).Error)
	assert.Equal(t, 90, afterUser2.Quota, "subscription_only must never deduct wallet")
}

func TestBillingShortfallPersistenceAndLogAccounting(t *testing.T) {
	truncate(t)
	now := time.Now().Unix()

	// R09 精确复现场景：套餐剩 15、钱包剩 15、预扣 10、实际费用 60 -> 实收 30、未收 30 持久落库可查且钱包不为负。
	plan := &model.SubscriptionPlan{
		Title:   "Shortfall Test Plan",
		Enabled: true,
	}
	require.NoError(t, model.DB.Create(plan).Error)
	const uid = 821
	require.NoError(t, model.DB.Create(&model.User{Id: uid, Username: "r09_user", AffCode: "aff_r09_1", Quota: 15, Status: common.UserStatusEnabled}).Error)
	require.NoError(t, model.DB.Create(&model.Token{Id: 821, UserId: uid, Key: "sk-r09-1", Name: "r09-1", RemainQuota: 1000, Status: common.TokenStatusEnabled}).Error)
	sub := &model.UserSubscription{
		Id: 821, UserId: uid, PlanId: plan.Id, Status: "active", AllowedGroups: "default", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 85, StartTime: now - 100, EndTime: now + 3600,
	}
	require.NoError(t, model.DB.Create(sub).Error)

	info := &relaycommon.RelayInfo{
		RequestId: "req-r09-1", UserId: uid, TokenId: 821, TokenKey: "sk-r09-1",
		ChannelMeta:     &relaycommon.ChannelMeta{ChannelId: 9},
		OriginModelName: "gpt-5.4", UsingGroup: "default", ForcePreConsume: true,
		UserSetting: dto.UserSetting{BillingPreference: "subscription_first"},
	}
	ginCtx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ginCtx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	sess, apiErr := NewBillingSession(ginCtx, info, 10)
	require.Nil(t, apiErr)
	require.NoError(t, sess.Settle(60))

	var uAfter model.User
	require.NoError(t, model.DB.First(&uAfter, uid).Error)
	assert.Equal(t, 0, uAfter.Quota, "wallet clamped at 0 and must not go negative")
	var subAfter model.UserSubscription
	require.NoError(t, model.DB.First(&subAfter, sub.Id).Error)
	assert.Equal(t, int64(100), subAfter.AmountUsed)

	// 持久化差异可查询
	res, err := model.QueryBillingShortfalls(context.Background(), model.BillingShortfallFilter{UserId: uid})
	require.NoError(t, err)
	require.Equal(t, int64(1), res.Total)
	require.Len(t, res.Items, 1)
	assert.Equal(t, int64(60), res.Items[0].ActualQuota)
	assert.Equal(t, int64(15), res.Items[0].SubscriptionCharged)
	assert.Equal(t, int64(15), res.Items[0].WalletCharged)
	assert.Equal(t, int64(30), res.Items[0].CollectedQuota)
	assert.Equal(t, int64(30), res.Items[0].UnbilledQuota)
	assert.Equal(t, "wallet_exhausted", res.Items[0].Reason)
	assert.Equal(t, int64(30), res.TotalUnbilledQuota)

	// 消费日志 other 字段同步口径可解释
	other := model.NewLogOther()
	appendBillingInfo(info, other)
	otherJSON := other.JSONString()
	assert.Contains(t, otherJSON, `"collected_quota":30`)
	assert.Contains(t, otherJSON, `"unbilled_quota":30`)
	assert.Contains(t, otherJSON, `"actual_quota_expected":60`)
}
