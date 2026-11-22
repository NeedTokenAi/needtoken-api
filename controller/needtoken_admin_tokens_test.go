package controller

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service/authz"

	"github.com/alicebob/miniredis/v2"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/go-redis/redis/v8"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupNeedTokenAdminTokensTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())

	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis := common.RedisEnabled
	previousSecret := common.SessionSecret
	previousMaster := common.IsMasterNode

	dsn := fmt.Sprintf("file:%s_%d?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"), time.Now().UnixNano())
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(
		&model.User{},
		&model.UserSession{},
		&model.UserAccessToken{},
		&model.Token{},
		&model.TwoFA{},
		&model.TwoFABackupCode{},
		&model.Option{},
		&model.CasbinRule{},
		&model.AuthzRole{},
		&model.Log{},
		&model.AuditLog{},
	))

	model.DB = db
	model.LOG_DB = db
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	common.SessionSecret = "needtoken-admin-tokens-test-secret"
	common.IsMasterNode = true
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitLogDB())
	require.NoError(t, authz.Init(db))

	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled = previousRedis
		common.SessionSecret = previousSecret
		common.IsMasterNode = previousMaster
		if sqlDB, err := db.DB(); err == nil {
			_ = sqlDB.Close()
		}
	})

	return db
}

func newNeedTokenAdminTokensTestRouter() *gin.Engine {
	router := gin.New()
	router.Use(middleware.RequestId(), middleware.AccessTokenAudit())
	api := router.Group("/api")
	adminRoute := api.Group("/user")
	adminRoute.Use(middleware.AdminAuth())
	{
		adminRoute.GET("/", GetAllUsers)
		adminRoute.GET("/search", SearchUsers)
		adminRoute.GET("/:id", GetUser)
		adminRoute.GET("/:id/tokens", middleware.DisableCache(), AdminGetUserTokens)
		adminRoute.POST("/:id/tokens", middleware.DisableCache(), AdminCreateUserToken)
		adminRoute.PUT("/:id/tokens/:token_id", middleware.DisableCache(), AdminUpdateUserToken)
		adminRoute.DELETE("/:id/tokens/:token_id", middleware.DisableCache(), AdminDeleteUserToken)
	}
	return router
}

func createAdminTokenTestUser(t *testing.T, db *gorm.DB, username string, role int, group string) *model.User {
	t.Helper()
	if group == "" {
		group = "default"
	}
	user := &model.User{
		Username:    username,
		DisplayName: username,
		Password:    "placeholder-hash",
		Role:        role,
		Status:      common.UserStatusEnabled,
		Group:       group,
		AuthVersion: 1,
		AffCode:     username + "_aff",
	}
	require.NoError(t, db.Create(user).Error)
	return user
}

