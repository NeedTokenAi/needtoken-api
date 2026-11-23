package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	var creditedCode string
	universalGiftCardCreditFn = func(userId int, quota int, cardCode string) error {
		creditedUID = userId
		creditedQuota = quota
		creditedCode = cardCode
		return nil
	}

	handled, q, err = tryRedeemUniversalGiftCard(context.Background(), " ntgc-abcd-1234 ", 7)
	require.NoError(t, err)
	assert.True(t, handled)
	assert.Equal(t, 5000000, q)
	assert.Equal(t, 7, creditedUID)
	assert.Equal(t, 5000000, creditedQuota)
	assert.Equal(t, "NTGC-ABCD-1234", creditedCode)
	assert.Equal(t, []string{needTokenUniversalGiftCardClaimPath, needTokenUniversalGiftCardConfirmPath}, calls)
	assert.NotEmpty(t, claimBody)
	assert.NotEmpty(t, claimSig)
	assert.NotEmpty(t, claimTS)

	// 3. Local credit error MUST trigger Release so card is never stranded.
	calls = nil
	lastReleaseReason = ""
	universalGiftCardCreditFn = func(userId int, quota int, cardCode string) error {
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
