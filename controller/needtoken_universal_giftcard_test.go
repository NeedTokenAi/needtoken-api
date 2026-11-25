package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestTryRedeemUniversalGiftCard_SuccessAndRollbackRelease(t *testing.T) {
	const testSecret = "0123456789abcdef0123456789abcdef0123456789abcdef"

	var calls []string
	var lastReleaseReason string
	var claimBody []byte
	var claimSig, claimTS string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		require.NoError(t, err)
		calls = append(calls, r.URL.Path)

		ts := r.Header.Get("X-NeedToken-Timestamp")
		sig := r.Header.Get("X-NeedToken-Signature")
		expected := computeUniversalGiftCardSignature([]byte(testSecret), ts, r.URL.Path, raw)
		require.Equal(t, expected, sig, "HMAC signature must match")

		switch r.URL.Path {
		case needTokenUniversalGiftCardClaimPath:
			claimBody = raw
			claimSig = sig
			claimTS = ts
			var req universalGiftCardClaimReq
			require.NoError(t, common.Unmarshal(raw, &req))
			out, _ := common.Marshal(universalGiftCardClaimResp{
				OK:           true,
				CardID:       "ugc_1",
				BatchNo:      "UNI-2026",
				Name:         "NeedToken Universal $10",
				Code:         req.Code,
				ClaimID:      req.ClaimID,
				FaceUSDCents: 1000,
			})
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(out)
		case needTokenUniversalGiftCardConfirmPath:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		case needTokenUniversalGiftCardReleasePath:
			var req universalGiftCardReleaseReq
			require.NoError(t, common.Unmarshal(raw, &req))
			lastReleaseReason = req.Reason
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	oldQPU := common.QuotaPerUnit
	oldCreditFn := universalGiftCardCreditFn
	oldClient := universalGiftCardHTTPClient
	common.QuotaPerUnit = 500000
	universalGiftCardHTTPClient = srv.Client()
	t.Cleanup(func() {
		common.QuotaPerUnit = oldQPU
		universalGiftCardCreditFn = oldCreditFn
		universalGiftCardHTTPClient = oldClient
	})

	// 1. Unconfigured returns handled=false without calling Glue.
	t.Setenv("NEEDTOKEN_GLUE_URL", "")
	t.Setenv("GLUE_PUBLIC_URL", "")
	t.Setenv("NEEDTOKEN_LINKING_SECRET", "")
	handled, q, err := tryRedeemUniversalGiftCard(context.Background(), "NTGC-1111", 7)
	require.NoError(t, err)
	assert.False(t, handled)
	assert.Equal(t, 0, q)
	assert.Empty(t, calls)

	// 2. Success path: Claim -> Credit -> Confirm.
	t.Setenv("NEEDTOKEN_GLUE_URL", srv.URL)
	t.Setenv("NEEDTOKEN_LINKING_SECRET", testSecret)
	var creditedUID, creditedQuota int
	var creditedClaimID, creditedCode string
	universalGiftCardCreditFn = func(_ context.Context, userId int, quota int, claimID, cardCode string) error {
		creditedUID = userId
		creditedQuota = quota
		creditedClaimID = claimID
		creditedCode = cardCode
		return nil
	}

	handled, q, err = tryRedeemUniversalGiftCard(context.Background(), " ntgc-abcd-1234 ", 7)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, 5000000, q)
	assert.Equal(t, 7, creditedUID)
	assert.Equal(t, 5000000, creditedQuota)
	assert.True(t, strings.HasPrefix(creditedClaimID, "na_"), "claim_id must be threaded into idempotent credit ledger")
	assert.Equal(t, "NTGC-ABCD-1234", creditedCode)
	assert.Equal(t, []string{needTokenUniversalGiftCardClaimPath, needTokenUniversalGiftCardConfirmPath}, calls)
	assert.NotEmpty(t, claimBody)
	assert.NotEmpty(t, claimSig)
	assert.NotEmpty(t, claimTS)

	// 3. Local credit error MUST trigger Release so card is never stranded.
	calls = nil
	lastReleaseReason = ""
	universalGiftCardCreditFn = func(_ context.Context, userId int, quota int, claimID, cardCode string) error {
		return errors.New("wallet limit exceeded")
	}
	handled, q, err = tryRedeemUniversalGiftCard(context.Background(), "NTGC-ABCD-1234", 7)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Equal(t, 0, q)
	assert.Equal(t, []string{needTokenUniversalGiftCardClaimPath, needTokenUniversalGiftCardReleasePath}, calls)
	assert.Equal(t, "newapi_wallet_credit_failed", lastReleaseReason)

	// 4. Quota overflow MUST be rejected by WalletQuotaFromDecimalStrict and release claim.
	calls = nil
	lastReleaseReason = ""
	common.QuotaPerUnit = float64(common.MaxWalletQuota)
	handled, q, err = tryRedeemUniversalGiftCard(context.Background(), "NTGC-ABCD-1234", 7)
	require.Error(t, err)
	assert.True(t, handled)
	assert.Equal(t, 0, q)
	assert.Equal(t, []string{needTokenUniversalGiftCardClaimPath, needTokenUniversalGiftCardReleasePath}, calls)
	assert.Equal(t, "quota_conversion_overflow", lastReleaseReason)
}