func TestAdminUserTokensCRUDMaskingAndAudit(t *testing.T) {
	db := setupNeedTokenAdminTokensTestDB(t)
	router := newNeedTokenAdminTokensTestRouter()

	admin := createAdminTokenTestUser(t, db, "nt_admin", common.RoleAdminUser, "default")
	member := createAdminTokenTestUser(t, db, "nt_member", common.RoleCommonUser, "default")
	_, adminJWT := createAccessTokenTestSession(t, admin.Id, "admin-crud-session")

	configureTokenAutoGroupsTest(t, "5", `["default","vip"]`)

	// 1. POST creates token and returns full sk- key once plus masked DTO.
	createBody := `{
		"name": "ops-created-key",
		"group": "auto",
		"remain_quota": 500000,
		"unlimited_quota": false,
		"expired_time": -1,
		"model_limits_enabled": true,
		"model_limits": "gpt-4o,claude-3-5-sonnet",
		"allow_ips": "10.0.0.1\n192.168.1.0/24",
		"cross_group_retry": true,
		"auto_groups": ["default", "vip"]
	}`
	createResp := accessTokenRequest(router, http.MethodPost, fmt.Sprintf("/api/user/%d/tokens", member.Id), adminJWT, "", createBody)
	require.Equal(t, http.StatusOK, createResp.Code, createResp.Body.String())
	assert.Contains(t, createResp.Header().Get("Cache-Control"), "no-store")

	var createPayload struct {
		Success bool   `json:"success"`
		Message string `json:"message"`
		Data    struct {
			Key   string          `json:"key"`
			Token json.RawMessage `json:"token"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(createResp.Body.Bytes(), &createPayload))
	require.True(t, createPayload.Success, createPayload.Message)
	require.True(t, strings.HasPrefix(createPayload.Data.Key, "sk-"))
	rawKey := strings.TrimPrefix(createPayload.Data.Key, "sk-")
	require.Len(t, rawKey, 48)

	var createdTokenMap map[string]any
	require.NoError(t, common.Unmarshal(createPayload.Data.Token, &createdTokenMap))
	_, hasRawKeyField := createdTokenMap["key"]
	assert.False(t, hasRawKeyField, "token metadata DTO must never include raw key field")
	assert.Equal(t, maskAdminTokenKey(rawKey), createdTokenMap["masked_key"])
	assert.NotContains(t, string(createPayload.Data.Token), rawKey)

	var createdDTO needTokenAdminTokenDTO
	require.NoError(t, common.Unmarshal(createPayload.Data.Token, &createdDTO))
	assert.Equal(t, member.Id, createdDTO.UserId)
	assert.Equal(t, "ops-created-key", createdDTO.Name)
	assert.Equal(t, common.TokenStatusEnabled, createdDTO.Status)
	assert.Equal(t, "auto", createdDTO.Group)
	assert.Equal(t, 500000, createdDTO.RemainQuota)
	assert.False(t, createdDTO.UnlimitedQuota)
	assert.Equal(t, int64(-1), createdDTO.ExpiredTime)
	assert.True(t, createdDTO.ModelLimitsEnabled)
	assert.Equal(t, "gpt-4o,claude-3-5-sonnet", createdDTO.ModelLimits)
	require.NotNil(t, createdDTO.AllowIps)
	assert.Equal(t, "10.0.0.1\n192.168.1.0/24", *createdDTO.AllowIps)
	assert.True(t, createdDTO.CrossGroupRetry)
	assert.Equal(t, []string{"default", "vip"}, createdDTO.AutoGroups)

	// Verify DB stored rawKey without sk- prefix and token validates normally.
	var storedToken model.Token
	require.NoError(t, db.First(&storedToken, createdDTO.Id).Error)
	assert.Equal(t, rawKey, storedToken.Key)

	// Seed two more tokens for pagination check.
	extraKey1 := "aaaa1111bbbb2222cccc3333dddd4444eeee5555ffff6666"
	extraKey2 := "1111aaaa2222bbbb3333cccc4444dddd5555eeee6666ffff"
	seedToken(t, db, member.Id, "second-token", extraKey1)
	seedToken(t, db, member.Id, "third-token", extraKey2)

	// 2. GET paginates and never exposes raw keys.
	listResp := accessTokenRequest(router, http.MethodGet, fmt.Sprintf("/api/user/%d/tokens?p=1&page_size=2", member.Id), adminJWT, "", "")
	require.Equal(t, http.StatusOK, listResp.Code, listResp.Body.String())
	assert.NotContains(t, listResp.Body.String(), rawKey)
	assert.NotContains(t, listResp.Body.String(), extraKey1)
	assert.NotContains(t, listResp.Body.String(), extraKey2)

	var listPayload struct {
		Success bool `json:"success"`
		Data    struct {
			Total int               `json:"total"`
			Items []json.RawMessage `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(listResp.Body.Bytes(), &listPayload))
	require.True(t, listPayload.Success)
	assert.Equal(t, 3, listPayload.Data.Total)
	require.Len(t, listPayload.Data.Items, 2)
	for _, rawItem := range listPayload.Data.Items {
		var m map[string]any
		require.NoError(t, common.Unmarshal(rawItem, &m))
		_, hasKey := m["key"]
		assert.False(t, hasKey, "list item must not contain raw key field")
		assert.True(t, strings.HasPrefix(fmt.Sprint(m["masked_key"]), "sk-"))
	}

	// 3. PUT edits metadata + status (1/2) and never exposes raw key.
	updateBody := `{
		"name": "ops-renamed-key",
		"status": 2,
		"group": "default",
		"remain_quota": 1200000,
		"unlimited_quota": true,
		"expired_time": 4102444800,
		"model_limits_enabled": false,
		"model_limits": "",
		"allow_ips": "",
		"cross_group_retry": true
	}`
	updateResp := accessTokenRequest(router, http.MethodPut, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, createdDTO.Id), adminJWT, "", updateBody)
	require.Equal(t, http.StatusOK, updateResp.Code, updateResp.Body.String())
	assert.NotContains(t, updateResp.Body.String(), rawKey)

	var updatePayload struct {
		Success bool            `json:"success"`
		Data    json.RawMessage `json:"data"`
	}
	require.NoError(t, common.Unmarshal(updateResp.Body.Bytes(), &updatePayload))
	require.True(t, updatePayload.Success)
	var updatedMap map[string]any
	require.NoError(t, common.Unmarshal(updatePayload.Data, &updatedMap))
	_, hasKey := updatedMap["key"]
	assert.False(t, hasKey)

	var updatedDTO needTokenAdminTokenDTO
	require.NoError(t, common.Unmarshal(updatePayload.Data, &updatedDTO))
	assert.Equal(t, "ops-renamed-key", updatedDTO.Name)
	assert.Equal(t, common.TokenStatusDisabled, updatedDTO.Status)
	assert.Equal(t, "default", updatedDTO.Group)
	assert.True(t, updatedDTO.UnlimitedQuota)
	assert.Equal(t, int64(4102444800), updatedDTO.ExpiredTime)
	assert.False(t, updatedDTO.CrossGroupRetry, "non-auto group must clear cross_group_retry")
	assert.Empty(t, updatedDTO.AutoGroups, "non-auto group must clear auto_groups")
	assert.Equal(t, maskAdminTokenKey(rawKey), updatedDTO.MaskedKey)

	// Invalid status (e.g. 3) must be rejected.
	badStatusResp := accessTokenRequest(router, http.MethodPut, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, createdDTO.Id), adminJWT, "", `{"status":3}`)
	require.Equal(t, http.StatusOK, badStatusResp.Code)
	assert.Contains(t, badStatusResp.Body.String(), `"success":false`)

	// 4. DELETE removes the token.
	deleteResp := accessTokenRequest(router, http.MethodDelete, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, createdDTO.Id), adminJWT, "", "")
	require.Equal(t, http.StatusOK, deleteResp.Code, deleteResp.Body.String())
	assert.Contains(t, deleteResp.Body.String(), `"success":true`)
	assert.NotContains(t, deleteResp.Body.String(), rawKey)

	var remaining int64
	require.NoError(t, db.Model(&model.Token{}).Where("id = ?", createdDTO.Id).Count(&remaining).Error)
	assert.Zero(t, remaining)

	// 5. Audit logs must record create/update/delete and never contain raw key.
	var audits []model.AuditLog
	require.NoError(t, db.Where("action IN ?", []string{"admin.user_token_create", "admin.user_token_update", "admin.user_token_delete"}).Find(&audits).Error)
	require.Len(t, audits, 3)
	for _, audit := range audits {
		assert.Equal(t, admin.Id, audit.UserId)
		assert.NotContains(t, audit.Content, rawKey)
		otherBytes, err := common.Marshal(audit.Other)
		require.NoError(t, err)
		assert.NotContains(t, string(otherBytes), rawKey)
		assert.Contains(t, string(otherBytes), maskAdminTokenKey(rawKey))
	}
}

