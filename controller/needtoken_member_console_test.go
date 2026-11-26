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
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestMemberConsoleLifecycleAndSecurity(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const secret = "0123456789abcdef0123456789abcdef0123456789abcdef"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	origDB := model.DB
	origLogDB := model.LOG_DB
	origNow := accountLinkNow
	origRedis := common.RedisEnabled
	common.RedisEnabled = false
	db, err := gorm.Open(sqlite.Open("file:member_console_test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.QuotaData{}, &model.UserSubscription{}, &model.SubscriptionPlan{}, &model.Task{}, &model.Midjourney{}))
	model.DB = db
	model.LOG_DB = db
	fixedNow := time.Now().UTC().Truncate(time.Second)
	accountLinkNow = func() time.Time { return fixedNow }
	t.Cleanup(func() {
		model.DB = origDB
		model.LOG_DB = origLogDB
		accountLinkNow = origNow
		common.RedisEnabled = origRedis
	})

	member := &model.User{Username: "member_a", DisplayName: "Member A", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "aff_a"}
	other := &model.User{Username: "member_b", DisplayName: "Member B", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "aff_b"}
	admin := &model.User{Username: "root_admin", DisplayName: "Root", Role: common.RoleAdminUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "aff_c"}
	require.NoError(t, db.Create(member).Error)
	require.NoError(t, db.Create(other).Error)
	require.NoError(t, db.Create(admin).Error)

	r := gin.New()
	grp := r.Group("/api/subscription/admin/needtoken-console")
	grp.GET("/users/:id/tokens", MemberConsoleListTokens)
	grp.POST("/users/:id/tokens", MemberConsoleCreateToken)
	grp.PUT("/users/:id/tokens/:token_id", MemberConsoleUpdateToken)
	grp.DELETE("/users/:id/tokens/:token_id", MemberConsoleDeleteToken)
	grp.GET("/users/:id/logs", MemberConsoleListLogs)
	grp.GET("/users/:id/usage", MemberConsoleUsage)
	grp.GET("/users/:id/models", MemberConsoleModels)
	grp.GET("/users/:id/tasks", MemberConsoleListTasks)
	grp.GET("/users/:id/drawings", MemberConsoleListDrawings)
	grp.GET("/users/:id/subscriptions", MemberConsoleSubscriptions)

	doSigned := func(method, target string, body string, ts int64, overrideSig string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		tsStr := strconv.FormatInt(ts, 10)
		sig := overrideSig
		if sig == "" {
			sig = computeMemberConsoleSignature([]byte(secret), method, req.URL.Path, req.URL.RawQuery, tsStr, []byte(body))
		}
		req.Header.Set("X-NeedToken-Timestamp", tsStr)
		req.Header.Set("X-NeedToken-Signature", sig)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1. Security checks: bad signature, expired timestamp, admin user blocked.
	badSig := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens", "", fixedNow.Unix(), "sha256=deadbeef")
	assert.Equal(t, http.StatusUnauthorized, badSig.Code)

	expired := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens", "", fixedNow.Unix()-120, "")
	assert.Equal(t, http.StatusUnauthorized, expired.Code)

	adminBlocked := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(admin.Id)+"/tokens", "", fixedNow.Unix(), "")
	assert.Equal(t, http.StatusForbidden, adminBlocked.Code)

	// 2. Create token for member.
	createRec := doSigned(
		http.MethodPost,
		"/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens",
		`{"name":"Claude Code","unlimited_quota":false,"remain_quota":2500000,"group":"default"}`,
		fixedNow.Unix(),
		"",
	)
	require.Equal(t, http.StatusOK, createRec.Code)
	var createResp struct {
		Success bool `json:"success"`
		Data    struct {
			Key   string                `json:"key"`
			Token memberConsoleTokenDTO `json:"token"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(createRec.Body.Bytes(), &createResp))
	require.True(t, createResp.Success)
	assert.True(t, len(createResp.Data.Key) > 10)
	assert.Equal(t, "Claude Code", createResp.Data.Token.Name)
	assert.Equal(t, 2500000, createResp.Data.Token.RemainQuota)
	tokenID := createResp.Data.Token.Id
	require.Greater(t, tokenID, 0)

	// 3. List tokens returns created token with sk- key and usable groups.
	listRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens", "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, listRec.Code)
	assert.Equal(t, "no-store", listRec.Header().Get("Cache-Control"))
	var listResp struct {
		Success bool `json:"success"`
		Data    struct {
			Items        []memberConsoleTokenDTO `json:"items"`
			UsableGroups map[string]string       `json:"usable_groups"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(listRec.Body.Bytes(), &listResp))
	require.Len(t, listResp.Data.Items, 1)
	assert.Equal(t, createResp.Data.Key, listResp.Data.Items[0].Key)
	assert.NotEmpty(t, listResp.Data.UsableGroups)

	// 4. Cross-user isolation: other user cannot update or delete member's token.
	crossUpd := doSigned(
		http.MethodPut,
		"/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(other.Id)+"/tokens/"+strconv.Itoa(tokenID),
		`{"name":"Hijacked"}`,
		fixedNow.Unix(),
		"",
	)
	assert.Equal(t, http.StatusNotFound, crossUpd.Code)

	// 5. Update token name & status for owner.
	updRec := doSigned(
		http.MethodPut,
		"/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens/"+strconv.Itoa(tokenID),
		`{"name":"Cursor Key","status":2}`,
		fixedNow.Unix(),
		"",
	)
	require.Equal(t, http.StatusOK, updRec.Code)
	var updResp struct {
		Success bool                  `json:"success"`
		Data    memberConsoleTokenDTO `json:"data"`
	}
	require.NoError(t, common.Unmarshal(updRec.Body.Bytes(), &updResp))
	assert.Equal(t, "Cursor Key", updResp.Data.Name)
	assert.Equal(t, common.TokenStatusDisabled, updResp.Data.Status)

	// 6. Seed logs & quota data and query logs + usage.
	require.NoError(t, db.Create(&model.Log{
		UserId:           member.Id,
		CreatedAt:        fixedNow.Unix() - 600,
		Type:             model.LogTypeConsume,
		ModelName:        "claude-sonnet-4-6",
		TokenName:        "Cursor Key",
		Group:            "default",
		PromptTokens:     1200,
		CompletionTokens: 450,
		Quota:            125000,
		UseTime:          3,
		ChannelId:        99,
		ChannelName:      "secret-internal-channel",
	}).Error)
	require.NoError(t, db.Create(&model.QuotaData{
		UserID:    member.Id,
		Username:  member.Username,
		ModelName: "claude-sonnet-4-6",
		CreatedAt: fixedNow.Unix() - 3600,
		TokenUsed: 1650,
		Count:     2,
		Quota:     250000,
	}).Error)

	logsRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/logs?p=1&page_size=20&model_name=claude-sonnet-4-6", "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, logsRec.Code)
	assert.NotContains(t, logsRec.Body.String(), "secret-internal-channel")
	var logsResp struct {
		Success bool `json:"success"`
		Data    struct {
			Items []memberConsoleLogDTO `json:"items"`
			Total int64                 `json:"total"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(logsRec.Body.Bytes(), &logsResp))
	require.Equal(t, int64(1), logsResp.Data.Total)
	assert.Equal(t, "claude-sonnet-4-6", logsResp.Data.Items[0].ModelName)

	usageRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/usage?days=7", "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, usageRec.Code)
	var usageResp struct {
		Success bool `json:"success"`
		Data    struct {
			TotalQuota    int64                        `json:"total_quota"`
			TotalRequests int64                        `json:"total_requests"`
			TotalTokens   int64                        `json:"total_tokens"`
			Daily         []memberConsoleUsageDailyDTO `json:"daily"`
			Models        []memberConsoleUsageModelDTO `json:"models"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(usageRec.Body.Bytes(), &usageResp))
	assert.Equal(t, int64(250000), usageResp.Data.TotalQuota)
	assert.Equal(t, int64(2), usageResp.Data.TotalRequests)
	require.Len(t, usageResp.Data.Models, 1)
	assert.Equal(t, "claude-sonnet-4-6", usageResp.Data.Models[0].ModelName)

	// 7. Models, tasks, drawings, and subscriptions endpoints.
	require.NoError(t, db.Create(&model.Task{
		TaskID:    "task_vid_001",
		UserId:    member.Id,
		ChannelId: 99,
		Platform:  "kling",
		Action:    "text2video",
		Status:    model.TaskStatusSuccess,
		Progress:  "100%",
		Quota:     50000,
		CreatedAt: fixedNow.Unix() - 120,
	}).Error)
	require.NoError(t, db.Create(&model.Midjourney{
		MjId:       "mj_draw_001",
		UserId:     member.Id,
		ChannelId:  99,
		Action:     "IMAGINE",
		Status:     "SUCCESS",
		Progress:   "100%",
		Prompt:     "emerald skyline",
		Quota:      25000,
		SubmitTime: fixedNow.Unix() - 180,
	}).Error)

	modelsRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/models", "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, modelsRec.Code)
	assert.Contains(t, modelsRec.Body.String(), `"usable_groups"`)

	tasksRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tasks?p=1&page_size=20", "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, tasksRec.Code)
	assert.Contains(t, tasksRec.Body.String(), `"task_vid_001"`)

	drawingsRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/drawings?p=1&page_size=20", "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, drawingsRec.Code)
	assert.Contains(t, drawingsRec.Body.String(), `"mj_draw_001"`)
	assert.NotContains(t, drawingsRec.Body.String(), `"channel_id"`)

	subsRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/subscriptions", "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, subsRec.Code)
	assert.Contains(t, subsRec.Body.String(), `"all_subscriptions"`)

	// 8. Delete token.
	delRec := doSigned(http.MethodDelete, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens/"+strconv.Itoa(tokenID), "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, delRec.Code)
}

