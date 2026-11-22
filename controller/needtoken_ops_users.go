package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	needTokenOpsUserDomain       = "needtoken:ops-user-admin:v1"
	needTokenOpsUserMaxBodyBytes = 16 * 1024
	needTokenOpsUserMaxSkewSec   = int64(60)
)

type opsCreateNeedTokenUserRequest struct {
	Username    string `json:"username"`
	Email       string `json:"email"`
	Password    string `json:"password"`
	DisplayName string `json:"display_name"`
	Group       string `json:"group"`
	Remark      string `json:"remark"`
}

type opsUpdateNeedTokenUserRequest struct {
	Username    *string `json:"username"`
	Email       *string `json:"email"`
	Password    *string `json:"password"`
	DisplayName *string `json:"display_name"`
	Group       *string `json:"group"`
	Remark      *string `json:"remark"`
	Status      *int    `json:"status"`
}

func computeOpsUserAdminSignature(secret []byte, method, path, timestamp string, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(needTokenOpsUserDomain))
	mac.Write([]byte{0})
	mac.Write([]byte(strings.ToUpper(strings.TrimSpace(method))))
	mac.Write([]byte{0})
	mac.Write([]byte(path))
	mac.Write([]byte{0})
	mac.Write([]byte(timestamp))
	mac.Write([]byte{0})
	mac.Write(rawBody)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func verifyOpsUserAdminHMAC(c *gin.Context) ([]byte, bool) {
	secret, err := accountLinkSecret()
	if err != nil {
		common.ApiErrorStatus(c, http.StatusServiceUnavailable, common.NewMessage("Ops user bridge is not enabled"), gin.H{"code": "ops_user_bridge_disabled"})
		return nil, false
	}
	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, needTokenOpsUserMaxBodyBytes+1))
	if err != nil || len(rawBody) > needTokenOpsUserMaxBodyBytes {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request body"), gin.H{"code": "invalid_request"})
		return nil, false
	}
	tsRaw := strings.TrimSpace(c.GetHeader("X-NeedToken-Timestamp"))
	sigRaw := strings.TrimSpace(c.GetHeader("X-NeedToken-Signature"))
	tsUnix, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil || sigRaw == "" {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Missing authentication headers"), gin.H{"code": "unauthorized"})
		return nil, false
	}
	skew := accountLinkNow().Unix() - tsUnix
	if skew < -needTokenOpsUserMaxSkewSec || skew > needTokenOpsUserMaxSkewSec {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Request timestamp expired"), gin.H{"code": "timestamp_expired"})
		return nil, false
	}
	expected := computeOpsUserAdminSignature(secret, c.Request.Method, c.Request.URL.Path, tsRaw, rawBody)
	if !hmac.Equal([]byte(strings.ToLower(sigRaw)), []byte(strings.ToLower(expected))) {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Invalid signature"), gin.H{"code": "invalid_signature"})
		return nil, false
	}
	return rawBody, true
}

func opsUserSummaryJSON(u *model.User) gin.H {
	return gin.H{
		"id":           u.Id,
		"username":     u.Username,
		"email":        u.Email,
		"display_name": u.DisplayName,
		"group":        u.Group,
		"status":       u.Status,
		"role":         u.Role,
		"remark":       u.Remark,
		"quota":        u.Quota,
		"used_quota":   u.UsedQuota,
	}
}

func recordOpsUserBridgeAudit(c *gin.Context, targetUserID int, action string, params map[string]any) {
	if params == nil {
		params = map[string]any{}
	}
	params["source"] = "needtoken_ops_bridge"
	auditInfo := &model.AuditRequestInfo{
		Method:  c.Request.Method,
		Route:   c.FullPath(),
		Path:    c.Request.URL.Path,
		Status:  http.StatusOK,
		Success: true,
	}
	model.RecordOperationAuditLog(
		targetUserID,
		common.RoleAdminUser,
		action,
		c.ClientIP(),
		action,
		params,
		nil,
		auditInfo,
		c,
	)
}

