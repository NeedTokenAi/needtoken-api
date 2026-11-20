package controller

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
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
	accountLinkTokenPrefix     = "v1."
	accountLinkChallengeDomain = "needtoken:account-link:challenge:v1"
	accountLinkApprovalDomain  = "needtoken:account-link:approval:v1"
	accountLinkMaxTokenLen     = 4096
	accountLinkApprovalTTL     = 2 * time.Minute
	accountLinkMaxClockSkew    = 30 * time.Second
)

var (
	errAccountLinkDisabled = errors.New("account link is not configured")
	errAccountLinkInvalid  = errors.New("invalid account link challenge")
	errAccountLinkExpired  = errors.New("account link challenge expired")
)

type accountLinkChallengeClaims struct {
	CID         string `json:"cid"`
	Source      string `json:"src"`
	MerchantID  string `json:"mid"`
	MemberLabel string `json:"mlabel"`
	ReturnURL   string `json:"ret"`
	IssuedAt    int64  `json:"iat"`
	ExpiresAt   int64  `json:"exp"`
}

type accountLinkApprovalClaims struct {
	CID          string `json:"cid"`
	NativeUserID int    `json:"uid"`
	Username     string `json:"u,omitempty"`
	Nonce        string `json:"nonce"`
	IssuedAt     int64  `json:"iat"`
	ExpiresAt    int64  `json:"exp"`
}

// accountLinkNow is a package-level clock hook overridden only in unit tests.
var accountLinkNow = func() time.Time { return time.Now().UTC() }

// accountLinkLoadUser is overridden in unit tests that do not require a DB.
var accountLinkLoadUser = func(id int) (*model.User, error) {
	return model.GetUserById(id, false)
}

func accountLinkSecret() ([]byte, error) {
	secret := strings.TrimSpace(os.Getenv("NEEDTOKEN_LINKING_SECRET"))
	if len(secret) < 32 {
		return nil, errAccountLinkDisabled
	}
	return []byte(secret), nil
}

func accountLinkMAC(secret []byte, domain, payload string) []byte {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(domain))
	mac.Write([]byte{0})
	mac.Write([]byte(payload))
	return mac.Sum(nil)
}

