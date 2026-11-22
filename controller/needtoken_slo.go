package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const (
	needTokenSLORevokeDomain       = "needtoken:slo-revoke:v1"
	needTokenSLONewAPILogoutDomain = "needtoken:slo-newapi-logout:v1"
	needTokenSLOMaxBodyBytes       = 4 * 1024
	needTokenSLOMaxSkewSec         = int64(60)
	needTokenSLOWebhookTimeout     = 5 * time.Second
)

type needTokenSLORevokeRequest struct {
	Source       string `json:"source"`
	NativeUserID int    `json:"native_user_id"`
	Reason       string `json:"reason,omitempty"`
}

type needTokenSLONewAPILogoutPayload struct {
	Source       string `json:"source"`
	NativeUserID int    `json:"native_user_id"`
	Event        string `json:"event"`
}

// Package-level hooks overridden only in unit tests.
var (
	sloRevokeUserSessions = model.RevokeAllUserSessions
	sloHTTPClient         = &http.Client{
		Timeout: needTokenSLOWebhookTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	sloWebhookURLResolver = func() string {
		return strings.TrimSpace(os.Getenv("NEEDTOKEN_SLO_WEBHOOK_URL"))
	}
)

func computeNeedTokenSLOSignature(secret []byte, domain, timestamp string, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(domain))
	mac.Write([]byte{0})
	mac.Write([]byte(timestamp + "."))
	mac.Write(rawBody)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// RevokeNeedTokenSessions handles authenticated back-channel single logout
// requests from NeedToken Glue when a linked storefront member signs out.
func RevokeNeedTokenSessions(c *gin.Context) {
	secret, err := accountLinkSecret()
	if err != nil {
		common.ApiErrorStatus(c, http.StatusServiceUnavailable, common.NewMessage("Single sign-on is not enabled"), gin.H{"code": "sso_disabled"})
		return
	}

	timestamp := strings.TrimSpace(c.GetHeader("X-NeedToken-Timestamp"))
	sigHeader := strings.TrimSpace(c.GetHeader("X-NeedToken-Signature"))
	ts, err := strconv.ParseInt(timestamp, 10, 64)
	if timestamp == "" || sigHeader == "" || err != nil {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Unauthorized"), gin.H{"code": "invalid_signature"})
		return
	}

	nowUnix := accountLinkNow().UTC().Unix()
	if ts < nowUnix-needTokenSLOMaxSkewSec || ts > nowUnix+needTokenSLOMaxSkewSec {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Unauthorized"), gin.H{"code": "signature_expired"})
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, needTokenSLOMaxBodyBytes+1))
	if err != nil || len(rawBody) == 0 || len(rawBody) > needTokenSLOMaxBodyBytes {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}

	expectedSig := computeNeedTokenSLOSignature(secret, needTokenSLORevokeDomain, timestamp, rawBody)
	if !hmac.Equal([]byte(sigHeader), []byte(expectedSig)) {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Unauthorized"), gin.H{"code": "invalid_signature"})
		return
	}

	var req needTokenSLORevokeRequest
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	if req.Source != needTokenSourceStorefront || req.NativeUserID <= 0 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}

	user, err := accountLinkLoadUser(req.NativeUserID)
	if err != nil || user == nil || user.Id != req.NativeUserID {
		common.ApiSuccess(c, gin.H{"revoked_sessions": 0})
		return
	}
	if user.Role >= common.RoleAdminUser {
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Operation not permitted"), gin.H{"code": "admin_protected"})
		return
	}

	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > 64 {
		reason = "needtoken_slo"
	}
	revokedCount, err := sloRevokeUserSessions(user.Id, reason)
	if err != nil {
		logger.LogError(c.Request.Context(), "needtoken slo revoke failed native_user_id="+strconv.Itoa(user.Id))
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Operation failed"))
		return
	}

	logger.LogInfo(c.Request.Context(), "needtoken slo revoked sessions native_user_id="+strconv.Itoa(user.Id)+" count="+strconv.FormatInt(revokedCount, 10))
	common.ApiSuccess(c, gin.H{"revoked_sessions": revokedCount})
}

func isValidSLOWebhookURL(raw string) bool {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Host == "" || parsed.User != nil {
		return false
	}
	return parsed.Scheme == "https" || parsed.Scheme == "http"
}

func notifyNeedTokenLogout(ctx context.Context, userID int) {
	if userID <= 0 {
		return
	}
	webhookURL := sloWebhookURLResolver()
	if webhookURL == "" || !isValidSLOWebhookURL(webhookURL) {
		return
	}
	secret, err := accountLinkSecret()
	if err != nil {
		return
	}
	body, err := common.Marshal(needTokenSLONewAPILogoutPayload{
		Source:       "new-api",
		NativeUserID: userID,
		Event:        "logout",
	})
	if err != nil {
		return
	}
	ts := strconv.FormatInt(accountLinkNow().UTC().Unix(), 10)
	sig := computeNeedTokenSLOSignature(secret, needTokenSLONewAPILogoutDomain, ts, body)

	reqCtx, cancel := context.WithTimeout(ctx, needTokenSLOWebhookTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, webhookURL, bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-NeedToken-Timestamp", ts)
	req.Header.Set("X-NeedToken-Signature", sig)

	resp, err := sloHTTPClient.Do(req)
	if err != nil {
		logger.LogWarn(reqCtx, "needtoken slo outbound notify failed native_user_id="+strconv.Itoa(userID))
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode >= 400 {
		logger.LogWarn(reqCtx, "needtoken slo outbound notify rejected native_user_id="+strconv.Itoa(userID)+" status="+strconv.Itoa(resp.StatusCode))
	}
}

func notifyNeedTokenLogoutAsync(userID int) {
	if userID <= 0 || sloWebhookURLResolver() == "" {
		return
	}
	go notifyNeedTokenLogout(context.Background(), userID)
}