func TestAddRedemptionDisabledReturns410Gone(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/redemption/", strings.NewReader(`{"name":"legacy","count":1,"quota":500000}`))
	c.Request.Header.Set("Content-Type", "application/json")

	AddRedemption(c)

	require.Equal(t, http.StatusGone, w.Code)
	assert.Contains(t, w.Body.String(), "single_side_redemption_generation_disabled")
}

func TestAdminGetWalletCreditFulfillment_FoundAndNotFound(t *testing.T) {
	prevDB, prevLogDB := model.DB, model.LOG_DB
	prevMain, prevLog := common.MainDatabaseType(), common.LogDatabaseType()
	prevRedis := common.RedisEnabled
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB, model.LOG_DB = db, db
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.ExternalGrant{}, &model.Log{}))
	t.Cleanup(func() {
		model.DB, model.LOG_DB = prevDB, prevLogDB
		common.SetDatabaseTypes(prevMain, prevLog)
		common.RedisEnabled = prevRedis
		_ = sqlDB.Close()
	})

	u := &model.User{Username: "ugc-audit", Password: "x", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", Quota: 0}
	require.NoError(t, db.Create(u).Error)
	_, err = model.ApplyExternalWalletCredit(context.Background(), u.Id, 5000000, "ugc_claim:na_claim_audit_1", "Universal gift card")
	require.NoError(t, err)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.GET("/api/subscription/admin/wallet-credits/:operation_id", func(c *gin.Context) {
		if c.GetHeader("X-Test-PAT") == "1" {
			c.Set("access_token_id", 1)
		}
		AdminGetWalletCreditFulfillment(c)
	})

	// 1. Without PAT auth -> rejected 403.
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/subscription/admin/wallet-credits/ugc_claim:na_claim_audit_1", nil))
	require.Equal(t, http.StatusForbidden, w.Code)

	// 2. Existing wallet credit op -> 200 with exact user_id & quota.
	req := httptest.NewRequest(http.MethodGet, "/api/subscription/admin/wallet-credits/ugc_claim:na_claim_audit_1", nil)
	req.Header.Set("X-Test-PAT", "1")
	w = httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var resp struct {
		Success bool                `json:"success"`
		Data    model.ExternalGrant `json:"data"`
	}
	require.NoError(t, common.Unmarshal(w.Body.Bytes(), &resp))
	assert.True(t, resp.Success)
	assert.Equal(t, "ugc_claim:na_claim_audit_1", resp.Data.OperationId)
	assert.Equal(t, u.Id, resp.Data.UserId)
	assert.Equal(t, 5000000, resp.Data.Quota)
	assert.Equal(t, "succeeded", resp.Data.State)

	// 3. Missing op -> 404.
	reqMiss := httptest.NewRequest(http.MethodGet, "/api/subscription/admin/wallet-credits/ugc_claim:not_found", nil)
	reqMiss.Header.Set("X-Test-PAT", "1")
	wMiss := httptest.NewRecorder()
	r.ServeHTTP(wMiss, reqMiss)
	require.Equal(t, http.StatusNotFound, wMiss.Code)
}

