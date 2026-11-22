package controller

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"

	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type needTokenAdminTokenDTO struct {
	Id                 int      `json:"id"`
	UserId             int      `json:"user_id"`
	Name               string   `json:"name"`
	Status             int      `json:"status"`
	Group              string   `json:"group"`
	RemainQuota        int      `json:"remain_quota"`
	UsedQuota          int      `json:"used_quota"`
	UnlimitedQuota     bool     `json:"unlimited_quota"`
	ExpiredTime        int64    `json:"expired_time"`
	CreatedTime        int64    `json:"created_time"`
	AccessedTime       int64    `json:"accessed_time"`
	ModelLimitsEnabled bool     `json:"model_limits_enabled"`
	ModelLimits        string   `json:"model_limits"`
	AllowIps           *string  `json:"allow_ips"`
	CrossGroupRetry    bool     `json:"cross_group_retry"`
	AutoGroups         []string `json:"auto_groups"`
	MaskedKey          string   `json:"masked_key"`
}

type needTokenAdminTokenRequest struct {
	Name               *string              `json:"name"`
	Status             *int                 `json:"status"`
	Group              *string              `json:"group"`
	RemainQuota        *int                 `json:"remain_quota"`
	UnlimitedQuota     *bool                `json:"unlimited_quota"`
	ExpiredTime        *int64               `json:"expired_time"`
	ModelLimitsEnabled *bool                `json:"model_limits_enabled"`
	ModelLimits        *string              `json:"model_limits"`
	AllowIps           *string              `json:"allow_ips"`
	CrossGroupRetry    *bool                `json:"cross_group_retry"`
	AutoGroups         tokenAutoGroupsInput `json:"auto_groups"`
}

func maskAdminTokenKey(rawKey string) string {
	trimmed := strings.TrimPrefix(strings.TrimSpace(rawKey), "sk-")
	if trimmed == "" {
		return ""
	}
	if len(trimmed) <= 4 {
		return "sk-" + strings.Repeat("*", len(trimmed))
	}
	if len(trimmed) <= 8 {
		return "sk-" + trimmed[:2] + "..." + trimmed[len(trimmed)-2:]
	}
	return "sk-" + trimmed[:4] + "..." + trimmed[len(trimmed)-4:]
}

func buildAdminTokenDTO(token *model.Token) *needTokenAdminTokenDTO {
	if token == nil {
		return nil
	}
	autoGroups, err := token.GetAutoGroups()
	if err != nil {
		common.SysError(common.LogText("failed to parse auto groups for token %d: %v", token.Id, err))
		autoGroups = nil
	}
	if len(autoGroups) == 0 {
		autoGroups = nil
	}
	return &needTokenAdminTokenDTO{
		Id:                 token.Id,
		UserId:             token.UserId,
		Name:               token.Name,
		Status:             token.Status,
		Group:              token.Group,
		RemainQuota:        token.RemainQuota,
		UsedQuota:          token.UsedQuota,
		UnlimitedQuota:     token.UnlimitedQuota,
		ExpiredTime:        token.ExpiredTime,
		CreatedTime:        token.CreatedTime,
		AccessedTime:       token.AccessedTime,
		ModelLimitsEnabled: token.ModelLimitsEnabled,
		ModelLimits:        token.ModelLimits,
		AllowIps:           token.AllowIps,
		CrossGroupRetry:    token.CrossGroupRetry,
		AutoGroups:         autoGroups,
		MaskedKey:          maskAdminTokenKey(token.Key),
	}
}

func populateUsersTwoFAEnabled(users []*model.User) error {
	if len(users) == 0 {
		return nil
	}
	ids := make([]int, 0, len(users))
	for _, user := range users {
		if user != nil && user.Id > 0 {
			ids = append(ids, user.Id)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	enabledSet, err := model.GetTwoFAEnabledUserIDs(ids)
	if err != nil {
		return err
	}
	for _, user := range users {
		if user != nil {
			user.TwoFAEnabled = enabledSet[user.Id]
		}
	}
	return nil
}

func loadManageableTokenTargetUser(c *gin.Context) (*model.User, bool) {
	userID, err := strconv.Atoi(c.Param("id"))
	if err != nil || userID <= 0 {
		common.ApiErrorT(c, "Invalid user ID")
		return nil, false
	}
	targetUser, err := model.GetUserById(userID, false)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorT(c, "User does not exist")
			return nil, false
		}
		common.ApiError(c, err)
		return nil, false
	}
	myRole := c.GetInt("role")
	if !canManageTargetRole(myRole, targetUser.Role) {
		common.ApiErrorT(c, "No permission to access users of same or higher level")
		return nil, false
	}
	return targetUser, true
}

