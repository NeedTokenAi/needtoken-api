package controller

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"net/http"
	"net/mail"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

const (
	needTokenSourceStorefront       = "needtoken-storefront"
	needTokenMerchantIDPrefix       = "needtoken-storefront:user:"
	needTokenProvisionDomain        = "needtoken:provision-member:v1"
	needTokenConsoleSSOTicketDomain = "needtoken:console-sso:ticket:v1"
	needTokenProvisionMaxBodyBytes  = 16 * 1024
	needTokenProvisionMaxSkewSec    = int64(60)
	needTokenSSOMaxTicketTTLSec     = int64(120)
	needTokenSSOMaxSkewSec          = int64(30)
	needTokenSSOJTIMaxLen           = 128
	needTokenSSOMemoryCap           = 10000
	needTokenSSORedisKeyPrefix      = "needtoken:sso:jti:"
	lowerAlnumAlphabet              = "abcdefghijklmnopqrstuvwxyz0123456789"
)

var (
	errSSOTicketInvalid      = errors.New("invalid or expired sso ticket")
	errSSOTicketReplayed     = errors.New("sso ticket already consumed")
	errSSOReplayStoreFailure = errors.New("sso replay store unavailable")

	ssoReplayMu     sync.Mutex
	ssoConsumedJTIs = make(map[string]int64)
)

type needTokenProvisionMemberRequest struct {
	Source             string `json:"source"`
	MerchantCustomerID string `json:"merchant_customer_id"`
	Email              string `json:"email"`
	EmailVerified      bool   `json:"email_verified"`
	AllowWithoutEmail  bool   `json:"allow_without_email,omitempty"`
	DisplayName        string `json:"display_name"`
}

type consoleSSOTicketClaims struct {
	JTI        string `json:"jti"`
	Source     string `json:"src"`
	MerchantID string `json:"mid"`
	NativeUID  int    `json:"uid"`
	TargetPath string `json:"target,omitempty"`
	IssuedAt   int64  `json:"iat"`
	ExpiresAt  int64  `json:"exp"`
}