// OpsCreateNeedTokenUser creates a common-role user with verified email &
// password on behalf of the NeedToken Ops control plane.
func OpsCreateNeedTokenUser(c *gin.Context) {
	rawBody, ok := verifyOpsUserAdminHMAC(c)
	if !ok {
		return
	}
	var req opsCreateNeedTokenUserRequest
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid JSON payload"), gin.H{"code": "invalid_request"})
		return
	}
	username := strings.TrimSpace(req.Username)
	email := model.NormalizeEmail(req.Email)
	if username == "" || len(username) > 20 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid username"), gin.H{"code": "invalid_username"})
		return
	}
	if email != "" && !isValidProvisionEmail(email) {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid email address"), gin.H{"code": "invalid_email"})
		return
	}
	if len(req.Password) < 8 || len(req.Password) > 128 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Password must be between 8 and 128 characters"), gin.H{"code": "invalid_password"})
		return
	}
	displayName := sanitizeProvisionDisplayName(req.DisplayName, email)
	if displayName == "" {
		displayName = username
	}
	group := strings.TrimSpace(req.Group)
	if group == "" {
		group = "default"
	}

	newUser := model.User{
		Username:    username,
		Email:       email,
		Password:    req.Password,
		DisplayName: displayName,
		Role:        common.RoleCommonUser,
		Status:      common.UserStatusEnabled,
		Group:       group,
		Remark:      strings.TrimSpace(req.Remark),
	}
	if err := model.DB.Transaction(func(tx *gorm.DB) error {
		return newUser.InsertWithTx(tx, 0)
	}); err != nil {
		common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Failed to create user: {{error}}", map[string]any{"error": err.Error()}), gin.H{"code": "create_failed"})
		return
	}
	newUser.FinishInsert(0)

	recordOpsUserBridgeAudit(c, newUser.Id, "ops.user.create", map[string]any{
		"user_id":   newUser.Id,
		"username":  newUser.Username,
		"has_email": newUser.Email != "",
		"group":     newUser.Group,
	})
	common.ApiSuccess(c, opsUserSummaryJSON(&newUser))
}

// OpsUpdateNeedTokenUser updates email, password, display name, group, remark,
// or status for a common-role user and revokes existing sessions on security
// changes (OWASP ASVS V2/V3).
func OpsUpdateNeedTokenUser(c *gin.Context) {
	rawBody, ok := verifyOpsUserAdminHMAC(c)
	if !ok {
		return
	}
	userID, err := strconv.Atoi(c.Param("id"))
	if err != nil || userID <= 0 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid user ID"), gin.H{"code": "invalid_user_id"})
		return
	}
	var req opsUpdateNeedTokenUserRequest
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid JSON payload"), gin.H{"code": "invalid_request"})
		return
	}
	if req.Email != nil {
		normalized := model.NormalizeEmail(*req.Email)
		if normalized != "" && !isValidProvisionEmail(normalized) {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid email address"), gin.H{"code": "invalid_email"})
			return
		}
		req.Email = &normalized
	}
	if req.Username != nil {
		trimmed := strings.TrimSpace(*req.Username)
		if trimmed == "" || len(trimmed) > 20 {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid username"), gin.H{"code": "invalid_username"})
			return
		}
		req.Username = &trimmed
	}
	if req.Password != nil && *req.Password != "" {
		if len(*req.Password) < 8 || len(*req.Password) > 128 {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Password must be between 8 and 128 characters"), gin.H{"code": "invalid_password"})
			return
		}
	}

	updated, authChanged, err := model.OpsUpdateCommonUser(userID, model.OpsUserUpdateInput{
		Username:    req.Username,
		Email:       req.Email,
		DisplayName: req.DisplayName,
		Group:       req.Group,
		Remark:      req.Remark,
		Password:    req.Password,
		Status:      req.Status,
	})
	if err != nil {
		switch {
		case errors.Is(err, gorm.ErrRecordNotFound):
			common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("User does not exist"), gin.H{"code": "user_not_found"})
		case errors.Is(err, model.ErrOpsTargetForbidden):
			common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Cannot manage admin or root account"), gin.H{"code": "forbidden_target"})
		case errors.Is(err, model.ErrOpsEmailTaken):
			common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Email is already taken"), gin.H{"code": "email_taken"})
		case errors.Is(err, model.ErrOpsUsernameTaken):
			common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Username is already taken"), gin.H{"code": "username_taken"})
		case errors.Is(err, model.ErrOpsInvalidStatus):
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid status"), gin.H{"code": "invalid_status"})
		default:
			common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to update user"), gin.H{"code": "update_failed"})
		}
		return
	}

	if authChanged {
		if _, err := model.RevokeAllUserSessions(updated.Id, "ops_admin_user_update"); err != nil {
			common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to revoke sessions"), gin.H{"code": "revoke_sessions_failed"})
			return
		}
	}
	if err := model.PublishUserAuthCache(updated.Id); err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to publish auth cache"), gin.H{"code": "cache_publish_failed"})
		return
	}

	recordOpsUserBridgeAudit(c, updated.Id, "ops.user.update", map[string]any{
		"user_id":          updated.Id,
		"email_updated":    req.Email != nil,
		"password_updated": req.Password != nil && *req.Password != "",
		"status_updated":   req.Status != nil,
		"group_updated":    req.Group != nil,
		"auth_changed":     authChanged,
	})
	common.ApiSuccess(c, opsUserSummaryJSON(updated))
}