func validateAdminTokenQuota(c *gin.Context, unlimitedQuota bool, remainQuota int) bool {
	if unlimitedQuota {
		return true
	}
	if remainQuota < 0 {
		common.ApiErrorT(c, "Quota value cannot be negative")
		return false
	}
	maxQuotaValue := maxTokenQuota()
	if remainQuota > maxQuotaValue {
		common.ApiErrorT(c, "Quota value exceeds valid range, maximum is {{max}}", map[string]any{"max": logger.FormatQuota(maxQuotaValue)})
		return false
	}
	return true
}

func setAdminTokenAutoGroups(c *gin.Context, targetUserGroup string, token *model.Token, groups []string) bool {
	if len(groups) == 0 {
		if err := token.SetAutoGroups(nil); err != nil {
			common.ApiError(c, err)
			return false
		}
		return true
	}
	maxCount := setting.GetMaxTokenAutoGroups()
	if len(groups) > maxCount {
		common.ApiErrorT(c, "A token can select at most {{max}} Auto groups", map[string]any{"max": maxCount})
		return false
	}
	seen := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		if _, ok := seen[group]; ok {
			common.ApiErrorT(c, "Auto group {{group}} is duplicated", map[string]any{"group": group})
			return false
		}
		seen[group] = struct{}{}
		if !service.IsUserSelectableGroup(targetUserGroup, group) {
			common.ApiErrorT(c, "Auto group {{group}} is unavailable or unauthorized", map[string]any{"group": group})
			return false
		}
	}
	if err := token.SetAutoGroups(groups); err != nil {
		common.ApiError(c, err)
		return false
	}
	return true
}

func recordAdminUserTokenAudit(c *gin.Context, targetUser *model.User, action string, content string, params map[string]any) {
	if params == nil {
		params = map[string]any{}
	}
	params["target_user_id"] = targetUser.Id
	params["target_username"] = targetUser.Username
	auditInfo := &model.AuditRequestInfo{
		Method:  c.Request.Method,
		Route:   c.FullPath(),
		Path:    c.FullPath(),
		Status:  http.StatusOK,
		Success: true,
	}
	model.RecordOperationAuditLog(
		c.GetInt("id"),
		c.GetInt("role"),
		content,
		c.ClientIP(),
		action,
		params,
		auditOperatorInfo(c),
		auditInfo,
		c,
	)
	markAuditLogged(c)
}

func AdminGetUserTokens(c *gin.Context) {
	targetUser, ok := loadManageableTokenTargetUser(c)
	if !ok {
		return
	}
	pageInfo := common.GetPageQuery(c)
	tokens, err := model.GetAllUserTokens(targetUser.Id, pageInfo.GetStartIdx(), pageInfo.GetPageSize())
	if err != nil {
		common.ApiError(c, err)
		return
	}
	total, err := model.CountUserTokens(targetUser.Id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	items := make([]*needTokenAdminTokenDTO, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, buildAdminTokenDTO(token))
	}
	pageInfo.SetTotal(int(total))
	pageInfo.SetItems(items)
	common.ApiSuccess(c, pageInfo)
}