func TestAdminUserTokensAuthorizationAndIDOR(t *testing.T) {
	db := setupNeedTokenAdminTokensTestDB(t)
	router := newNeedTokenAdminTokensTestRouter()

	root := createAdminTokenTestUser(t, db, "nt_root", common.RoleRootUser, "default")
	adminA := createAdminTokenTestUser(t, db, "nt_admin_a", common.RoleAdminUser, "default")
	adminB := createAdminTokenTestUser(t, db, "nt_admin_b", common.RoleAdminUser, "default")
	memberA := createAdminTokenTestUser(t, db, "nt_member_a", common.RoleCommonUser, "default")
	memberB := createAdminTokenTestUser(t, db, "nt_member_b", common.RoleCommonUser, "default")

	_, rootJWT := createAccessTokenTestSession(t, root.Id, "root-authz-session")
	_, adminAJWT := createAccessTokenTestSession(t, adminA.Id, "admin-a-authz-session")
	_, memberAJWT := createAccessTokenTestSession(t, memberA.Id, "member-a-authz-session")

	rootToken := seedToken(t, db, root.Id, "root-secret-tok", "root9999secret8888token7777value6666")
	adminBToken := seedToken(t, db, adminB.Id, "adminb-secret-tok", "adminb9999secret8888token7777value6666")
	memberBToken := seedToken(t, db, memberB.Id, "memberb-tok", "memberb9999secret8888token7777value6666")

	// Common user is rejected by AdminAuth (403).
	resp := accessTokenRequest(router, http.MethodGet, fmt.Sprintf("/api/user/%d/tokens", memberB.Id), memberAJWT, "", "")
	assert.Equal(t, http.StatusForbidden, resp.Code)

	// Admin cannot view/create/update/delete Root or peer Admin tokens.
	for _, tc := range []struct {
		name, method, path, body, forbiddenSecret string
	}{
		{"admin_get_root", http.MethodGet, fmt.Sprintf("/api/user/%d/tokens", root.Id), "", rootToken.Key},
		{"admin_create_root", http.MethodPost, fmt.Sprintf("/api/user/%d/tokens", root.Id), `{"name":"x"}`, rootToken.Key},
		{"admin_update_root", http.MethodPut, fmt.Sprintf("/api/user/%d/tokens/%d", root.Id, rootToken.Id), `{"name":"x"}`, rootToken.Key},
		{"admin_delete_root", http.MethodDelete, fmt.Sprintf("/api/user/%d/tokens/%d", root.Id, rootToken.Id), "", rootToken.Key},
		{"admin_get_peer_admin", http.MethodGet, fmt.Sprintf("/api/user/%d/tokens", adminB.Id), "", adminBToken.Key},
		{"admin_delete_peer_admin", http.MethodDelete, fmt.Sprintf("/api/user/%d/tokens/%d", adminB.Id, adminBToken.Id), "", adminBToken.Key},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := accessTokenRequest(router, tc.method, tc.path, adminAJWT, "", tc.body)
			assert.Contains(t, rec.Body.String(), `"success":false`)
			assert.NotContains(t, rec.Body.String(), tc.forbiddenSecret)
		})
	}

	// Cross-user IDOR: user A's path ID with user B's token ID must fail without mutating user B's token.
	idorUpdate := accessTokenRequest(router, http.MethodPut, fmt.Sprintf("/api/user/%d/tokens/%d", memberA.Id, memberBToken.Id), adminAJWT, "", `{"name":"hijacked"}`)
	assert.Contains(t, idorUpdate.Body.String(), `"success":false`)
	assert.Contains(t, idorUpdate.Body.String(), "Token does not exist")

	idorDelete := accessTokenRequest(router, http.MethodDelete, fmt.Sprintf("/api/user/%d/tokens/%d", memberA.Id, memberBToken.Id), adminAJWT, "", "")
	assert.Contains(t, idorDelete.Body.String(), `"success":false`)

	var untouched model.Token
	require.NoError(t, db.First(&untouched, memberBToken.Id).Error)
	assert.Equal(t, "memberb-tok", untouched.Name)

	// Root can list and manage Admin and Root tokens.
	rootListAdmin := accessTokenRequest(router, http.MethodGet, fmt.Sprintf("/api/user/%d/tokens", adminB.Id), rootJWT, "", "")
	require.Equal(t, http.StatusOK, rootListAdmin.Code)
	assert.Contains(t, rootListAdmin.Body.String(), `"success":true`)
	assert.Contains(t, rootListAdmin.Body.String(), maskAdminTokenKey(adminBToken.Key))
	assert.NotContains(t, rootListAdmin.Body.String(), adminBToken.Key)

	rootListSelf := accessTokenRequest(router, http.MethodGet, fmt.Sprintf("/api/user/%d/tokens", root.Id), rootJWT, "", "")
	require.Equal(t, http.StatusOK, rootListSelf.Code)
	assert.Contains(t, rootListSelf.Body.String(), `"success":true`)
	assert.NotContains(t, rootListSelf.Body.String(), rootToken.Key)
}

