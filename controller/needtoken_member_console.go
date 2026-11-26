package controller

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

const (
	needTokenMemberConsoleDomain       = "needtoken:member-console:v1"
	needTokenMemberConsoleMaxBodyBytes = 16 * 1024
	needTokenMemberConsoleMaxSkewSec   = int64(60)
)

type memberConsoleTokenDTO struct {
	Id                 int      `json:"id"`
	UserId             int      `json:"user_id"`
	Name               string   `json:"name"`
	Key                string   `json:"key"`
	MaskedKey          string   `json:"masked_key"`
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
}

type memberConsoleLogDTO struct {
	Id               int    `json:"id"`
	CreatedAt        int64  `json:"created_at"`
	Type             int    `json:"type"`
	ModelName        string `json:"model_name"`
	TokenName        string `json:"token_name"`
	Group            string `json:"group"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Quota            int    `json:"quota"`
	UseTime          int    `json:"use_time"`
	IsStream         bool   `json:"is_stream"`
	Content          string `json:"content"`
}

type memberConsoleUsageDailyDTO struct {
	Date      string `json:"date"`
	Quota     int64  `json:"quota"`
	Requests  int64  `json:"requests"`
	TokenUsed int64  `json:"token_used"`
}

type memberConsoleUsageModelDTO struct {
	ModelName string `json:"model_name"`
	Quota     int64  `json:"quota"`
	Requests  int64  `json:"requests"`
	TokenUsed int64  `json:"token_used"`
}

func computeMemberConsoleSignature(secret []byte, method, path, rawQuery, timestamp string, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(needTokenMemberConsoleDomain))
	mac.Write([]byte{0})
	mac.Write([]byte(strings.ToUpper(strings.TrimSpace(method))))
	mac.Write([]byte{0})
	mac.Write([]byte(path))
	mac.Write([]byte{0})
	mac.Write([]byte(rawQuery))
	mac.Write([]byte{0})
	mac.Write([]byte(timestamp))
	mac.Write([]byte{0})
	mac.Write(rawBody)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func verifyMemberConsoleRequest(c *gin.Context) (*model.User, []byte, bool) {
	c.Header("Cache-Control", "no-store")
	secret, err := accountLinkSecret()
	if err != nil {
		common.ApiErrorStatus(c, http.StatusServiceUnavailable, common.NewMessage("Member console bridge is not enabled"), gin.H{"code": "member_console_disabled"})
		return nil, nil, false
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, needTokenMemberConsoleMaxBodyBytes+1))
	if err != nil || len(rawBody) > needTokenMemberConsoleMaxBodyBytes {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request body"), gin.H{"code": "invalid_request"})
		return nil, nil, false
	}

	tsRaw := strings.TrimSpace(c.GetHeader("X-NeedToken-Timestamp"))
	sigRaw := strings.TrimSpace(c.GetHeader("X-NeedToken-Signature"))
	tsUnix, err := strconv.ParseInt(tsRaw, 10, 64)
	if err != nil || sigRaw == "" {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Missing authentication headers"), gin.H{"code": "unauthorized"})
		return nil, nil, false
	}

	skew := accountLinkNow().Unix() - tsUnix
	if skew < -needTokenMemberConsoleMaxSkewSec || skew > needTokenMemberConsoleMaxSkewSec {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Request timestamp expired"), gin.H{"code": "timestamp_expired"})
		return nil, nil, false
	}

	expected := computeMemberConsoleSignature(secret, c.Request.Method, c.Request.URL.Path, c.Request.URL.RawQuery, tsRaw, rawBody)
	if !hmac.Equal([]byte(strings.ToLower(sigRaw)), []byte(strings.ToLower(expected))) {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Invalid signature"), gin.H{"code": "invalid_signature"})
		return nil, nil, false
	}

	userID, err := strconv.Atoi(strings.TrimSpace(c.Param("id")))
	if err != nil || userID <= 0 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid user ID"), gin.H{"code": "invalid_user_id"})
		return nil, nil, false
	}

	user, err := model.GetUserById(userID, false)
	if err != nil || user == nil || user.Id != userID {
		common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("User not found"), gin.H{"code": "user_not_found"})
		return nil, nil, false
	}
	if user.Role >= common.RoleAdminUser {
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Operation not permitted"), gin.H{"code": "admin_protected"})
		return nil, nil, false
	}
	return user, rawBody, true
}

func buildMemberConsoleTokenDTO(tok *model.Token) memberConsoleTokenDTO {
	autoGroups, _ := tok.GetAutoGroups()
	if len(autoGroups) == 0 {
		autoGroups = nil
	}
	fullKey := strings.TrimSpace(tok.Key)
	if fullKey != "" && !strings.HasPrefix(fullKey, "sk-") {
		fullKey = "sk-" + fullKey
	}
	return memberConsoleTokenDTO{
		Id:                 tok.Id,
		UserId:             tok.UserId,
		Name:               tok.Name,
		Key:                fullKey,
		MaskedKey:          maskAdminTokenKey(tok.Key),
		Status:             tok.Status,
		Group:              tok.Group,
		RemainQuota:        tok.RemainQuota,
		UsedQuota:          tok.UsedQuota,
		UnlimitedQuota:     tok.UnlimitedQuota,
		ExpiredTime:        tok.ExpiredTime,
		CreatedTime:        tok.CreatedTime,
		AccessedTime:       tok.AccessedTime,
		ModelLimitsEnabled: tok.ModelLimitsEnabled,
		ModelLimits:        tok.ModelLimits,
		AllowIps:           tok.AllowIps,
		CrossGroupRetry:    tok.CrossGroupRetry,
		AutoGroups:         autoGroups,
	}
}

type memberConsoleGroupMetaDTO struct {
	Code                string   `json:"code"`
	Description         string   `json:"description"`
	ChannelNames        []string `json:"channel_names"`
	DisplayName         string   `json:"display_name"`
	Ratio               float64  `json:"ratio"`
	EnabledChannelCount int      `json:"enabled_channel_count"`
	Models              []string `json:"models"`
}

func isGenericGroupDescription(code, desc string) bool {
	trimmed := strings.TrimSpace(desc)
	if trimmed == "" || strings.EqualFold(trimmed, strings.TrimSpace(code)) {
		return true
	}
	switch strings.ToLower(trimmed) {
	case "default group", "vip group", "user group", "auto group":
		return true
	default:
		return false
	}
}

func buildMemberConsoleGroupMeta(user *model.User, usableGroups map[string]string) (map[string]memberConsoleGroupMetaDTO, map[string]float64) {
	userGroup := "default"
	if user != nil {
		if trimmed := strings.TrimSpace(user.Group); trimmed != "" {
			userGroup = trimmed
		}
	}
	allRatios := ratio_setting.GetGroupRatioCopy()
	groupRatio := make(map[string]float64, len(usableGroups))
	groupMeta := make(map[string]memberConsoleGroupMetaDTO, len(usableGroups))

	type groupChannelAgg struct {
		channelNames []string
		channelSeen  map[string]struct{}
		modelSet     map[string]struct{}
		enabledCount int
	}
	aggs := make(map[string]*groupChannelAgg, len(usableGroups))

	for g, desc := range usableGroups {
		if g == "" || g == "auto" {
			continue
		}
		ratio := 1.0
		if override, exists := ratio_setting.GetGroupGroupRatio(userGroup, g); exists {
			ratio = override
		} else if baseRatio, exists := allRatios[g]; exists {
			ratio = baseRatio
		}
		groupRatio[g] = ratio
		aggs[g] = &groupChannelAgg{
			channelSeen: make(map[string]struct{}),
			modelSet:    make(map[string]struct{}),
		}
		groupMeta[g] = memberConsoleGroupMetaDTO{
			Code:         g,
			Description:  strings.TrimSpace(desc),
			ChannelNames: []string{},
			DisplayName:  strings.TrimSpace(desc),
			Ratio:        ratio,
			Models:       []string{},
		}
	}

	if model.DB != nil && len(aggs) > 0 {
		var channels []*model.Channel
		if err := model.DB.Where("status = ?", common.ChannelStatusEnabled).Order("priority desc, id asc").Find(&channels).Error; err == nil {
			for _, ch := range channels {
				if ch == nil {
					continue
				}
				chName := strings.TrimSpace(ch.Name)
				chModels := ch.GetModels()
				for _, rawG := range ch.GetGroups() {
					g := strings.TrimSpace(rawG)
					agg, ok := aggs[g]
					if !ok {
						continue
					}
					agg.enabledCount++
					if chName != "" {
						if _, seen := agg.channelSeen[chName]; !seen {
							agg.channelSeen[chName] = struct{}{}
							agg.channelNames = append(agg.channelNames, chName)
						}
					}
					for _, rawM := range chModels {
						m := strings.TrimSpace(rawM)
						if m != "" {
							agg.modelSet[m] = struct{}{}
						}
					}
				}
			}
		}
	}

	for g, meta := range groupMeta {
		agg := aggs[g]
		if agg != nil {
			meta.EnabledChannelCount = agg.enabledCount
			if len(agg.channelNames) > 0 {
				meta.ChannelNames = agg.channelNames
			}
			if len(agg.modelSet) > 0 {
				models := make([]string, 0, len(agg.modelSet))
				for m := range agg.modelSet {
					models = append(models, m)
				}
				slices.Sort(models)
				meta.Models = models
			}
		}
		switch {
		case len(meta.ChannelNames) == 1:
			meta.DisplayName = meta.ChannelNames[0]
		case !isGenericGroupDescription(meta.Code, meta.Description):
			meta.DisplayName = meta.Description
		case len(meta.ChannelNames) > 0:
			meta.DisplayName = meta.ChannelNames[0]
		case meta.Description != "":
			meta.DisplayName = meta.Description
		default:
			meta.DisplayName = meta.Code
		}
		groupMeta[g] = meta
	}

	return groupMeta, groupRatio
}

func memberConsoleUsableGroups(user *model.User) map[string]string {
	userId := 0
	userGroup := "default"
	if user != nil {
		userId = user.Id
		if trimmed := strings.TrimSpace(user.Group); trimmed != "" {
			userGroup = trimmed
		}
	}
	out := service.GetUserEffectiveUsableGroups(userId, userGroup)
	if len(out) == 0 {
		out = map[string]string{userGroup: setting.GetUsableGroupDescription(userGroup)}
	}
	return out
}

func validateMemberTokenGroup(user *model.User, group string) bool {
	trimmed := strings.TrimSpace(group)
	if trimmed == "" {
		return true
	}
	if user != nil && trimmed == strings.TrimSpace(user.Group) {
		return true
	}
	usable := memberConsoleUsableGroups(user)
	_, ok := usable[trimmed]
	return ok
}

func sanitizeMemberAutoGroups(user *model.User, rawGroups []string) ([]string, bool) {
	usable := memberConsoleUsableGroups(user)
	userGroup := ""
	if user != nil {
		userGroup = strings.TrimSpace(user.Group)
	}
	maxCount := setting.GetMaxTokenAutoGroups()
	cleaned := make([]string, 0, len(rawGroups))
	seen := make(map[string]struct{}, len(rawGroups))
	for _, raw := range rawGroups {
		g := strings.TrimSpace(raw)
		if g == "" {
			continue
		}
		if g == "auto" {
			return nil, false
		}
		if _, dup := seen[g]; dup {
			continue
		}
		if g != userGroup {
			if _, ok := usable[g]; !ok {
				return nil, false
			}
		}
		if !ratio_setting.ContainsGroupRatio(g) {
			return nil, false
		}
		seen[g] = struct{}{}
		cleaned = append(cleaned, g)
		if maxCount > 0 && len(cleaned) > maxCount {
			return nil, false
		}
	}
	return cleaned, len(cleaned) > 0
}

func MemberConsoleListTokens(c *gin.Context) {
	user, _, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	tokens, err := model.GetAllUserTokens(user.Id, 0, 200)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	items := make([]memberConsoleTokenDTO, 0, len(tokens))
	for _, tok := range tokens {
		if tok == nil {
			continue
		}
		items = append(items, buildMemberConsoleTokenDTO(tok))
	}

	activeSubs, err := model.GetAllActiveUserSubscriptions(user.Id)
	if err != nil {
		activeSubs = []model.SubscriptionSummary{}
	}

	usableGroups := memberConsoleUsableGroups(user)
	groupMeta, groupRatio := buildMemberConsoleGroupMeta(user, usableGroups)

	common.ApiSuccess(c, gin.H{
		"items":                items,
		"total":                len(items),
		"usable_groups":        usableGroups,
		"group_ratio":          groupRatio,
		"group_meta":           groupMeta,
		"default_group":        user.Group,
		"default_auto_group":   setting.DefaultUseAutoGroup,
		"auto_groups":          service.GetUserEffectiveAutoGroup(user.Id, user.Group),
		"quota_per_unit":       common.QuotaPerUnit,
		"active_subscriptions": activeSubs,
	})
}

func MemberConsoleCreateToken(c *gin.Context) {
	user, rawBody, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}
	if user.Status != common.UserStatusEnabled {
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("User account is disabled"), gin.H{"code": "user_disabled"})
		return
	}

	var req needTokenAdminTokenRequest
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}

	name := ""
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if name == "" {
		name = "API Key"
	}
	if len(name) > 50 {
		common.ApiErrorT(c, "Token name is too long")
		return
	}

	unlimitedQuota := true
	if req.UnlimitedQuota != nil {
		unlimitedQuota = *req.UnlimitedQuota
	}
	remainQuota := 0
	if req.RemainQuota != nil {
		remainQuota = *req.RemainQuota
	}
	if !unlimitedQuota && remainQuota < 0 {
		common.ApiErrorT(c, "Quota cannot be negative")
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
	count, err := model.CountUserTokens(user.Id)
	if err != nil {
		common.ApiError(c, err)
		return
	}
	if maxTokens > 0 && int(count) >= maxTokens {
		common.ApiErrorT(c, "Maximum number of tokens reached ({{max}})", map[string]any{"max": maxTokens})
		return
	}

	group := ""
	if req.Group != nil {
		group = strings.TrimSpace(*req.Group)
	}
	if group == "" && setting.DefaultUseAutoGroup {
		group = "auto"
	}
	var explicitAutoGroups []string
	if group == "auto" && len(req.AutoGroups.Groups) > 0 {
		cleaned, okGroups := sanitizeMemberAutoGroups(user, req.AutoGroups.Groups)
		if !okGroups {
			common.ApiErrorT(c, "Invalid group")
			return
		}
		if len(cleaned) == 1 {
			group = cleaned[0]
		} else {
			explicitAutoGroups = cleaned
		}
	} else if !validateMemberTokenGroup(user, group) {
		common.ApiErrorT(c, "Invalid group")
		return
	}

	modelLimitsEnabled := false
	if req.ModelLimitsEnabled != nil {
		modelLimitsEnabled = *req.ModelLimitsEnabled
	}
	modelLimits := ""
	if req.ModelLimits != nil {
		modelLimits = strings.TrimSpace(*req.ModelLimits)
	}
	crossGroupRetry := false
	if group == "auto" {
		if req.CrossGroupRetry != nil {
			crossGroupRetry = *req.CrossGroupRetry
		} else if len(explicitAutoGroups) > 1 {
			crossGroupRetry = true
		}
	}

	now := common.GetTimestamp()
	cleanToken := model.Token{
		UserId:              user.Id,
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
	if cleanToken.Group == "auto" && len(explicitAutoGroups) > 0 {
		_ = cleanToken.SetAutoGroups(explicitAutoGroups)
	}

	rawKey, err := common.GenerateKey()
	if err != nil {
		common.ApiErrorT(c, "Failed to generate token")
		return
	}
	cleanToken.Key = rawKey
	if err := cleanToken.Insert(); err != nil {
		common.ApiError(c, err)
		return
	}

	dto := buildMemberConsoleTokenDTO(&cleanToken)
	common.ApiSuccess(c, gin.H{
		"key":   dto.Key,
		"token": dto,
	})
}

func MemberConsoleUpdateToken(c *gin.Context) {
	user, rawBody, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	tokenID, err := strconv.Atoi(strings.TrimSpace(c.Param("token_id")))
	if err != nil || tokenID <= 0 {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}

	var req needTokenAdminTokenRequest
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}

	cleanToken, err := model.GetTokenByIds(tokenID, user.Id)
	if err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Token does not exist"), gin.H{"code": "token_not_found"})
			return
		}
		common.ApiError(c, err)
		return
	}

	previous := *cleanToken
	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" || len(name) > 50 {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		cleanToken.Name = name
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
		if *req.RemainQuota < 0 {
			common.ApiErrorT(c, "Quota cannot be negative")
			return
		}
		cleanToken.RemainQuota = *req.RemainQuota
	}
	if req.ModelLimitsEnabled != nil {
		cleanToken.ModelLimitsEnabled = *req.ModelLimitsEnabled
	}
	if req.ModelLimits != nil {
		cleanToken.ModelLimits = strings.TrimSpace(*req.ModelLimits)
	}
	if req.AllowIps != nil {
		cleanToken.AllowIps = req.AllowIps
	}
	var explicitAutoGroups []string
	explicitAutoSet := false
	if req.Group != nil {
		group := strings.TrimSpace(*req.Group)
		if group == "auto" && req.AutoGroups.Set && len(req.AutoGroups.Groups) > 0 {
			cleaned, okGroups := sanitizeMemberAutoGroups(user, req.AutoGroups.Groups)
			if !okGroups {
				common.ApiErrorT(c, "Invalid group")
				return
			}
			if len(cleaned) == 1 {
				cleanToken.Group = cleaned[0]
			} else {
				cleanToken.Group = "auto"
				explicitAutoGroups = cleaned
				explicitAutoSet = true
			}
		} else {
			if !validateMemberTokenGroup(user, group) {
				common.ApiErrorT(c, "Invalid group")
				return
			}
			cleanToken.Group = group
		}
	} else if cleanToken.Group == "auto" && req.AutoGroups.Set {
		if len(req.AutoGroups.Groups) == 0 {
			if !validateMemberTokenGroup(user, "auto") {
				common.ApiErrorT(c, "Invalid group")
				return
			}
			explicitAutoSet = true
		} else {
			cleaned, okGroups := sanitizeMemberAutoGroups(user, req.AutoGroups.Groups)
			if !okGroups {
				common.ApiErrorT(c, "Invalid group")
				return
			}
			if len(cleaned) == 1 {
				cleanToken.Group = cleaned[0]
			} else {
				explicitAutoGroups = cleaned
				explicitAutoSet = true
			}
		}
	}
	if cleanToken.Group != "auto" {
		cleanToken.CrossGroupRetry = false
		_ = cleanToken.SetAutoGroups(nil)
	} else {
		if req.CrossGroupRetry != nil {
			cleanToken.CrossGroupRetry = *req.CrossGroupRetry
		} else if explicitAutoSet && len(explicitAutoGroups) > 1 {
			cleanToken.CrossGroupRetry = true
		}
		if explicitAutoSet {
			_ = cleanToken.SetAutoGroups(explicitAutoGroups)
		} else if req.AutoGroups.Set {
			_ = cleanToken.SetAutoGroups(nil)
		}
	}
	if req.Status != nil {
		if *req.Status != common.TokenStatusEnabled && *req.Status != common.TokenStatusDisabled {
			common.ApiErrorT(c, "Invalid parameters")
			return
		}
		if *req.Status == common.TokenStatusEnabled {
			if previous.Status == common.TokenStatusExpired && cleanToken.ExpiredTime != -1 && cleanToken.ExpiredTime <= common.GetTimestamp() {
				common.ApiErrorT(c, "Token has expired and cannot be enabled")
				return
			}
			if previous.Status == common.TokenStatusExhausted && !cleanToken.UnlimitedQuota && cleanToken.RemainQuota <= 0 {
				common.ApiErrorT(c, "Token quota is exhausted and cannot be enabled")
				return
			}
		}
		cleanToken.Status = *req.Status
	}

	if err := cleanToken.Update(); err != nil {
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, buildMemberConsoleTokenDTO(cleanToken))
}

func MemberConsoleDeleteToken(c *gin.Context) {
	user, _, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	tokenID, err := strconv.Atoi(strings.TrimSpace(c.Param("token_id")))
	if err != nil || tokenID <= 0 {
		common.ApiErrorT(c, "Invalid parameters")
		return
	}

	if err := model.DeleteTokenById(tokenID, user.Id); err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			common.ApiErrorStatus(c, http.StatusNotFound, common.NewMessage("Token does not exist"), gin.H{"code": "token_not_found"})
			return
		}
		common.ApiError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"deleted": true, "token_id": tokenID})
}

func MemberConsoleListLogs(c *gin.Context) {
	user, _, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	if page <= 0 {
		page = 1
	}
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pageSize <= 0 {
		pageSize = 20
	}
	pageSize = min(pageSize, 100)

	filter := logFilterFromQuery(c, false)
	if startTimestamp, err := limitLogTimeRange(filter.StartTimestamp, filter.EndTimestamp); err == nil {
		filter.StartTimestamp = startTimestamp
	} else {
		common.ApiError(c, err)
		return
	}

	logs, total, err := model.GetUserLogs(user.Id, filter, (page-1)*pageSize, pageSize)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	items := make([]memberConsoleLogDTO, 0, len(logs))
	for _, row := range logs {
		if row == nil {
			continue
		}
		items = append(items, memberConsoleLogDTO{
			Id:               row.Id,
			CreatedAt:        row.CreatedAt,
			Type:             row.Type,
			ModelName:        row.ModelName,
			TokenName:        row.TokenName,
			Group:            row.Group,
			PromptTokens:     row.PromptTokens,
			CompletionTokens: row.CompletionTokens,
			Quota:            row.Quota,
			UseTime:          row.UseTime,
			IsStream:         row.IsStream,
			Content:          row.Content,
		})
	}

	common.ApiSuccess(c, gin.H{
		"items":          items,
		"total":          total,
		"page":           page,
		"page_size":      pageSize,
		"quota_per_unit": common.QuotaPerUnit,
	})
}

func MemberConsoleUsage(c *gin.Context) {
	user, _, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	days, _ := strconv.Atoi(c.DefaultQuery("days", "7"))
	if days <= 0 {
		days = 7
	}
	days = min(days, 30)

	now := accountLinkNow().UTC()
	endTS := now.Unix()
	startTS := now.AddDate(0, 0, -days).Unix()
	if rawStart, err := strconv.ParseInt(strings.TrimSpace(c.Query("start_timestamp")), 10, 64); err == nil && rawStart > 0 {
		startTS = rawStart
	}
	if rawEnd, err := strconv.ParseInt(strings.TrimSpace(c.Query("end_timestamp")), 10, 64); err == nil && rawEnd > 0 {
		endTS = rawEnd
	}
	if limited, err := limitLogTimeRange(startTS, endTS); err == nil {
		startTS = limited
	}

	rows, err := model.GetQuotaDataByUserId(user.Id, startTS, endTS)
	if err != nil {
		common.ApiError(c, err)
		return
	}

	dailyMap := make(map[string]*memberConsoleUsageDailyDTO)
	modelMap := make(map[string]*memberConsoleUsageModelDTO)
	var totalQuota, totalRequests, totalTokens int64

	for _, row := range rows {
		if row == nil {
			continue
		}
		q := int64(row.Quota)
		cnt := int64(row.Count)
		toks := int64(row.TokenUsed)
		totalQuota += q
		totalRequests += cnt
		totalTokens += toks

		dayKey := time.Unix(row.CreatedAt, 0).UTC().Format("2006-01-02")
		d := dailyMap[dayKey]
		if d == nil {
			d = &memberConsoleUsageDailyDTO{Date: dayKey}
			dailyMap[dayKey] = d
		}
		d.Quota += q
		d.Requests += cnt
		d.TokenUsed += toks

		modelKey := strings.TrimSpace(row.ModelName)
		if modelKey == "" {
			modelKey = "unknown"
		}
		m := modelMap[modelKey]
		if m == nil {
			m = &memberConsoleUsageModelDTO{ModelName: modelKey}
			modelMap[modelKey] = m
		}
		m.Quota += q
		m.Requests += cnt
		m.TokenUsed += toks
	}

	daily := make([]memberConsoleUsageDailyDTO, 0, len(dailyMap))
	for _, v := range dailyMap {
		daily = append(daily, *v)
	}
	slices.SortFunc(daily, func(a, b memberConsoleUsageDailyDTO) int {
		return strings.Compare(a.Date, b.Date)
	})

	models := make([]memberConsoleUsageModelDTO, 0, len(modelMap))
	for _, v := range modelMap {
		models = append(models, *v)
	}
	slices.SortFunc(models, func(a, b memberConsoleUsageModelDTO) int {
		if a.Quota != b.Quota {
			if a.Quota > b.Quota {
				return -1
			}
			return 1
		}
		return strings.Compare(a.ModelName, b.ModelName)
	})

	common.ApiSuccess(c, gin.H{
		"days":           days,
		"start_ts":       startTS,
		"end_ts":         endTS,
		"total_quota":    totalQuota,
		"total_requests": totalRequests,
		"total_tokens":   totalTokens,
		"quota_per_unit": common.QuotaPerUnit,
		"daily":          daily,
		"models":         models,
	})
}

type memberConsoleDrawingDTO struct {
	Id         int    `json:"id"`
	MjId       string `json:"mj_id"`
	Action     string `json:"action"`
	Status     string `json:"status"`
	Progress   string `json:"progress"`
	Prompt     string `json:"prompt"`
	PromptEn   string `json:"prompt_en"`
	FailReason string `json:"fail_reason"`
	ImageUrl   string `json:"image_url"`
	Quota      int    `json:"quota"`
	SubmitTime int64  `json:"submit_time"`
	FinishTime int64  `json:"finish_time"`
}

func MemberConsoleModels(c *gin.Context) {
	user, _, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	group := strings.TrimSpace(user.Group)
	usableGroup := memberConsoleUsableGroups(user)
	delete(usableGroup, "auto")
	groupMeta, groupRatio := buildMemberConsoleGroupMeta(user, usableGroup)

	pricing := filterPricingByUsableGroups(model.GetPricing(), usableGroup)
	common.ApiSuccess(c, gin.H{
		"models":             pricing,
		"vendors":            model.GetVendors(),
		"usable_groups":      usableGroup,
		"group_ratio":        groupRatio,
		"group_meta":         groupMeta,
		"default_group":      group,
		"supported_endpoint": model.GetSupportedEndpointMap(),
		"auto_groups":        service.GetUserEffectiveAutoGroup(user.Id, group),
		"quota_per_unit":     common.QuotaPerUnit,
	})
}

func MemberConsoleListTasks(c *gin.Context) {
	user, _, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	page = max(page, 1)
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pageSize <= 0 {
		pageSize = 20
	}
	pageSize = min(pageSize, 100)

	startTS, _ := strconv.ParseInt(strings.TrimSpace(c.Query("start_timestamp")), 10, 64)
	endTS, _ := strconv.ParseInt(strings.TrimSpace(c.Query("end_timestamp")), 10, 64)
	queryParams := model.SyncTaskQueryParams{
		Platform:       constant.TaskPlatform(strings.TrimSpace(c.Query("platform"))),
		TaskID:         strings.TrimSpace(c.Query("task_id")),
		Status:         strings.TrimSpace(c.Query("status")),
		Action:         strings.TrimSpace(c.Query("action")),
		StartTimestamp: startTS,
		EndTimestamp:   endTS,
	}

	items := model.TaskGetAllUserTask(user.Id, (page-1)*pageSize, pageSize, queryParams)
	total := model.TaskCountAllUserTask(user.Id, queryParams)
	common.ApiSuccess(c, gin.H{
		"items":          tasksToDto(items, false, common.RoleCommonUser),
		"total":          total,
		"page":           page,
		"page_size":      pageSize,
		"quota_per_unit": common.QuotaPerUnit,
	})
}

func MemberConsoleListDrawings(c *gin.Context) {
	user, _, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	page, _ := strconv.Atoi(c.DefaultQuery("p", "1"))
	page = max(page, 1)
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if pageSize <= 0 {
		pageSize = 20
	}
	pageSize = min(pageSize, 100)

	queryParams := model.TaskQueryParams{
		MjID:           strings.TrimSpace(c.Query("mj_id")),
		StartTimestamp: strings.TrimSpace(c.Query("start_timestamp")),
		EndTimestamp:   strings.TrimSpace(c.Query("end_timestamp")),
	}
	rawItems := model.GetAllUserTask(user.Id, (page-1)*pageSize, pageSize, queryParams)
	total := model.CountAllUserTask(user.Id, queryParams)

	items := make([]memberConsoleDrawingDTO, 0, len(rawItems))
	for _, row := range rawItems {
		if row == nil {
			continue
		}
		imgURL := row.ImageUrl
		if setting.MjForwardUrlEnabled && row.MjId != "" {
			imgURL = system_setting.ServerAddress + "/mj/image/" + row.MjId
		}
		items = append(items, memberConsoleDrawingDTO{
			Id:         row.Id,
			MjId:       row.MjId,
			Action:     row.Action,
			Status:     row.Status,
			Progress:   row.Progress,
			Prompt:     row.Prompt,
			PromptEn:   row.PromptEn,
			FailReason: row.FailReason,
			ImageUrl:   imgURL,
			Quota:      row.Quota,
			SubmitTime: row.SubmitTime,
			FinishTime: row.FinishTime,
		})
	}

	common.ApiSuccess(c, gin.H{
		"items":          items,
		"total":          total,
		"page":           page,
		"page_size":      pageSize,
		"quota_per_unit": common.QuotaPerUnit,
	})
}

func MemberConsoleSubscriptions(c *gin.Context) {
	user, _, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}

	activeSubs, err := model.GetAllActiveUserSubscriptions(user.Id)
	if err != nil {
		activeSubs = []model.SubscriptionSummary{}
	}
	allSubs, err := model.GetAllUserSubscriptions(user.Id)
	if err != nil {
		allSubs = []model.SubscriptionSummary{}
	}

	usableGroups := memberConsoleUsableGroups(user)
	groupMeta, groupRatio := buildMemberConsoleGroupMeta(user, usableGroups)

	common.ApiSuccess(c, gin.H{
		"active_subscriptions": activeSubs,
		"all_subscriptions":    allSubs,
		"user_group":           user.Group,
		"user_quota":           user.Quota,
		"usable_groups":        usableGroups,
		"group_ratio":          groupRatio,
		"group_meta":           groupMeta,
		"quota_per_unit":       common.QuotaPerUnit,
	})
}