// OpsDeleteNeedTokenUser permanently closes and deletes a common-role user,
// revoking all sessions and API tokens.
func OpsDeleteNeedTokenUser(c *gin.Context) {
	if _, ok := verifyOpsUserAdminHMAC(c); !ok {
		return
	}
	userID, err := strconv.Atoi(c.Param("id"))
	if err != nil || userID <= 0 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid user ID"), gin.H{"code": "invalid_user_id"})
		return
	}
	target, err := model.GetUserById(userID, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("User does not exist"), gin.H{"code": "user_not_found"})
			return
		}
		common.ApiError(c, err)
		return
	}
	if target.Role >= common.RoleAdminUser {
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Cannot delete admin or root account"), gin.H{"code": "forbidden_target"})
		return
	}
	revokedTokens, err := model.HardDeleteUserById(userID)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to delete user"), gin.H{"code": "delete_failed"})
		return
	}
	recordOpsUserBridgeAudit(c, target.Id, "ops.user.delete", map[string]any{
		"user_id":               target.Id,
		"username":              target.Username,
		"revoked_access_tokens": revokedTokens,
	})
	common.ApiSuccess(c, gin.H{
		"deleted":               true,
		"user_id":               target.Id,
		"revoked_access_tokens": revokedTokens,
	})
}

// OpsDisableNeedTokenUser2FA resets 2FA for a common-role user and revokes
// existing sessions.
func OpsDisableNeedTokenUser2FA(c *gin.Context) {
	if _, ok := verifyOpsUserAdminHMAC(c); !ok {
		return
	}
	userID, err := strconv.Atoi(c.Param("id"))
	if err != nil || userID <= 0 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid user ID"), gin.H{"code": "invalid_user_id"})
		return
	}
	target, err := model.GetUserById(userID, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("User does not exist"), gin.H{"code": "user_not_found"})
			return
		}
		common.ApiError(c, err)
		return
	}
	if target.Role >= common.RoleAdminUser {
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Cannot modify admin or root account"), gin.H{"code": "forbidden_target"})
		return
	}
	if err := model.DisableTwoFAWithAuthVersion(userID); err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Failed to reset 2FA"), gin.H{"code": "reset_2fa_failed"})
		return
	}
	_, _ = model.RevokeAllUserSessions(userID, "ops_admin_reset_2fa")
	_ = model.PublishUserAuthCache(userID)
	recordOpsUserBridgeAudit(c, target.Id, "ops.user.reset_2fa", map[string]any{
		"user_id":  target.Id,
		"username": target.Username,
	})
	common.ApiSuccess(c, gin.H{"disabled": true, "user_id": target.Id})
}