func TestAdminUserTokensScopedPATPermissions(t *testing.T) {
	db := setupNeedTokenAdminTokensTestDB(t)
	router := newNeedTokenAdminTokensTestRouter()

	admin := createAdminTokenTestUser(t, db, "pat_admin", common.RoleAdminUser, "default")
	member := createAdminTokenTestUser(t, db, "pat_member", common.RoleCommonUser, "default")
	existing := seedToken(t, db, member.Id, "existing-tok", "pat1111existing2222token3333value4444")

	readPAT, _ := createScopedAccessToken(t, admin.Id, 0, "user:read")
	writePAT, _ := createScopedAccessToken(t, admin.Id, 0, "user:write")
	unrelatedPAT, _ := createScopedAccessToken(t, admin.Id, 0, "profile:read")

	// user:read allows GET, rejects POST/PUT/DELETE.
	rec := accessTokenRequest(router, http.MethodGet, fmt.Sprintf("/api/user/%d/tokens", member.Id), readPAT, "", "")
	assert.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"success":true`)

	rec = accessTokenRequest(router, http.MethodPost, fmt.Sprintf("/api/user/%d/tokens", member.Id), readPAT, "", `{"name":"x"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_TOKEN_SCOPE_DENIED")

	rec = accessTokenRequest(router, http.MethodPut, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, existing.Id), readPAT, "", `{"name":"x"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)

	rec = accessTokenRequest(router, http.MethodDelete, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, existing.Id), readPAT, "", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)

	// user:write rejects GET, allows POST/PUT/DELETE.
	rec = accessTokenRequest(router, http.MethodGet, fmt.Sprintf("/api/user/%d/tokens", member.Id), writePAT, "", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	assert.Contains(t, rec.Body.String(), "ACCESS_TOKEN_SCOPE_DENIED")

	rec = accessTokenRequest(router, http.MethodPost, fmt.Sprintf("/api/user/%d/tokens", member.Id), writePAT, "", `{"name":"pat-created"}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"success":true`)

	rec = accessTokenRequest(router, http.MethodPut, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, existing.Id), writePAT, "", `{"name":"pat-updated","status":2}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"success":true`)

	rec = accessTokenRequest(router, http.MethodDelete, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, existing.Id), writePAT, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"success":true`)

	// Unrelated scope is rejected on GET and POST.
	rec = accessTokenRequest(router, http.MethodGet, fmt.Sprintf("/api/user/%d/tokens", member.Id), unrelatedPAT, "", "")
	assert.Equal(t, http.StatusForbidden, rec.Code)
	rec = accessTokenRequest(router, http.MethodPost, fmt.Sprintf("/api/user/%d/tokens", member.Id), unrelatedPAT, "", `{"name":"nope"}`)
	assert.Equal(t, http.StatusForbidden, rec.Code)
}

