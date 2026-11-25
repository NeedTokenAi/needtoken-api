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

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
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
