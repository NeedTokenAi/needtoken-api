package controller

import (
	"bytes"
	"crypto/hmac"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func makeTestChallengeToken(t *testing.T, secret []byte, claims accountLinkChallengeClaims) string {
	t.Helper()
	raw, err := common.Marshal(claims)
	require.NoError(t, err)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	sig := accountLinkMAC(secret, accountLinkChallengeDomain, payload)
	return accountLinkTokenPrefix + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func runAccountLinkRequest(t *testing.T, handler gin.HandlerFunc, userID, accessTokenID int, body any) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	raw, err := common.Marshal(body)
	require.NoError(t, err)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/user/account-link", bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	if userID > 0 {
		c.Set("id", userID)
	}
	if accessTokenID > 0 {
		c.Set("access_token_id", accessTokenID)
	}
	handler(c)
	return rec
}

func TestNeedTokenAccountLinkPreviewAndApprove(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	origNow := accountLinkNow
	origLoadUser := accountLinkLoadUser
	t.Cleanup(func() {
		accountLinkNow = origNow
		accountLinkLoadUser = origLoadUser
	})
	accountLinkNow = func() time.Time { return now }
	accountLinkLoadUser = func(id int) (*model.User, error) {
		switch id {
		case 42:
			return &model.User{Id: 42, Username: "alice", Status: common.UserStatusEnabled}, nil
		case 77:
			return &model.User{Id: 77, Username: "banned", Status: common.UserStatusDisabled}, nil
		default:
			return nil, nil
		}
	}

	validToken := makeTestChallengeToken(t, []byte(secret), accountLinkChallengeClaims{
		CID:         "lk_test_challenge_12345678",
		Source:      "needtoken-storefront",
		MerchantID:  "needtoken-storefront:user:101",
		MemberLabel: "NeedToken #101",
		ReturnURL:   "https://needtoken.io/account-link/callback",
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(10 * time.Minute).Unix(),
	})

	// Preview succeeds for authenticated browser session.
	rec := runAccountLinkRequest(t, PreviewAccountLinkChallenge, 42, 0, map[string]any{"challenge": validToken})
	require.Equal(t, http.StatusOK, rec.Code)
	var previewResp struct {
		Success bool `json:"success"`
		Data    struct {
			ChallengeID  string `json:"challenge_id"`
			Source       string `json:"source"`
			MemberLabel  string `json:"member_label"`
			ReturnOrigin string `json:"return_origin"`
			ExpiresAt    int64  `json:"expires_at"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &previewResp))
	assert.True(t, previewResp.Success)
	assert.Equal(t, "lk_test_challenge_12345678", previewResp.Data.ChallengeID)
	assert.Equal(t, "NeedToken #101", previewResp.Data.MemberLabel)
	assert.Equal(t, "https://needtoken.io", previewResp.Data.ReturnOrigin)

	// Approve signs a valid domain-separated approval assertion bound to user 42.
	rec = runAccountLinkRequest(t, ApproveAccountLinkChallenge, 42, 0, map[string]any{"challenge": validToken})
	require.Equal(t, http.StatusOK, rec.Code)
	var approveResp struct {
		Success bool `json:"success"`
		Data    struct {
			ChallengeID string `json:"challenge_id"`
			Approval    string `json:"approval"`
			ReturnURL   string `json:"return_url"`
			ExpiresAt   int64  `json:"expires_at"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &approveResp))
	assert.True(t, approveResp.Success)
	assert.Equal(t, "lk_test_challenge_12345678", approveResp.Data.ChallengeID)
	assert.Equal(t, "https://needtoken.io/account-link/callback", approveResp.Data.ReturnURL)

	// Verify signed approval payload and MAC directly.
	rest, ok := strings.CutPrefix(approveResp.Data.Approval, accountLinkTokenPrefix)
	require.True(t, ok)
	payloadB64, sigB64, ok := strings.Cut(rest, ".")
	require.True(t, ok)
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	require.NoError(t, err)
	assert.True(t, hmac.Equal(sig, accountLinkMAC([]byte(secret), accountLinkApprovalDomain, payloadB64)))
	rawClaims, err := base64.RawURLEncoding.DecodeString(payloadB64)
	require.NoError(t, err)
	var approvalClaims accountLinkApprovalClaims
	require.NoError(t, common.Unmarshal(rawClaims, &approvalClaims))
	assert.Equal(t, "lk_test_challenge_12345678", approvalClaims.CID)
	assert.Equal(t, 42, approvalClaims.NativeUserID)
	assert.Equal(t, "alice", approvalClaims.Username)
	assert.NotEmpty(t, approvalClaims.Nonce)
	assert.Equal(t, now.Add(accountLinkApprovalTTL).Unix(), approvalClaims.ExpiresAt)
}

