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
	require.NoError(t, db.AutoMigrate(&model.User{}, &model.Token{}, &model.Log{}, &model.QuotaData{}, &model.UserSubscription{}, &model.SubscriptionPlan{}))
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

	// 7. Delete token.
	delRec := doSigned(http.MethodDelete, "/api/subscription/admin/needtoken-console/users/"+strconv.Itoa(member.Id)+"/tokens/"+strconv.Itoa(tokenID), "", fixedNow.Unix(), "")
	require.Equal(t, http.StatusOK, delRec.Code)
}
