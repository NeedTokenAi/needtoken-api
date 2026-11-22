package controller

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRevokeNeedTokenSessions(t *testing.T) {
	gin.SetMode(gin.TestMode)
	secret := "0123456789abcdef0123456789abcdef"
	fixedNow := time.Unix(1_795_360_000, 0).UTC()

	prevNow := accountLinkNow
	prevLoad := accountLinkLoadUser
	prevRevoke := sloRevokeUserSessions
	t.Cleanup(func() {
		accountLinkNow = prevNow
		accountLinkLoadUser = prevLoad
		sloRevokeUserSessions = prevRevoke
	})
	accountLinkNow = func() time.Time { return fixedNow }

	var revokedUID int
	var revokedReason string
	sloRevokeUserSessions = func(userID int, reason string) (int64, error) {
		revokedUID = userID
		revokedReason = reason
		return 2, nil
	}
	accountLinkLoadUser = func(id int) (*model.User, error) {
		switch id {
		case 25:
			return &model.User{Id: 25, Username: "nt_25", Role: common.RoleCommonUser, Status: common.UserStatusEnabled}, nil
		case 1:
			return &model.User{Id: 1, Username: "root", Role: common.RoleRootUser, Status: common.UserStatusEnabled}, nil
		default:
			return nil, assert.AnError
		}
	}

	makeSignedReq := func(ts string, body string, sigOverride string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPost, "/api/subscription/admin/needtoken-slo-revoke", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		if ts != "" {
			c.Request.Header.Set("X-NeedToken-Timestamp", ts)
		}
		sig := sigOverride
		if sig == "" && ts != "" {
			sig = computeNeedTokenSLOSignature([]byte(secret), needTokenSLORevokeDomain, ts, []byte(body))
		}
		if sig != "" {
			c.Request.Header.Set("X-NeedToken-Signature", sig)
		}
		RevokeNeedTokenSessions(c)
		return w
	}

	t.Run("disabled without secret", func(t *testing.T) {
		t.Setenv("NEEDTOKEN_LINKING_SECRET", "")
		w := makeSignedReq(strconv.FormatInt(fixedNow.Unix(), 10), `{"source":"needtoken-storefront","native_user_id":25}`, "")
		assert.Equal(t, http.StatusServiceUnavailable, w.Code)
	})

	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	t.Run("invalid signature rejected", func(t *testing.T) {
		w := makeSignedReq(strconv.FormatInt(fixedNow.Unix(), 10), `{"source":"needtoken-storefront","native_user_id":25}`, "sha256=deadbeef")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("expired timestamp rejected", func(t *testing.T) {
		expired := strconv.FormatInt(fixedNow.Unix()-120, 10)
		w := makeSignedReq(expired, `{"source":"needtoken-storefront","native_user_id":25}`, "")
		assert.Equal(t, http.StatusUnauthorized, w.Code)
	})

	t.Run("admin account protected from storefront slo", func(t *testing.T) {
		revokedUID = 0
		w := makeSignedReq(strconv.FormatInt(fixedNow.Unix(), 10), `{"source":"needtoken-storefront","native_user_id":1}`, "")
		assert.Equal(t, http.StatusForbidden, w.Code)
		assert.Equal(t, 0, revokedUID)
	})

	t.Run("unknown user returns zero without enumeration", func(t *testing.T) {
		revokedUID = 0
		w := makeSignedReq(strconv.FormatInt(fixedNow.Unix(), 10), `{"source":"needtoken-storefront","native_user_id":9999}`, "")
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"revoked_sessions":0`)
		assert.Equal(t, 0, revokedUID)
	})

	t.Run("valid common member revoked", func(t *testing.T) {
		revokedUID = 0
		revokedReason = ""
		w := makeSignedReq(strconv.FormatInt(fixedNow.Unix(), 10), `{"source":"needtoken-storefront","native_user_id":25,"reason":"storefront_logout"}`, "")
		assert.Equal(t, http.StatusOK, w.Code)
		assert.Contains(t, w.Body.String(), `"revoked_sessions":2`)
		assert.Equal(t, 25, revokedUID)
		assert.Equal(t, "storefront_logout", revokedReason)
	})
}

func TestNotifyNeedTokenLogoutSignature(t *testing.T) {
	secret := "0123456789abcdef0123456789abcdef"
	t.Setenv("NEEDTOKEN_LINKING_SECRET", secret)

	fixedNow := time.Unix(1_795_360_100, 0).UTC()
	prevNow := accountLinkNow
	prevClient := sloHTTPClient
	prevURL := sloWebhookURLResolver
	t.Cleanup(func() {
		accountLinkNow = prevNow
		sloHTTPClient = prevClient
		sloWebhookURLResolver = prevURL
	})
	accountLinkNow = func() time.Time { return fixedNow }

	var gotBody []byte
	var gotTS, gotSig string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotTS = r.Header.Get("X-NeedToken-Timestamp")
		gotSig = r.Header.Get("X-NeedToken-Signature")
		gotBody, _ = io.ReadAll(r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	sloHTTPClient = srv.Client()
	sloWebhookURLResolver = func() string { return srv.URL + "/api/internal/slo/newapi-logout" }

	notifyNeedTokenLogout(context.Background(), 42)
	require.NotEmpty(t, gotBody)
	assert.Equal(t, strconv.FormatInt(fixedNow.Unix(), 10), gotTS)
	expectedSig := computeNeedTokenSLOSignature([]byte(secret), needTokenSLONewAPILogoutDomain, gotTS, gotBody)
	assert.Equal(t, expectedSig, gotSig)

	var payload needTokenSLONewAPILogoutPayload
	require.NoError(t, common.Unmarshal(bytes.TrimSpace(gotBody), &payload))
	assert.Equal(t, "new-api", payload.Source)
	assert.Equal(t, 42, payload.NativeUserID)
	assert.Equal(t, "logout", payload.Event)
}