func verifyAccountLinkChallenge(secret []byte, token string, now time.Time) (accountLinkChallengeClaims, error) {
	if len(secret) < 32 || len(token) == 0 || len(token) > accountLinkMaxTokenLen {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	rest, ok := strings.CutPrefix(token, accountLinkTokenPrefix)
	if !ok {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	payload, sigB64, ok := strings.Cut(rest, ".")
	if !ok || payload == "" || sigB64 == "" || strings.Contains(sigB64, ".") {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	sig, err := base64.RawURLEncoding.DecodeString(sigB64)
	if err != nil || len(sig) != sha256.Size {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	if !hmac.Equal(sig, accountLinkMAC(secret, accountLinkChallengeDomain, payload)) {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	if err != nil {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	var claims accountLinkChallengeClaims
	if err := common.Unmarshal(raw, &claims); err != nil {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	if claims.CID == "" || claims.Source == "" || claims.MerchantID == "" || !validAccountLinkReturnURL(claims.ReturnURL) {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	unix := now.UTC().Unix()
	if claims.IssuedAt <= 0 || claims.ExpiresAt <= claims.IssuedAt || claims.IssuedAt > unix+int64(accountLinkMaxClockSkew.Seconds()) {
		return accountLinkChallengeClaims{}, errAccountLinkInvalid
	}
	if unix >= claims.ExpiresAt {
		return accountLinkChallengeClaims{}, errAccountLinkExpired
	}
	return claims, nil
}

func signAccountLinkApproval(secret []byte, claims accountLinkApprovalClaims) (string, error) {
	raw, err := common.Marshal(claims)
	if err != nil {
		return "", err
	}
	payload := base64.RawURLEncoding.EncodeToString(raw)
	sig := accountLinkMAC(secret, accountLinkApprovalDomain, payload)
	return accountLinkTokenPrefix + payload + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

func validAccountLinkReturnURL(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return false
	}
	return u.Path == "/account-link/callback"
}

func accountLinkRandomNonce() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func requireAccountLinkBrowserSession(c *gin.Context) (int, bool) {
	if c.GetInt("access_token_id") > 0 {
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Browser session required for account linking"))
		return 0, false
	}
	userId := c.GetInt("id")
	if userId <= 0 {
		common.ApiErrorStatus(c, http.StatusUnauthorized, common.NewMessage("Unauthorized"))
		return 0, false
	}
	return userId, true
}

func writeAccountLinkError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, errAccountLinkDisabled):
		common.ApiErrorStatus(c, http.StatusServiceUnavailable, common.NewMessage("Account linking is not enabled"), gin.H{"code": "account_link_disabled"})
	case errors.Is(err, errAccountLinkExpired):
		common.ApiErrorStatus(c, http.StatusGone, common.NewMessage("Account link challenge has expired"), gin.H{"code": "link_challenge_expired"})
	default:
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid account link challenge"), gin.H{"code": "invalid_link_challenge"})
	}
}

// PreviewAccountLinkChallenge verifies a glue-signed storefront linking
// challenge and returns safe display details for the authenticated browser user.
func PreviewAccountLinkChallenge(c *gin.Context) {
	if _, ok := requireAccountLinkBrowserSession(c); !ok {
		return
	}
	secret, err := accountLinkSecret()
	if err != nil {
		writeAccountLinkError(c, err)
		return
	}
	var req struct {
		Challenge string `json:"challenge" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeAccountLinkError(c, errAccountLinkInvalid)
		return
	}
	claims, err := verifyAccountLinkChallenge(secret, strings.TrimSpace(req.Challenge), accountLinkNow())
	if err != nil {
		writeAccountLinkError(c, err)
		return
	}
	retURL, _ := url.Parse(claims.ReturnURL)
	common.ApiSuccess(c, gin.H{
		"challenge_id":  claims.CID,
		"source":        claims.Source,
		"member_label":  claims.MemberLabel,
		"return_origin": retURL.Scheme + "://" + retURL.Host,
		"expires_at":    claims.ExpiresAt,
	})
}

// ApproveAccountLinkChallenge binds the currently authenticated active user's
// server-side ID to a verified storefront challenge and returns a short-lived
// HMAC-signed approval assertion for the storefront callback.
func ApproveAccountLinkChallenge(c *gin.Context) {
	userId, ok := requireAccountLinkBrowserSession(c)
	if !ok {
		return
	}
	secret, err := accountLinkSecret()
	if err != nil {
		writeAccountLinkError(c, err)
		return
	}
	var req struct {
		Challenge string `json:"challenge" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		writeAccountLinkError(c, errAccountLinkInvalid)
		return
	}
	now := accountLinkNow()
	claims, err := verifyAccountLinkChallenge(secret, strings.TrimSpace(req.Challenge), now)
	if err != nil {
		writeAccountLinkError(c, err)
		return
	}
	user, err := accountLinkLoadUser(userId)
	if err != nil || user == nil || user.Id != userId || user.Status != common.UserStatusEnabled {
		common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Account is unavailable for linking"), gin.H{"code": "account_unavailable"})
		return
	}
	nonce, err := accountLinkRandomNonce()
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Operation failed"))
		return
	}
	expiresAt := now.Add(accountLinkApprovalTTL).Unix()
	approval, err := signAccountLinkApproval(secret, accountLinkApprovalClaims{
		CID:          claims.CID,
		NativeUserID: user.Id,
		Username:     user.Username,
		Nonce:        nonce,
		IssuedAt:     now.Unix(),
		ExpiresAt:    expiresAt,
	})
	if err != nil {
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Operation failed"))
		return
	}
	// Audit without logging any secret, challenge token, or approval token.
	logger.LogInfo(c.Request.Context(), "needtoken account link approved for user "+strconv.Itoa(user.Id)+" challenge "+claims.CID)
	common.ApiSuccess(c, gin.H{
		"challenge_id": claims.CID,
		"approval":     approval,
		"return_url":   claims.ReturnURL,
		"expires_at":   expiresAt,
	})
}