func TestNeedTokenAccountLinkSecurityRejections(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	origNow := accountLinkNow
	origLoadUser := accountLinkLoadUser
	t.Cleanup(func() {
		accountLinkNow = origNow
		accountLinkLoadUser = origLoadUser
	})
	accountLinkNow = func() time.Time { return now }
	accountLinkLoadUser = func(id int) (*model.User, error) {
		if id == 77 {
			return &model.User{Id: 77, Username: "disabled", Status: common.UserStatusDisabled}, nil
		}
		return &model.User{Id: id, Username: "active", Status: common.UserStatusEnabled}, nil
	}

	validToken := makeTestChallengeToken(t, []byte(secret), accountLinkChallengeClaims{
		CID:         "lk_sec_test_12345678",
		Source:      "needtoken-storefront",
		MerchantID:  "needtoken-storefront:user:101",
		MemberLabel: "NeedToken #101",
		ReturnURL:   "https://needtoken.io/account-link/callback",
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(10 * time.Minute).Unix(),
	})

	expiredToken := makeTestChallengeToken(t, []byte(secret), accountLinkChallengeClaims{
		CID:         "lk_expired_12345678",
		Source:      "needtoken-storefront",
		MerchantID:  "needtoken-storefront:user:101",
		MemberLabel: "NeedToken #101",
		ReturnURL:   "https://needtoken.io/account-link/callback",
		IssuedAt:    now.Add(-15 * time.Minute).Unix(),
		ExpiresAt:   now.Add(-5 * time.Minute).Unix(),
	})

	badReturnToken := makeTestChallengeToken(t, []byte(secret), accountLinkChallengeClaims{
		CID:         "lk_badret_12345678",
		Source:      "needtoken-storefront",
		MerchantID:  "needtoken-storefront:user:101",
		MemberLabel: "NeedToken #101",
		ReturnURL:   "https://evil.example/steal",
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(10 * time.Minute).Unix(),
	})

	cases := []struct {
		name          string
		handler       gin.HandlerFunc
		userID        int
		accessTokenID int
		token         string
		wantStatus    int
	}{
		{"pat_rejected_on_preview", PreviewAccountLinkChallenge, 42, 9, validToken, http.StatusForbidden},
		{"pat_rejected_on_approve", ApproveAccountLinkChallenge, 42, 9, validToken, http.StatusForbidden},
		{"unauthenticated_rejected", ApproveAccountLinkChallenge, 0, 0, validToken, http.StatusUnauthorized},
		{"disabled_user_rejected", ApproveAccountLinkChallenge, 77, 0, validToken, http.StatusForbidden},
		{"tampered_signature_rejected", ApproveAccountLinkChallenge, 42, 0, validToken + "x", http.StatusBadRequest},
		{"expired_challenge_rejected", ApproveAccountLinkChallenge, 42, 0, expiredToken, http.StatusGone},
		{"open_redirect_return_url_rejected", PreviewAccountLinkChallenge, 42, 0, badReturnToken, http.StatusBadRequest},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := runAccountLinkRequest(t, tc.handler, tc.userID, tc.accessTokenID, map[string]any{"challenge": tc.token})
			assert.Equal(t, tc.wantStatus, rec.Code)
		})
	}

	t.Run("missing_secret_fails_closed", func(t *testing.T) {
		t.Setenv("NEEDTOKEN_LINKING_SECRET", "")
		rec := runAccountLinkRequest(t, PreviewAccountLinkChallenge, 42, 0, map[string]any{"challenge": validToken})
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
	})
}