func AdminCreateUserToken(c *gin.Context) {
	targetUser, ok := loadManageableTokenTargetUser(c)
	if !ok {
		return
	}

	var req needTokenAdminTokenRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}

	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	if len(name) > 50 {
		common.ApiErrorT(c, "Token name is too long")
		return
	}

	unlimitedQuota := false
	if req.UnlimitedQuota != nil {
		unlimitedQuota = *req.UnlimitedQuota
	}
	remainQuota := 0
	if req.RemainQuota != nil {
		remainQuota = *req.RemainQuota
	}
	if !validateAdminTokenQuota(c, unlimitedQuota, remainQuota) {
		return
	}

	expiredTime := int64(-1)
	if req.ExpiredTime != nil {
		expiredTime = *req.ExpiredTime
		if expiredTime != -1 && expiredTime < 0 {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
	}

	maxTokens := operation_setting.GetMaxUserTokens()
	count, err := model.CountUserTokens(targetUser.Id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if int(count) >= maxTokens {
		common.ApiErrorT(c, "Maximum number of tokens reached ({{max}})", map[string]any{"max": maxTokens})
		return
	}

	group := ""
	if req.Group != nil {
		group = *req.Group
	}
	modelLimitsEnabled := false
	if req.ModelLimitsEnabled != nil {
		modelLimitsEnabled = *req.ModelLimitsEnabled
	}
	modelLimits := ""
	if req.ModelLimits != nil {
		modelLimits = *req.ModelLimits
	}
	crossGroupRetry := false
	if req.CrossGroupRetry != nil {
		crossGroupRetry = *req.CrossGroupRetry
	}

	now := common.GetTimestamp()
	cleanToken := model.Token{
		UserId:              targetUser.Id,
		Name:                name,
		Status:              common.TokenStatusEnabled,
		CreatedTime:         now,
		AccessedTime:        now,
		ExpiredTime:         expiredTime,
		RemainQuota:         remainQuota,
		UnlimitedQuota:      unlimitedQuota,
		ModelLimitsEnabled:  modelLimitsEnabled,
		ModelLimits:         modelLimits,
		AllowIps:            req.AllowIps,
		Group:               group,
		CrossGroupRetry:     crossGroupRetry,
		AllowUserQuotaQuery: true,
	}

	if cleanToken.Group == "auto" {
		if !setAdminTokenAutoGroups(c, targetUser.Group, &cleanToken, req.AutoGroups.Groups) {
			return
		}
	} else {
		cleanToken.CrossGroupRetry = false
		_ = cleanToken.SetAutoGroups(nil)
	}

	rawKey, err := common.GenerateKey()
	if err != nil {
		common.ApiErrorT(c, "Failed to generate token")
		common.SysLog(common.LogText("failed to generate token key: %s", err.Error()))
		return
	}
	cleanToken.Key = rawKey

	if err := cleanToken.Insert(); err != nil {
		common.ApiError(c, err)
		return
	}

	tokenDTO := buildAdminTokenDTO(&cleanToken)
	recordAdminUserTokenAudit(
		c,
		targetUser,
		"admin.user_token_create",
		fmt.Sprintf("Created API token %q (ID: %d) for user %s (ID: %d)", cleanToken.Name, cleanToken.Id, targetUser.Username, targetUser.Id),
		map[string]any{
			"token_id":   cleanToken.Id,
			"token_name": cleanToken.Name,
			"masked_key": tokenDTO.MaskedKey,
		},
	)

	common.ApiSuccess(c, gin.H{
		"key":   "sk-" + rawKey,
		"token": tokenDTO,
	})
}

func AdminUpdateUserToken(c *gin.Context) {
	targetUser, ok := loadManageableTokenTargetUser(c)
	if !ok {
		return
	}

	tokenID, err := strconv.Atoi(c.Param("token_id"))
	if err != nil || tokenID <= 0 {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}

	var req needTokenAdminTokenRequest
	if err := common.DecodeJson(c.Request.Body, &req); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}

	cleanToken, err := model.GetTokenByIds(tokenID, targetUser.Id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorT(c, "Token does not exist")
			return
		}
		common.ApiError(c, err)
		return
	}

	previous := *cleanToken

	if req.Name != nil {
		if len(*req.Name) > 50 {
			common.ApiErrorT(c, "Token name is too long")
			return
		}
		cleanToken.Name = *req.Name
	}
	if req.ExpiredTime != nil {
		if *req.ExpiredTime != -1 && *req.ExpiredTime < 0 {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		cleanToken.ExpiredTime = *req.ExpiredTime
	}
	if req.UnlimitedQuota != nil {
		cleanToken.UnlimitedQuota = *req.UnlimitedQuota
	}
	if req.RemainQuota != nil {
		cleanToken.RemainQuota = *req.RemainQuota
	}
	if !validateAdminTokenQuota(c, cleanToken.UnlimitedQuota, cleanToken.RemainQuota) {
		return
	}
	if req.ModelLimitsEnabled != nil {
		cleanToken.ModelLimitsEnabled = *req.ModelLimitsEnabled
	}
	if req.ModelLimits != nil {
		cleanToken.ModelLimits = *req.ModelLimits
	}
	if req.AllowIps != nil {
		cleanToken.AllowIps = req.AllowIps
	}
	if req.Group != nil {
		cleanToken.Group = *req.Group
	}
	if req.CrossGroupRetry != nil {
		cleanToken.CrossGroupRetry = *req.CrossGroupRetry
	}
	if cleanToken.Group != "auto" {
		cleanToken.CrossGroupRetry = false
		_ = cleanToken.SetAutoGroups(nil)
	} else if req.AutoGroups.Set {
		if !setAdminTokenAutoGroups(c, targetUser.Group, cleanToken, req.AutoGroups.Groups) {
			return
		}
	}

	if req.Status != nil {
		if *req.Status != common.TokenStatusEnabled && *req.Status != common.TokenStatusDisabled {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		if *req.Status == common.TokenStatusEnabled {
			if previous.Status == common.TokenStatusExpired && cleanToken.ExpiredTime != -1 && cleanToken.ExpiredTime <= common.GetTimestamp() {
				common.ApiErrorT(c, "Token has expired and cannot be enabled. Please modify the expiration time or set it to never expire")
				return
			}
			if previous.Status == common.TokenStatusExhausted && !cleanToken.UnlimitedQuota && cleanToken.RemainQuota <= 0 {
				common.ApiErrorT(c, "Token quota is exhausted and cannot be enabled. Please modify the remaining quota or set it to unlimited")
				return
			}
		}
		cleanToken.Status = *req.Status
	}

	// cleanToken.Update() invalidates the Redis token cache (with mutation fence) before persisting.
	if err := cleanToken.Update(); err != nil {
		common.ApiError(c, err)
		return
	}

	changedFields := make([]string, 0, 11)
	for _, field := range []struct {
		name    string
		changed bool
	}{
		{"name", previous.Name != cleanToken.Name},
		{"status", previous.Status != cleanToken.Status},
		{"group", previous.Group != cleanToken.Group},
		{"remain_quota", previous.RemainQuota != cleanToken.RemainQuota},
		{"unlimited_quota", previous.UnlimitedQuota != cleanToken.UnlimitedQuota},
		{"expired_time", previous.ExpiredTime != cleanToken.ExpiredTime},
		{"model_limits_enabled", previous.ModelLimitsEnabled != cleanToken.ModelLimitsEnabled},
		{"model_limits", previous.ModelLimits != cleanToken.ModelLimits},
		{"allow_ips", (previous.AllowIps == nil) != (cleanToken.AllowIps == nil) ||
			(previous.AllowIps != nil && cleanToken.AllowIps != nil && *previous.AllowIps != *cleanToken.AllowIps)},
		{"cross_group_retry", previous.CrossGroupRetry != cleanToken.CrossGroupRetry},
		{"auto_groups", previous.AutoGroups != cleanToken.AutoGroups},
	} {
		if field.changed {
			changedFields = append(changedFields, field.name)
		}
	}

	tokenDTO := buildAdminTokenDTO(cleanToken)
	recordAdminUserTokenAudit(
		c,
		targetUser,
		"admin.user_token_update",
		fmt.Sprintf("Updated API token %q (ID: %d) for user %s (ID: %d)", cleanToken.Name, cleanToken.Id, targetUser.Username, targetUser.Id),
		map[string]any{
			"token_id":       cleanToken.Id,
			"token_name":     cleanToken.Name,
			"masked_key":     tokenDTO.MaskedKey,
			"changed_fields": changedFields,
		},
	)

	common.ApiSuccess(c, tokenDTO)
}

func AdminDeleteUserToken(c *gin.Context) {
	targetUser, ok := loadManageableTokenTargetUser(c)
	if !ok {
		return
	}

	tokenID, err := strconv.Atoi(c.Param("token_id"))
	if err != nil || tokenID <= 0 {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}

	token, err := model.GetTokenByIds(tokenID, targetUser.Id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorT(c, "Token does not exist")
			return
		}
		common.ApiError(c, err)
		return
	}

	maskedKey := maskAdminTokenKey(token.Key)
	// token.Delete() invalidates the Redis token cache (with mutation fence) before deleting.
	if err := token.Delete(); err != nil {
		common.ApiError(c, err)
		return
	}

	recordAdminUserTokenAudit(
		c,
		targetUser,
		"admin.user_token_delete",
		fmt.Sprintf("Deleted API token %q (ID: %d) for user %s (ID: %d)", token.Name, token.Id, targetUser.Username, targetUser.Id),
		map[string]any{
			"token_id":   token.Id,
			"token_name": token.Name,
			"masked_key": maskedKey,
		},
	)

	common.ApiSuccess(c, nil)
}