func TestAdminFulfillmentCapabilitiesAndQuotaSummaryExposeRoutableAutoGroups(t *testing.T) {
	prevDB, prevLogDB := model.DB, model.LOG_DB
	prevMain, prevLog := common.MainDatabaseType(), common.LogDatabaseType()
	prevRedis := common.RedisEnabled
	prevAutoGroups := setting.AutoGroups2JsonString()
	prevUsableGroups := setting.UserUsableGroups2JSONString()
	prevRatios := ratio_setting.GroupRatio2JSONString()

	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	model.DB, model.LOG_DB = db, db
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.SubscriptionPlan{}, &model.UserSubscription{}, &model.Token{}))
	t.Cleanup(func() {
		model.DB, model.LOG_DB = prevDB, prevLogDB
		common.SetDatabaseTypes(prevMain, prevLog)
		common.RedisEnabled = prevRedis
		_ = setting.UpdateAutoGroupsByJsonString(prevAutoGroups)
		_ = setting.UpdateUserUsableGroupsByJSONString(prevUsableGroups)
		_ = ratio_setting.UpdateGroupRatioByJSONString(prevRatios)
		_ = sqlDB.Close()
	})

	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["default","unpriced"]`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip-only":1}`))

	now := time.Now().Unix()
	u := &model.User{Username: "auto-route-user", Password: "x", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", Quota: 100}
	require.NoError(t, db.Create(u).Error)
	plan := &model.SubscriptionPlan{Title: "VIP Plan", AllowedGroups: "vip-only", TotalAmount: 1000, Enabled: true}
	require.NoError(t, db.Create(plan).Error)
	sub := &model.UserSubscription{
		UserId: u.Id, PlanId: plan.Id, AllowedGroups: "vip-only",
		AmountTotal: 1000, StartTime: now - 60, EndTime: now + 3600, Status: "active",
	}
	require.NoError(t, db.Create(sub).Error)
	tok := &model.Token{
		UserId: u.Id, Key: "tok-auto-route-1", Name: "Managed Key", Group: "auto",
		Status: common.TokenStatusEnabled, UnlimitedQuota: true, ExpiredTime: now + 3600,
	}
	require.NoError(t, db.Create(tok).Error)

	gin.SetMode(gin.TestMode)
	r := gin.New()
	withPAT := func(h gin.HandlerFunc) gin.HandlerFunc {
		return func(c *gin.Context) {
			c.Set("access_token_id", 1)
			c.Set("access_token_scopes", []string{"billing:write"})
			h(c)
		}
	}
	r.GET("/api/subscription/admin/capabilities", withPAT(AdminGetFulfillmentCapabilities))
	r.GET("/api/subscription/admin/users/:id/summary", withPAT(AdminGetCustomerQuotaSummary))

	// 1. Capabilities only lists global auto groups that have configured ratios.
	wCap := httptest.NewRecorder()
	r.ServeHTTP(wCap, httptest.NewRequest(http.MethodGet, "/api/subscription/admin/capabilities", nil))
	require.Equal(t, http.StatusOK, wCap.Code)
	var capResp struct {
		Success bool `json:"success"`
		Data    struct {
			AutoGroups         []string `json:"auto_groups"`
			RoutableAutoGroups []string `json:"routable_auto_groups"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(wCap.Body.Bytes(), &capResp))
	assert.True(t, capResp.Success)
	assert.Equal(t, []string{"default", "unpriced"}, capResp.Data.AutoGroups)
	assert.Equal(t, []string{"default"}, capResp.Data.RoutableAutoGroups)

	// 2. When vip-only is missing from global AutoGroups, auto key cannot route to it.
	wSum1 := httptest.NewRecorder()
	r.ServeHTTP(wSum1, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/subscription/admin/users/%d/summary", u.Id), nil))
	require.Equal(t, http.StatusOK, wSum1.Code)
	var sumResp struct {
		Success bool                       `json:"success"`
		Data    model.CustomerQuotaSummary `json:"data"`
	}
	require.NoError(t, common.Unmarshal(wSum1.Body.Bytes(), &sumResp))
	assert.Equal(t, []string{"default"}, sumResp.Data.EffectiveAutoGroups)
	require.Len(t, sumResp.Data.TokenCaps, 1)
	assert.Equal(t, "auto", sumResp.Data.TokenCaps[0].Group)
	assert.Equal(t, []string{"default"}, sumResp.Data.TokenCaps[0].EffectiveAutoGroups)
	require.Len(t, sumResp.Data.Subscriptions, 1)
	assert.Equal(t, "vip-only", sumResp.Data.Subscriptions[0].AllowedGroups)

	// 3. Adding vip-only to global AutoGroups makes it routable on the auto key.
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["vip-only","default"]`))
	wSum2 := httptest.NewRecorder()
	r.ServeHTTP(wSum2, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/subscription/admin/users/%d/summary", u.Id), nil))
	require.Equal(t, http.StatusOK, wSum2.Code)
	sumResp = struct {
		Success bool                       `json:"success"`
		Data    model.CustomerQuotaSummary `json:"data"`
	}{}
	require.NoError(t, common.Unmarshal(wSum2.Body.Bytes(), &sumResp))
	assert.Equal(t, []string{"vip-only", "default"}, sumResp.Data.EffectiveAutoGroups)
	require.Len(t, sumResp.Data.TokenCaps, 1)
	assert.Equal(t, []string{"vip-only", "default"}, sumResp.Data.TokenCaps[0].EffectiveAutoGroups)
}