func TestAdminUserTokensInvalidatesRedisCacheOnUpdateAndDelete(t *testing.T) {
	db := setupNeedTokenAdminTokensTestDB(t)
	router := newNeedTokenAdminTokensTestRouter()

	mr := miniredis.RunT(t)
	oldRDB := common.RDB
	common.RDB = redis.NewClient(&redis.Options{Addr: mr.Addr(), MaxRetries: -1})
	common.RedisEnabled = true
	t.Cleanup(func() {
		_ = common.RDB.Close()
		common.RDB = oldRDB
	})

	admin := createAdminTokenTestUser(t, db, "cache_admin", common.RoleAdminUser, "default")
	member := createAdminTokenTestUser(t, db, "cache_member", common.RoleCommonUser, "default")
	_, adminJWT := createAccessTokenTestSession(t, admin.Id, "admin-cache-session")

	tokUpdate := seedToken(t, db, member.Id, "cached-for-update", "cacheupdate111122223333444455556666")
	tokDelete := seedToken(t, db, member.Id, "cached-for-delete", "cachedelete111122223333444455556666")

	// Hydrate Redis cache for both tokens.
	_, err := model.GetTokenByKey(tokUpdate.Key, false)
	require.NoError(t, err)
	_, err = model.GetTokenByKey(tokDelete.Key, false)
	require.NoError(t, err)

	updateCacheKey := fmt.Sprintf("token:%s", common.GenerateHMAC(tokUpdate.Key))
	updateFenceKey := fmt.Sprintf("token:fence:%s", common.GenerateHMAC(tokUpdate.Key))
	deleteCacheKey := fmt.Sprintf("token:%s", common.GenerateHMAC(tokDelete.Key))
	deleteFenceKey := fmt.Sprintf("token:fence:%s", common.GenerateHMAC(tokDelete.Key))

	require.True(t, mr.Exists(updateCacheKey))
	require.True(t, mr.Exists(deleteCacheKey))

	// PUT invalidates Redis token hash and sets fence key.
	rec := accessTokenRequest(router, http.MethodPut, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, tokUpdate.Id), adminJWT, "", `{"status":2}`)
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"success":true`)
	assert.False(t, mr.Exists(updateCacheKey), "update must delete stale Redis token cache")
	assert.True(t, mr.Exists(updateFenceKey), "update must set mutation fence key")

	reloaded, err := model.GetTokenByKey(tokUpdate.Key, false)
	require.NoError(t, err)
	assert.Equal(t, common.TokenStatusDisabled, reloaded.Status)

	// DELETE invalidates Redis token hash and sets fence key.
	rec = accessTokenRequest(router, http.MethodDelete, fmt.Sprintf("/api/user/%d/tokens/%d", member.Id, tokDelete.Id), adminJWT, "", "")
	require.Equal(t, http.StatusOK, rec.Code)
	assert.Contains(t, rec.Body.String(), `"success":true`)
	assert.False(t, mr.Exists(deleteCacheKey), "delete must delete stale Redis token cache")
	assert.True(t, mr.Exists(deleteFenceKey), "delete must set mutation fence key")
}

func TestUserEndpointsPopulateTwoFAEnabledInBatchWithoutSecretLeak(t *testing.T) {
	db := setupNeedTokenAdminTokensTestDB(t)
	router := newNeedTokenAdminTokensTestRouter()

	root := createAdminTokenTestUser(t, db, "twofa_root", common.RoleRootUser, "default")
	userEnabled := createAdminTokenTestUser(t, db, "twofa_enabled", common.RoleCommonUser, "default")
	userDisabled := createAdminTokenTestUser(t, db, "twofa_disabled", common.RoleCommonUser, "default")
	userNone := createAdminTokenTestUser(t, db, "twofa_none", common.RoleCommonUser, "default")
	userDeletedTwoFA := createAdminTokenTestUser(t, db, "twofa_deleted", common.RoleCommonUser, "default")

	const secretEnabled = "SUPERSECRETENABLED2FATOTP"
	const secretDisabled = "SUPERSECRETDISABLED2FATOTP"
	const secretDeleted = "SUPERSECRETDELETED2FATOTP"

	require.NoError(t, db.Create(&model.TwoFA{UserId: userEnabled.Id, Secret: secretEnabled, IsEnabled: true}).Error)
	require.NoError(t, db.Create(&model.TwoFA{UserId: userDisabled.Id, Secret: secretDisabled, IsEnabled: false}).Error)
	deletedRow := &model.TwoFA{UserId: userDeletedTwoFA.Id, Secret: secretDeleted, IsEnabled: true}
	require.NoError(t, db.Create(deletedRow).Error)
	require.NoError(t, db.Delete(deletedRow).Error)

	_, rootJWT := createAccessTokenTestSession(t, root.Id, "root-twofa-session")

	var twofaQueries int
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register("test:count_twofa_queries", func(tx *gorm.DB) {
		if tx.Statement != nil && tx.Statement.Table == "two_fas" {
			twofaQueries++
		}
	}))
	t.Cleanup(func() {
		_ = db.Callback().Query().Remove("test:count_twofa_queries")
	})

	// 1. GetAllUsers: single batch IN query, accurate twofa_enabled, zero secret leakage.
	twofaQueries = 0
	listResp := accessTokenRequest(router, http.MethodGet, "/api/user/?p=1&page_size=20", rootJWT, "", "")
	require.Equal(t, http.StatusOK, listResp.Code, listResp.Body.String())
	assert.Equal(t, 1, twofaQueries, "GetAllUsers must batch-load two_fas in a single IN query without N+1")
	assert.NotContains(t, listResp.Body.String(), secretEnabled)
	assert.NotContains(t, listResp.Body.String(), secretDisabled)
	assert.NotContains(t, listResp.Body.String(), secretDeleted)

	var pageResp struct {
		Success bool `json:"success"`
		Data    struct {
			Items []struct {
				Id           int  `json:"id"`
				TwoFAEnabled bool `json:"twofa_enabled"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(listResp.Body.Bytes(), &pageResp))
	require.True(t, pageResp.Success)
	gotByID := make(map[int]bool, len(pageResp.Data.Items))
	for _, item := range pageResp.Data.Items {
		gotByID[item.Id] = item.TwoFAEnabled
	}
	assert.True(t, gotByID[userEnabled.Id])
	assert.False(t, gotByID[userDisabled.Id])
	assert.False(t, gotByID[userNone.Id])
	assert.False(t, gotByID[userDeletedTwoFA.Id])

	// 2. SearchUsers: single batch IN query, accurate twofa_enabled, zero secret leakage.
	twofaQueries = 0
	searchResp := accessTokenRequest(router, http.MethodGet, "/api/user/search?keyword=twofa_&p=1&page_size=20", rootJWT, "", "")
	require.Equal(t, http.StatusOK, searchResp.Code, searchResp.Body.String())
	assert.Equal(t, 1, twofaQueries, "SearchUsers must batch-load two_fas in a single IN query without N+1")
	assert.NotContains(t, searchResp.Body.String(), secretEnabled)
	assert.NotContains(t, searchResp.Body.String(), secretDisabled)

	var searchPageResp struct {
		Success bool `json:"success"`
		Data    struct {
			Items []struct {
				Id           int  `json:"id"`
				TwoFAEnabled bool `json:"twofa_enabled"`
			} `json:"items"`
		} `json:"data"`
	}
	require.NoError(t, common.Unmarshal(searchResp.Body.Bytes(), &searchPageResp))
	require.True(t, searchPageResp.Success)
	searchByID := make(map[int]bool, len(searchPageResp.Data.Items))
	for _, item := range searchPageResp.Data.Items {
		searchByID[item.Id] = item.TwoFAEnabled
	}
	assert.True(t, searchByID[userEnabled.Id])
	assert.False(t, searchByID[userDisabled.Id])
	assert.False(t, searchByID[userNone.Id])
	assert.False(t, searchByID[userDeletedTwoFA.Id])

	// 3. GetUser: populates twofa_enabled for single user without leaking secret.
	for _, tc := range []struct {
		userID int
		want   bool
	}{
		{userEnabled.Id, true},
		{userDisabled.Id, false},
		{userNone.Id, false},
		{userDeletedTwoFA.Id, false},
	} {
		rec := accessTokenRequest(router, http.MethodGet, "/api/user/"+strconv.Itoa(tc.userID), rootJWT, "", "")
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		assert.NotContains(t, rec.Body.String(), secretEnabled)
		assert.NotContains(t, rec.Body.String(), secretDisabled)
		var singleResp struct {
			Success bool `json:"success"`
			Data    struct {
				Id           int  `json:"id"`
				TwoFAEnabled bool `json:"twofa_enabled"`
			} `json:"data"`
		}
		require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &singleResp))
		require.True(t, singleResp.Success)
		assert.Equal(t, tc.want, singleResp.Data.TwoFAEnabled, "user_id=%d", tc.userID)
	}
}