func computeProvisionMemberSignature(secret []byte, timestamp string, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(needTokenProvisionDomain))
	mac.Write([]byte{0})
	mac.Write([]byte(timestamp + "."))
	mac.Write(rawBody)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func isValidProvisionEmail(email string) bool {
	if email == "" || len(email) > 50 {
		return false
	}
	for _, r := range email {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	addr, err := mail.ParseAddress(email)
	if err != nil || !strings.EqualFold(addr.Address, email) {
		return false
	}
	local, domain, ok := strings.Cut(email, "@")
	if !ok || local == "" || domain == "" || strings.Contains(domain, "@") {
		return false
	}
	return strings.Contains(domain, ".") && !strings.HasPrefix(domain, ".") && !strings.HasSuffix(domain, ".")
}

func extractMerchantCustomerIDToken(merchantCustomerID string) (string, bool) {
	raw, ok := strings.CutPrefix(strings.TrimSpace(merchantCustomerID), needTokenMerchantIDPrefix)
	if !ok || raw == "" {
		return "", false
	}
	for i := range len(raw) {
		ch := raw[i]
		if (ch < '0' || ch > '9') && (ch < 'a' || ch > 'z') {
			return "", false
		}
	}
	if len(raw) > 9 {
		raw = raw[len(raw)-9:]
	}
	return raw, true
}

func sanitizeProvisionDisplayName(displayName, email string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(displayName) {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := strings.TrimSpace(b.String())
	if cleaned == "" {
		local, _, _ := strings.Cut(email, "@")
		cleaned = local
	}
	runes := []rune(cleaned)
	if len(runes) > 20 {
		return string(runes[:20])
	}
	return cleaned
}

func randomLowerAlnum(length int) (string, error) {
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = lowerAlnumAlphabet[int(b)%len(lowerAlnumAlphabet)]
	}
	return string(out), nil
}

func generateProvisionAccountPassword() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ProvisionNeedTokenMember idempotently checks or provisions a common member
// account for a verified NeedToken storefront customer. Existing accounts with
// the same email are never modified or auto-bound (OWASP anti-takeover).
func ProvisionNeedTokenMember(c *gin.Context) {
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

	now := accountLinkNow()
	nowUnix := now.UTC().Unix()
	if ts < nowUnix-needTokenProvisionMaxSkewSec || ts > nowUnix+needTokenProvisionMaxSkewSec {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Unauthorized"), gin.H{"code": "signature_expired"})
		return
	}

	rawBody, err := io.ReadAll(io.LimitReader(c.Request.Body, needTokenProvisionMaxBodyBytes+1))
	if err != nil || len(rawBody) == 0 || len(rawBody) > needTokenProvisionMaxBodyBytes {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}

	expectedSig := computeProvisionMemberSignature(secret, timestamp, rawBody)
	if !hmac.Equal([]byte(sigHeader), []byte(expectedSig)) {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Unauthorized"), gin.H{"code": "invalid_signature"})
		return
	}

	var req needTokenProvisionMemberRequest
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}

	customerToken, validMerchant := extractMerchantCustomerIDToken(req.MerchantCustomerID)
	email := model.NormalizeEmail(req.Email)
	if req.Source != needTokenSourceStorefront || !validMerchant {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	if req.AllowWithoutEmail {
		if email != "" || req.EmailVerified {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
	} else {
		if !req.EmailVerified || !isValidProvisionEmail(email) {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
			return
		}
		existingCount, err := model.CountUsersByEmail(email)
		if err != nil {
			common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Operation failed"))
			return
		}
		if existingCount > 0 {
			common.ApiSuccess(c, gin.H{"status": "existing_requires_link"})
			return
		}
	}

	randomPassword, err := generateProvisionAccountPassword()
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Operation failed"))
		return
	}
	fallbackDisplay := email
	if fallbackDisplay == "" {
		fallbackDisplay = "nt_" + customerToken
	}
	displayName := sanitizeProvisionDisplayName(req.DisplayName, fallbackDisplay)

	var created *model.User
	for range 3 {
		suffix, randErr := randomLowerAlnum(4)
		if randErr != nil {
			common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Operation failed"))
			return
		}
		candidate := &model.User{
			Username:    "nt_" + customerToken + "_" + suffix,
			Password:    randomPassword,
			Email:       email,
			DisplayName: displayName,
			Role:        common.RoleCommonUser,
			Status:      common.UserStatusEnabled,
			AuthVersion: 1,
		}
		insertErr := candidate.Insert(0)
		if insertErr == nil {
			created = candidate
			break
		}
		if !req.AllowWithoutEmail && (errors.Is(insertErr, model.ErrEmailAlreadyTaken) || model.IsEmailAlreadyTaken(email)) {
			common.ApiSuccess(c, gin.H{"status": "existing_requires_link"})
			return
		}
	}
	if created == nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Operation failed"))
		return
	}

	logger.LogInfo(c.Request.Context(), "needtoken member provisioned native_user_id="+strconv.Itoa(created.Id)+" merchant="+strings.TrimSpace(req.MerchantCustomerID))
	common.ApiSuccess(c, gin.H{
		"status":         "created",
		"native_user_id": created.Id,
		"username":       created.Username,
	})
}

