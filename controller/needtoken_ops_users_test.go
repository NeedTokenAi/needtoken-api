package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func execOpsUserBridge(t *testing.T, secret, ts, method, path, paramID string, body []byte, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if ts != "" {
		req.Header.Set("X-NeedToken-Timestamp", ts)
	}
	if secret != "" && ts != "" {
		req.Header.Set("X-NeedToken-Signature", computeOpsUserAdminSignature([]byte(secret), method, path, ts, body))
	}
	if paramID != "" {
		c.Params = gin.Params{{Key: "id", Value: paramID}}
	}
	c.Request = req
	handler(c)
	return rec
}

func TestNeedTokenOpsUsersLifecycleAndSecurityControls(t *testing.T) {
	existingUser, _ := setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(
		&model.Token{},
		&model.UserAccessToken{},
		&model.ExternalIdentityClaim{},
		&model.UserOAuthBinding{},
		&model.PasskeyCredential{},
	))
	require.NoError(t, model.LOG_DB.AutoMigrate(&model.Log{}))
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", existingUser.Id).Update("email", "existing@example.com").Error)
	existingUser.Email = "existing@example.com"

	secret := "0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	origNow := accountLinkNow
	t.Cleanup(func() { accountLinkNow = origNow })
	accountLinkNow = func() time.Time { return now }
	ts := strconv.FormatInt(now.Unix(), 10)

	// 1. Reject missing / expired / invalid HMAC signature.
	badBody, err := common.Marshal(opsCreateNeedTokenUserRequest{
		Username: "alice_ops",
		Email:    "alice@example.com",
		Password: "StrongPassword123!",
	})
	require.NoError(t, err)
	recNoSig := execOpsUserBridge(t, "", ts, http.MethodPost, "/api/subscription/admin/needtoken-users", "", badBody, OpsCreateNeedTokenUser)
	assert.Equal(t, http.StatusUnauthorized, recNoSig.Code)

	expiredTS := strconv.FormatInt(now.Add(-5*time.Minute).Unix(), 10)
	recExpired := execOpsUserBridge(t, secret, expiredTS, http.MethodPost, "/api/subscription/admin/needtoken-users", "", badBody, OpsCreateNeedTokenUser)
	assert.Equal(t, http.StatusUnauthorized, recExpired.Code)

	// 2. Create common user with normalized email and hashed password.
	createBody, err := common.Marshal(opsCreateNeedTokenUserRequest{
		Username:    "alice_ops",
		Email:       "Alice.Ops@Example.com",
		Password:    "StrongPassword123!",
		DisplayName: "Alice Ops",
		Group:       "vip",
	})
	require.NoError(t, err)
	recCreate := execOpsUserBridge(t, secret, ts, http.MethodPost, "/api/subscription/admin/needtoken-users", "", createBody, OpsCreateNeedTokenUser)
	require.Equal(t, http.StatusOK, recCreate.Code, recCreate.Body.String())

	var createdResp struct {
		Success bool `json:"success"`
		Data    struct {
			ID       int    `json:"id"`
			Username string `json:"username"`
			Email    string `json:"email"`
			Group    string `json:"group"`
			Role     int    `json:"role"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recCreate.Body.Bytes(), &createdResp))
	require.True(t, createdResp.Success)
	assert.Greater(t, createdResp.Data.ID, 0)
	assert.Equal(t, "alice.ops@example.com", createdResp.Data.Email)
	assert.Equal(t, "vip", createdResp.Data.Group)
	assert.Equal(t, common.RoleCommonUser, createdResp.Data.Role)

	stored, err := model.GetUserById(createdResp.Data.ID, true)
	require.NoError(t, err)
	assert.NotEqual(t, "StrongPassword123!", stored.Password)
	assert.True(t, common.ValidatePasswordAndHash("StrongPassword123!", stored.Password))
	origAuthVersion := stored.AuthVersion

	// 3. Update email, password, and status -> bumps AuthVersion and rejects duplicate email.
	dupEmail := existingUser.Email
	dupBody, err := common.Marshal(opsUpdateNeedTokenUserRequest{Email: &dupEmail})
	require.NoError(t, err)
	idStr := strconv.Itoa(createdResp.Data.ID)
	recDup := execOpsUserBridge(t, secret, ts, http.MethodPut, "/api/subscription/admin/needtoken-users/"+idStr, idStr, dupBody, OpsUpdateNeedTokenUser)
	assert.Equal(t, http.StatusConflict, recDup.Code)

	newEmail := "alice.updated@example.com"
	newPass := "UpdatedSecret456!"
	disabled := common.UserStatusDisabled
	updBody, err := common.Marshal(opsUpdateNeedTokenUserRequest{
		Email:    &newEmail,
		Password: &newPass,
		Status:   &disabled,
	})
	require.NoError(t, err)
	recUpd := execOpsUserBridge(t, secret, ts, http.MethodPut, "/api/subscription/admin/needtoken-users/"+idStr, idStr, updBody, OpsUpdateNeedTokenUser)
	require.Equal(t, http.StatusOK, recUpd.Code, recUpd.Body.String())

	afterUpd, err := model.GetUserById(createdResp.Data.ID, true)
	require.NoError(t, err)
	assert.Equal(t, "alice.updated@example.com", afterUpd.Email)
	assert.Equal(t, common.UserStatusDisabled, afterUpd.Status)
	assert.True(t, common.ValidatePasswordAndHash("UpdatedSecret456!", afterUpd.Password))
	assert.Greater(t, afterUpd.AuthVersion, origAuthVersion)

	// 4. Forbid modifying or deleting admin/root accounts.
	require.NoError(t, model.DB.Model(&model.User{}).Where("id = ?", existingUser.Id).Update("role", common.RoleAdminUser).Error)
	adminIDStr := strconv.Itoa(existingUser.Id)
	recAdminUpd := execOpsUserBridge(t, secret, ts, http.MethodPut, "/api/subscription/admin/needtoken-users/"+adminIDStr, adminIDStr, updBody, OpsUpdateNeedTokenUser)
	assert.Equal(t, http.StatusForbidden, recAdminUpd.Code)
	recAdminDel := execOpsUserBridge(t, secret, ts, http.MethodDelete, "/api/subscription/admin/needtoken-users/"+adminIDStr, adminIDStr, nil, OpsDeleteNeedTokenUser)
	assert.Equal(t, http.StatusForbidden, recAdminDel.Code)

	// 5. Hard delete common user succeeds.
	recDel := execOpsUserBridge(t, secret, ts, http.MethodDelete, "/api/subscription/admin/needtoken-users/"+idStr, idStr, nil, OpsDeleteNeedTokenUser)
	require.Equal(t, http.StatusOK, recDel.Code, recDel.Body.String())
	_, err = model.GetUserById(createdResp.Data.ID, false)
	assert.Error(t, err)
}