func TestMemberConsoleGroupIsolationAndSubscriptionUnlock(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const secret = "fedcba9876543210fedcba9876543210fedcba9876543210"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	origDB := model.DB
	origNow := accountLinkNow
	origUsable := setting.UserUsableGroups2JSONString()
	origRatios := ratio_setting.GroupRatio2JSONString()
	origAutoGroups := setting.GetAutoGroups()
	origDefaultAuto := setting.DefaultUseAutoGroup

	db, err := gorm.Open(sqlite.Open("file:member_group_isolation_test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}))
	model.DB = db
	fixedNow := time.Now().UTC().Truncate(time.Second)
	accountLinkNow = func() time.Time { return fixedNow }

	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default group"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":0.8,"openai-exclusive-1":0.5}`))
	setting.DefaultUseAutoGroup = false
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["openai-exclusive-1","default"]`))

	t.Cleanup(func() {
		model.DB = origDB
		accountLinkNow = origNow
		_ = setting.UpdateUserUsableGroupsByJSONString(origUsable)
		_ = ratio_setting.UpdateGroupRatioByJSONString(origRatios)
		setting.DefaultUseAutoGroup = origDefaultAuto
		rawAuto, _ := common.Marshal(origAutoGroups)
		_ = setting.UpdateAutoGroupsByJsonString(string(rawAuto))
	})

	member := &model.User{Username: "std_member", DisplayName: "Std", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "aff_std"}
	require.NoError(t, db.Create(member).Error)

	r := gin.New()
	grp := r.Group("/api/subscription/admin/needtoken-console")
	grp.GET("/users/:id/tokens", MemberConsoleListTokens)
	grp.POST("/users/:id/tokens", MemberConsoleCreateToken)
	grp.GET("/users/:id/models", MemberConsoleModels)

	doSigned := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		tsStr := strconv.FormatInt(fixedNow.Unix(), 10)
		sig := computeMemberConsoleSignature([]byte(secret), method, req.URL.Path, req.URL.RawQuery, tsStr, []byte(body))
		req.Header.Set("X-NeedToken-Timestamp", tsStr)
		req.Header.Set("X-NeedToken-Signature", sig)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1. Standard default user sees only default; vip, auto, and exclusive groups are blocked.
	listRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens", "")
	require.Equal(t, http.StatusOK, listRec.Code)
	var listResp struct {
		Success bool `json:"success"`
		Data    struct {
			UsableGroups map[string]string `json:"usable_groups"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(listRec.Body.Bytes(), &listResp))
	assert.Contains(t, listResp.Data.UsableGroups, "default")
	assert.NotContains(t, listResp.Data.UsableGroups, "vip")
	assert.NotContains(t, listResp.Data.UsableGroups, "openai-exclusive-1")
	assert.NotContains(t, listResp.Data.UsableGroups, "auto")

	for _, forbiddenGroup := range []string{"vip", "openai-exclusive-1", "auto"} {
		rec := doSigned(http.MethodPost, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens", `{"name":"Try","group":"`+forbiddenGroup+`"}`)
		var resp struct {
			Success bool `json:"success"`
		}
		require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &resp))
		assert.False(t, resp.Success, "group %s must be rejected", forbiddenGroup)
		assert.False(t, service.UserCanUseGroup(member.Id, member.Group, forbiddenGroup))
	}

	// 2. Wildcard/all-group subscription (empty allowed_groups) must NOT unlock restricted exclusive groups.
	wildcardSub := &model.UserSubscription{
		UserId:        member.Id,
		PlanId:        1,
		Status:        "active",
		StartTime:     fixedNow.Unix() - 60,
		EndTime:       fixedNow.Unix() + 3600,
		AmountTotal:   1000000,
		AllowedGroups: "",
	}
	require.NoError(t, db.Create(wildcardSub).Error)
	assert.False(t, service.UserCanUseGroup(member.Id, member.Group, "openai-exclusive-1"))

	// 3. Explicit exclusive-group subscription unlocks group for token creation, models, auth, and auto routing.
	exclSub := &model.UserSubscription{
		UserId:        member.Id,
		PlanId:        2,
		Status:        "active",
		StartTime:     fixedNow.Unix() - 60,
		EndTime:       fixedNow.Unix() + 3600,
		AmountTotal:   2000000,
		AllowedGroups: "openai-exclusive-1",
	}
	require.NoError(t, db.Create(exclSub).Error)

	assert.True(t, service.UserCanUseGroup(member.Id, member.Group, "openai-exclusive-1"))
	assert.False(t, service.UserCanUseGroup(member.Id, member.Group, "vip"))
	assert.Equal(t, []string{"openai-exclusive-1", "default"}, service.GetUserEffectiveAutoGroup(member.Id, member.Group))

	createExcl := doSigned(http.MethodPost, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens", `{"name":"Exclusive Key","group":"openai-exclusive-1"}`)
	require.Equal(t, http.StatusOK, createExcl.Code)
	var createExclResp struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(createExcl.Body.Bytes(), &createExclResp))
	assert.True(t, createExclResp.Success)

	// Enabling DefaultUseAutoGroup exposes "auto" based on backend setting.
	setting.DefaultUseAutoGroup = true
	assert.True(t, service.UserCanUseGroup(member.Id, member.Group, "auto"))
	listWithAuto := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens", "")
	require.NoError(t, common.Unmarshal(listWithAuto.Body.Bytes(), &listResp))
	assert.Contains(t, listResp.Data.UsableGroups, "openai-exclusive-1")
	assert.Contains(t, listResp.Data.UsableGroups, "auto")
	assert.NotContains(t, listResp.Data.UsableGroups, "vip")

	// 4. Expiring the exclusive subscription revokes access automatically.
	require.NoError(t, db.Model(exclSub).Update("end_time", fixedNow.Unix()-10).Error)
	assert.False(t, service.UserCanUseGroup(member.Id, member.Group, "openai-exclusive-1"))
}

func TestMemberConsoleGroupMetaAndMultiGroupTokenGuard(t *testing.T) {
	gin.SetMode(gin.TestMode)

	const secret = "11223344556677889900aabbccddeeff1122334455667788"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	origDB := model.DB
	origNow := accountLinkNow
	origRedis := common.RedisEnabled
	origUsable := setting.UserUsableGroups2JSONString()
	origRatios := ratio_setting.GroupRatio2JSONString()
	origDefaultAuto := setting.DefaultUseAutoGroup
	common.RedisEnabled = false

	db, err := gorm.Open(sqlite.Open("file:member_group_meta_test?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.UserSubscription{}, &model.Channel{}))
	model.DB = db
	fixedNow := time.Now().UTC().Truncate(time.Second)
	accountLinkNow = func() time.Time { return fixedNow }

	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default group"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"ssvip":0.5,"vip":0.8}`))
	setting.DefaultUseAutoGroup = false

	t.Cleanup(func() {
		model.DB = origDB
		accountLinkNow = origNow
		common.RedisEnabled = origRedis
		_ = setting.UpdateUserUsableGroupsByJSONString(origUsable)
		_ = ratio_setting.UpdateGroupRatioByJSONString(origRatios)
		setting.DefaultUseAutoGroup = origDefaultAuto
	})

	member := &model.User{Username: "meta_member", DisplayName: "Meta", Role: common.RoleCommonUser, Status: common.UserStatusEnabled, Group: "default", AffCode: "aff_meta"}
	require.NoError(t, db.Create(member).Error)
	require.NoError(t, db.Create(&model.UserSubscription{
		UserId:        member.Id,
		PlanId:        10,
		Status:        "active",
		StartTime:     fixedNow.Unix() - 60,
		EndTime:       fixedNow.Unix() + 3600,
		AmountTotal:   5000000,
		AllowedGroups: "ssvip",
	}).Error)

	prioHigh := int64(10)
	require.NoError(t, db.Create(&model.Channel{
		Name:     "S2-OpenAI 组池",
		Type:     59,
		Key:      "sk-secret-pool",
		Status:   common.ChannelStatusEnabled,
		Group:    "ssvip",
		Models:   "gpt-6.2-sol,gpt-5.4",
		Priority: &prioHigh,
	}).Error)
	// Disabled channel in default must not count as enabled supply.
	require.NoError(t, db.Create(&model.Channel{
		Name:   "Disabled Default",
		Type:   1,
		Key:    "sk-disabled",
		Status: common.ChannelStatusManuallyDisabled,
		Group:  "default",
		Models: "gpt-4o",
	}).Error)
	// Unauthorized vip channel must never leak into member's group_meta.
	require.NoError(t, db.Create(&model.Channel{
		Name:   "Hidden VIP Pool",
		Type:   59,
		Key:    "sk-vip",
		Status: common.ChannelStatusEnabled,
		Group:  "vip",
		Models: "claude-opus-4-6",
	}).Error)

	r := gin.New()
	grp := r.Group("/api/subscription/admin/needtoken-console")
	grp.GET("/users/:id/tokens", MemberConsoleListTokens)
	grp.POST("/users/:id/tokens", MemberConsoleCreateToken)
	grp.PUT("/users/:id/tokens/:token_id", MemberConsoleUpdateToken)
	grp.GET("/users/:id/models", MemberConsoleModels)

	doSigned := func(method, target, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, bytes.NewBufferString(body))
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		tsStr := strconv.FormatInt(fixedNow.Unix(), 10)
		sig := computeMemberConsoleSignature([]byte(secret), method, req.URL.Path, req.URL.RawQuery, tsStr, []byte(body))
		req.Header.Set("X-NeedToken-Timestamp", tsStr)
		req.Header.Set("X-NeedToken-Signature", sig)
		rec := httptest.NewRecorder()
		r.ServeHTTP(rec, req)
		return rec
	}

	// 1. List tokens includes group_meta with channel name, ratio, active count, and models; hides vip and secrets.
	listRec := doSigned(http.MethodGet, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens", "")
	require.Equal(t, http.StatusOK, listRec.Code)
	assert.NotContains(t, listRec.Body.String(), "sk-secret-pool")
	assert.NotContains(t, listRec.Body.String(), "Hidden VIP Pool")
	var listResp struct {
		Success bool `json:"success"`
		Data    struct {
			GroupMeta  map[string]memberConsoleGroupMetaDTO `json:"group_meta"`
			GroupRatio map[string]float64                   `json:"group_ratio"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(listRec.Body.Bytes(), &listResp))
	require.Contains(t, listResp.Data.GroupMeta, "ssvip")
	require.Contains(t, listResp.Data.GroupMeta, "default")
	assert.NotContains(t, listResp.Data.GroupMeta, "vip")
	assert.Equal(t, "S2-OpenAI 组池", listResp.Data.GroupMeta["ssvip"].DisplayName)
	assert.Equal(t, 0.5, listResp.Data.GroupMeta["ssvip"].Ratio)
	assert.Equal(t, 1, listResp.Data.GroupMeta["ssvip"].EnabledChannelCount)
	assert.Equal(t, []string{"gpt-5.4", "gpt-6.2-sol"}, listResp.Data.GroupMeta["ssvip"].Models)
	assert.Equal(t, 0, listResp.Data.GroupMeta["default"].EnabledChannelCount)

	// 2. Explicit multi-group token works even when DefaultUseAutoGroup=false.
	multiRec := doSigned(http.MethodPost, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens",
		`{"name":"Multi Group Key","group":"auto","auto_groups":["ssvip","default"],"model_limits_enabled":false}`)
	require.Equal(t, http.StatusOK, multiRec.Code)
	var multiResp struct {
		Success bool `json:"success"`
		Data    struct {
			Token memberConsoleTokenDTO `json:"token"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(multiRec.Body.Bytes(), &multiResp))
	require.True(t, multiResp.Success)
	assert.Equal(t, "auto", multiResp.Data.Token.Group)
	assert.Equal(t, []string{"ssvip", "default"}, multiResp.Data.Token.AutoGroups)
	assert.True(t, multiResp.Data.Token.CrossGroupRetry)

	// 3. Smuggling unauthorized group in auto_groups is strictly rejected.
	smuggleRec := doSigned(http.MethodPost, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens",
		`{"name":"Bad Multi","group":"auto","auto_groups":["ssvip","vip"]}`)
	var smuggleResp struct {
		Success bool `json:"success"`
	}
	require.NoError(t, common.Unmarshal(smuggleRec.Body.Bytes(), &smuggleResp))
	assert.False(t, smuggleResp.Success)

	// 4. Single-item auto_groups normalizes cleanly to a direct group token.
	updRec := doSigned(http.MethodPut, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens/"+strconv.Itoa(multiResp.Data.Token.Id),
		`{"group":"auto","auto_groups":["ssvip"]}`)
	require.Equal(t, http.StatusOK, updRec.Code)
	var updResp struct {
		Success bool                  `json:"success"`
		Data    memberConsoleTokenDTO `json:"data"`
	}
	require.NoError(t, common.Unmarshal(updRec.Body.Bytes(), &updResp))
	require.True(t, updResp.Success)
	assert.Equal(t, "ssvip", updResp.Data.Group)
	assert.Empty(t, updResp.Data.AutoGroups)
	assert.False(t, updResp.Data.CrossGroupRetry)
}
