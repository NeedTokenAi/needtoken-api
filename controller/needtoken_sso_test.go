package controller

import (
	"bytes"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/pquerna/otp/totp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func resetNeedTokenSSOReplayCache(t *testing.T) {
	t.Helper()
	ssoReplayMu.Lock()
	clear(ssoConsumedJTIs)
	ssoReplayMu.Unlock()
	t.Cleanup(func() {
		ssoReplayMu.Lock()
		clear(ssoConsumedJTIs)
		ssoReplayMu.Unlock()
	})
}

func makeSignedProvisionRequest(t *testing.T, secret string, timestamp string, rawBody []byte) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/api/subscription/admin/needtoken-member", bytes.NewReader(rawBody))
	req.Header.Set("Content-Type", "application/json")
	if timestamp != "" {
		req.Header.Set("X-NeedToken-Timestamp", timestamp)
	}
	if secret != "" && timestamp != "" {
		req.Header.Set("X-NeedToken-Signature", computeProvisionMemberSignature([]byte(secret), timestamp, rawBody))
	}
	c.Request = req
	ProvisionNeedTokenMember(c)
	return rec
}

func makeConsoleSSOTicket(t *testing.T, secret []byte, claims consoleSSOTicketClaims) string {
	t.Helper()
	raw, err := common.Marshal(claims)
	require.NoError(t, err)
	payload := base64.RawURLEncoding.EncodeToString(raw)
	sig := accountLinkMAC(secret, needTokenConsoleSSOTicketDomain, payload)
	return accountLinkTokenPrefix + payload + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func runConsoleSSOExchange(t *testing.T, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := common.Marshal(body)
	require.NoError(t, err)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(http.MethodPost, "/api/user/needtoken-sso", bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "NeedTokenSSOTest/1.0")
	c.Request = req
	ExchangeNeedTokenSSO(c)
	return rec
}

func TestNeedTokenProvisionMemberCreatesAndProtectsExistingAccounts(t *testing.T) {
	existingUser, _ := setupSecurityEnrollmentTest(t)
	require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	resetNeedTokenSSOReplayCache(t)

	secret := "0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	origNow := accountLinkNow
	t.Cleanup(func() { accountLinkNow = origNow })
	accountLinkNow = func() time.Time { return now }
	ts := strconv.FormatInt(now.Unix(), 10)

	// 1. Fresh verified storefront customer is provisioned with safe username & hashed password.
	createPayload, err := common.Marshal(needTokenProvisionMemberRequest{
		Source:             "needtoken-storefront",
		MerchantCustomerID: "needtoken-storefront:user:42",
		Email:              "NewMember@Example.com",
		EmailVerified:      true,
		DisplayName:        "  qiutanglanyue_very_long_display_name  ",
	})
	require.NoError(t, err)

	rec := makeSignedProvisionRequest(t, secret, ts, createPayload)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var createResp struct {
		Success bool `json:"success"`
		Data    struct {
			Status       string `json:"status"`
			NativeUserID int    `json:"native_user_id"`
			Username     string `json:"username"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &createResp))
	require.True(t, createResp.Success)
	assert.Equal(t, "created", createResp.Data.Status)
	assert.Greater(t, createResp.Data.NativeUserID, 0)
	assert.Regexp(t, regexp.MustCompile(`^nt_42_[a-z0-9]{4}$`), createResp.Data.Username)
	assert.LessOrEqual(t, len(createResp.Data.Username), 18)

	storedNewUser, err := model.GetUserById(createResp.Data.NativeUserID, true)
	require.NoError(t, err)
	assert.Equal(t, "newmember@example.com", storedNewUser.Email)
	assert.Equal(t, common.RoleCommonUser, storedNewUser.Role)
	assert.Equal(t, common.UserStatusEnabled, storedNewUser.Status)
	assert.Equal(t, "qiutanglanyue_very_l", storedNewUser.DisplayName)
	assert.True(t, strings.HasPrefix(storedNewUser.Password, "$argon2id$") || strings.HasPrefix(storedNewUser.Password, "$2"), "password must be hashed once")

	// 2. Pre-existing account with matching email MUST NOT be modified or auto-bound.
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", existingUser.Id).Update("email", "legacy@example.com").Error)
	beforeExisting, err := model.GetUserById(existingUser.Id, true)
	require.NoError(t, err)

	existingPayload, err := common.Marshal(needTokenProvisionMemberRequest{
		Source:             "needtoken-storefront",
		MerchantCustomerID: "needtoken-storefront:user:99",
		Email:              "LEGACY@example.com",
		EmailVerified:      true,
		DisplayName:        "attacker_attempt",
	})
	require.NoError(t, err)

	rec = makeSignedProvisionRequest(t, secret, ts, existingPayload)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var existingResp struct {
		Success bool `json:"success"`
		Data    struct {
			Status       string `json:"status"`
			NativeUserID int    `json:"native_user_id"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &existingResp))
	require.True(t, existingResp.Success)
	assert.Equal(t, "existing_requires_link", existingResp.Data.Status)
	assert.Zero(t, existingResp.Data.NativeUserID)

	afterExisting, err := model.GetUserById(existingUser.Id, true)
	require.NoError(t, err)
	assert.Equal(t, beforeExisting.Username, afterExisting.Username)
	assert.Equal(t, beforeExisting.Password, afterExisting.Password)
	assert.Equal(t, beforeExisting.DisplayName, afterExisting.DisplayName)
	assert.Equal(t, beforeExisting.AuthVersion, afterExisting.AuthVersion)
}

func TestNeedTokenProvisionMemberSecurityRejections(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	secret := "0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	origNow := accountLinkNow
	t.Cleanup(func() { accountLinkNow = origNow })
	accountLinkNow = func() time.Time { return now }
	validTS := strconv.FormatInt(now.Unix(), 10)

	validBody, err := common.Marshal(needTokenProvisionMemberRequest{
		Source:             "needtoken-storefront",
		MerchantCustomerID: "needtoken-storefront:user:88",
		Email:              "valid88@example.com",
		EmailVerified:      true,
		DisplayName:        "valid88",
	})
	require.NoError(t, err)

	t.Run("disabled_when_secret_short", func(t *testing.T) {
		t.Setenv("NEEDTOKEN_LINKING_SECRET", "short-secret")
		rec := makeSignedProvisionRequest(t, "short-secret", validTS, validBody)
		assert.Equal(t, http.StatusServiceUnavailable, rec.Code)
		assert.Contains(t, rec.Body.String(), `"sso_disabled"`)
	})

	t.Run("tampered_signature_rejected", func(t *testing.T) {
		rec := makeSignedProvisionRequest(t, strings.Repeat("x", 48), validTS, validBody)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("expired_timestamp_rejected", func(t *testing.T) {
		staleTS := strconv.FormatInt(now.Add(-61*time.Second).Unix(), 10)
		rec := makeSignedProvisionRequest(t, secret, staleTS, validBody)
		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})

	t.Run("oversized_body_rejected", func(t *testing.T) {
		hugeBody := []byte(`{"source":"needtoken-storefront","padding":"` + strings.Repeat("a", needTokenProvisionMaxBodyBytes+10) + `"}`)
		rec := makeSignedProvisionRequest(t, secret, validTS, hugeBody)
		assert.Equal(t, http.StatusBadRequest, rec.Code)
	})

	invalidPayloads := []struct {
		name string
		req  needTokenProvisionMemberRequest
	}{
		{
			name: "unverified_email",
			req: needTokenProvisionMemberRequest{
				Source: "needtoken-storefront", MerchantCustomerID: "needtoken-storefront:user:1", Email: "u1@example.com", EmailVerified: false,
			},
		},
		{
			name: "wrong_source",
			req: needTokenProvisionMemberRequest{
				Source: "other-system", MerchantCustomerID: "needtoken-storefront:user:1", Email: "u1@example.com", EmailVerified: true,
			},
		},
		{
			name: "wrong_merchant_prefix",
			req: needTokenProvisionMemberRequest{
				Source: "needtoken-storefront", MerchantCustomerID: "user:1", Email: "u1@example.com", EmailVerified: true,
			},
		},
		{
			name: "empty_merchant_suffix",
			req: needTokenProvisionMemberRequest{
				Source: "needtoken-storefront", MerchantCustomerID: "needtoken-storefront:user:", Email: "u1@example.com", EmailVerified: true,
			},
		},
		{
			name: "invalid_email",
			req: needTokenProvisionMemberRequest{
				Source: "needtoken-storefront", MerchantCustomerID: "needtoken-storefront:user:1", Email: "not-an-email", EmailVerified: true,
			},
		},
	}
	for _, tc := range invalidPayloads {
		t.Run(tc.name, func(t *testing.T) {
			raw, marshalErr := common.Marshal(tc.req)
			require.NoError(t, marshalErr)
			rec := makeSignedProvisionRequest(t, secret, validTS, raw)
			assert.Equal(t, http.StatusBadRequest, rec.Code)
		})
	}
}

func TestNeedTokenConsoleSSOExchangeReplayAndMFA(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	resetNeedTokenSSOReplayCache(t)

	secret := "0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	// Use real current time so StartLoginVerification / CreateLoginSession DB timestamps align.
	now := time.Now().UTC().Truncate(time.Second)
	origNow := accountLinkNow
	t.Cleanup(func() { accountLinkNow = origNow })
	accountLinkNow = func() time.Time { return now }

	ticket := makeConsoleSSOTicket(t, []byte(secret), consoleSSOTicketClaims{
		JTI:        "jti_primary_once_001",
		Source:     "needtoken-storefront",
		MerchantID: "needtoken-storefront:user:42",
		NativeUID:  user.Id,
		TargetPath: "/dashboard",
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(60 * time.Second).Unix(),
	})

	// 1. Valid exchange issues AuthBundle and RefreshCookie with login_method=needtoken_sso.
	rec := runConsoleSSOExchange(t, map[string]string{"ticket": ticket})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())

	var loginResp struct {
		Success bool `json:"success"`
		Data    struct {
			AccessToken string `json:"access_token"`
			Session     struct {
				SID         string `json:"sid"`
				LoginMethod string `json:"login_method"`
			} `json:"session"`
			User struct {
				ID int `json:"id"`
			} `json:"user"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &loginResp))
	require.True(t, loginResp.Success)
	assert.NotEmpty(t, loginResp.Data.AccessToken)
	assert.Equal(t, "needtoken_sso", loginResp.Data.Session.LoginMethod)
	assert.Equal(t, user.Id, loginResp.Data.User.ID)
	assert.NotEmpty(t, rec.Result().Cookies())

	// Audit log must record login without leaking ticket or secret.
	var auditLogs []model.AuditLog
	require.NoError(t, model.LOG_DB.Find(&auditLogs).Error)
	require.NotEmpty(t, auditLogs)
	for _, entry := range auditLogs {
		assert.NotContains(t, entry.Content, ticket)
		assert.NotContains(t, entry.Content, secret)
	}

	// 2. Replaying the exact same JTI is immediately rejected with sso_ticket_replayed.
	replayRec := runConsoleSSOExchange(t, map[string]string{"ticket": ticket})
	require.Equal(t, http.StatusBadRequest, replayRec.Code)
	assert.Contains(t, replayRec.Body.String(), `"sso_ticket_replayed"`)

	// 3. Expired and oversized-TTL (>120s) tickets are rejected.
	expiredTicket := makeConsoleSSOTicket(t, []byte(secret), consoleSSOTicketClaims{
		JTI:        "jti_expired_002",
		Source:     "needtoken-storefront",
		MerchantID: "needtoken-storefront:user:42",
		NativeUID:  user.Id,
		IssuedAt:   now.Add(-70 * time.Second).Unix(),
		ExpiresAt:  now.Add(-10 * time.Second).Unix(),
	})
	assert.Equal(t, http.StatusBadRequest, runConsoleSSOExchange(t, map[string]string{"ticket": expiredTicket}).Code)

	longTTLTicket := makeConsoleSSOTicket(t, []byte(secret), consoleSSOTicketClaims{
		JTI:        "jti_long_ttl_003",
		Source:     "needtoken-storefront",
		MerchantID: "needtoken-storefront:user:42",
		NativeUID:  user.Id,
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(121 * time.Second).Unix(),
	})
	assert.Equal(t, http.StatusBadRequest, runConsoleSSOExchange(t, map[string]string{"ticket": longTTLTicket}).Code)

	// 4. User with 2FA enabled receives LoginChallenge and NEVER bypasses MFA.
	factor := &model.TwoFA{UserId: user.Id, Secret: "JBSWY3DPEHPK3PXP", IsEnabled: true}
	require.NoError(t, model.DB.Create(factor).Error)

	mfaTicket := makeConsoleSSOTicket(t, []byte(secret), consoleSSOTicketClaims{
		JTI:        "jti_mfa_challenge_004",
		Source:     "needtoken-storefront",
		MerchantID: "needtoken-storefront:user:42",
		NativeUID:  user.Id,
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(60 * time.Second).Unix(),
	})
	mfaRec := runConsoleSSOExchange(t, map[string]string{"ticket": mfaTicket})
	require.Equal(t, http.StatusOK, mfaRec.Code, mfaRec.Body.String())

	var challengeResp struct {
		Success bool `json:"success"`
		Data    struct {
			RequireVerification bool   `json:"require_verification"`
			FlowToken           string `json:"flow_token"`
			AccessToken         string `json:"access_token"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(mfaRec.Body.Bytes(), &challengeResp))
	require.True(t, challengeResp.Success)
	assert.True(t, challengeResp.Data.RequireVerification)
	assert.NotEmpty(t, challengeResp.Data.FlowToken)
	assert.Empty(t, challengeResp.Data.AccessToken)
	assert.Empty(t, mfaRec.Result().Cookies())

	// Completing second-factor TOTP verification issues final session preserving needtoken_sso method.
	code, err := totp.GenerateCode(factor.Secret, time.Now())
	require.NoError(t, err)
	verifyBody, err := common.Marshal(map[string]string{"flow_token": challengeResp.Data.FlowToken, "code": code})
	require.NoError(t, err)
	verifyRec := httptest.NewRecorder()
	vc, _ := gin.CreateTestContext(verifyRec)
	vc.Request = httptest.NewRequest(http.MethodPost, "/api/user/login/verify", bytes.NewReader(verifyBody))
	vc.Request.Header.Set("Content-Type", "application/json")
	VerifyLogin(vc)
	require.Equal(t, http.StatusOK, verifyRec.Code, verifyRec.Body.String())
	require.NoError(t, common.Unmarshal(verifyRec.Body.Bytes(), &loginResp))
	assert.Equal(t, "needtoken_sso", loginResp.Data.Session.LoginMethod)
	assert.NotEmpty(t, loginResp.Data.AccessToken)

	// 5. Disabled account is rejected with 403.
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", user.Id).Update("status", common.UserStatusDisabled).Error)
	bannedTicket := makeConsoleSSOTicket(t, []byte(secret), consoleSSOTicketClaims{
		JTI:        "jti_banned_005",
		Source:     "needtoken-storefront",
		MerchantID: "needtoken-storefront:user:42",
		NativeUID:  user.Id,
		IssuedAt:   now.Unix(),
		ExpiresAt:  now.Add(60 * time.Second).Unix(),
	})
	assert.Equal(t, http.StatusForbidden, runConsoleSSOExchange(t, map[string]string{"ticket": bannedTicket}).Code)
}