func verifyConsoleSSOTicket(secret []byte, token string, now time.Time) (consoleSSOTicketClaims, error) {
	if len(secret) < 32 || len(token) == 0 || len(token) > accountLinkMaxTokenLen {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	rest, ok := strings.CutPrefix(token, accountLinkTokenPrefix)
	if !ok {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	payloadB64, sigB64, ok := strings.Cut(rest, ".")
	if !ok || payloadB64 == "" || sigB64 == "" || strings.Contains(sigB64, ".") {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != sha256.Size {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	if !hmac.Equal(sig, accountLinkMAC(secret, needTokenConsoleSSOTicketDomain, payloadB64)) {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(payloadB64)
	if err != nil {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	var claims consoleSSOTicketClaims
	if err := common.Unmarshal(raw, &claims); err != nil {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	claims.JTI = strings.TrimSpace(claims.JTI)
	if claims.JTI == "" || len(claims.JTI) > needTokenSSOJTIMaxLen || claims.Source != needTokenSourceStorefront || claims.NativeUID <= 0 {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	unix := now.UTC().Unix()
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.ExpiresAt-claims.IssuedAt > needTokenSSOMaxTicketTTLSec {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	if claims.IssuedAt > unix+needTokenSSOMaxSkewSec || unix >= claims.ExpiresAt {
		return consoleSSOTicketClaims{}, errSSOTicketInvalid
	}
	return claims, nil
}

func consumeConsoleSSOTicketJTI(ctx context.Context, jti string, expiresAt int64, now time.Time) error {
	nowUnix := now.UTC().Unix()
	retainUntil := max(expiresAt+needTokenSSOMaxSkewSec, nowUnix+120)

	ssoReplayMu.Lock()
	for k, exp := range ssoConsumedJTIs {
		if exp <= nowUnix {
			delete(ssoConsumedJTIs, k)
		}
	}
	if existingExp, exists := ssoConsumedJTIs[jti]; exists && existingExp > nowUnix {
		ssoReplayMu.Unlock()
		return errSSOTicketReplayed
	}
	if len(ssoConsumedJTIs) >= needTokenSSOMemoryCap {
		var oldestKey string
		var oldestExp int64
		for k, exp := range ssoConsumedJTIs {
			if oldestKey == "" || exp < oldestExp {
				oldestKey = k
				oldestExp = exp
			}
		}
		if oldestKey != "" {
			delete(ssoConsumedJTIs, oldestKey)
		}
	}
	ssoConsumedJTIs[jti] = retainUntil
	ssoReplayMu.Unlock()

	if common.RedisEnabled && common.RDB != nil {
		ttl := max(2*time.Minute, time.Duration(retainUntil-nowUnix)*time.Second)
		ok, err := common.RDB.SetNX(ctx, needTokenSSORedisKeyPrefix+jti, "1", ttl).Result()
		if err != nil {
			ssoReplayMu.Lock()
			delete(ssoConsumedJTIs, jti)
			ssoReplayMu.Unlock()
			return errSSOReplayStoreFailure
		}
		if !ok {
			return errSSOTicketReplayed
		}
	}

	return nil
}

// ExchangeNeedTokenSSO exchanges a short-lived, single-use HMAC ticket from
// NeedToken Glue into a native console session or MFA login challenge via
// setupLogin without bypassing 2FA or Passkey requirements.
func ExchangeNeedTokenSSO(c *gin.Context) {
	secret, err := accountLinkSecret()
	if err != nil {
		common.ApiErrorStatus(c, http.StatusServiceUnavailable, common.NewMessage("Single sign-on is not enabled"), gin.H{"code": "sso_disabled"})
		return
	}

	var req struct {
		Ticket string `json:"ticket"`
	}
	if err := common.DecodeJson(io.LimitReader(c.Request.Body, 8192), &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid or expired SSO ticket"), gin.H{"code": "invalid_sso_ticket"})
		return
	}

	now := accountLinkNow()
	claims, err := verifyConsoleSSOTicket(secret, strings.TrimSpace(req.Ticket), now)
	if err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid or expired SSO ticket"), gin.H{"code": "invalid_sso_ticket"})
		return
	}

	if err := consumeConsoleSSOTicketJTI(c.Request.Context(), claims.JTI, claims.ExpiresAt, now); err != nil {
		if errors.Is(err, errSSOTicketReplayed) {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid or expired SSO ticket"), gin.H{"code": "sso_ticket_replayed"})
			return
		}
		common.ApiErrorStatus(c, http.StatusServiceUnavailable, common.NewMessage("Operation failed"))
		return
	}

	user, err := accountLinkLoadUser(claims.NativeUID)
	if err != nil || user == nil || user.Id != claims.NativeUID {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid or expired SSO ticket"), gin.H{"code": "invalid_sso_ticket"})
		return
	}
	if user.Status != common.UserStatusEnabled {
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("User has been banned"), gin.H{"code": "account_disabled"})
		return
	}

	c.Set("login_method", "needtoken_sso")
	setupLogin(user, nil, c)
}
