package controller

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/shopspring/decimal"
)

const (
	needTokenUniversalGiftCardDomain      = "needtoken:universal-gift-card:v1"
	needTokenUniversalGiftCardClaimPath   = "/api/internal/universal-gift-cards/claim"
	needTokenUniversalGiftCardConfirmPath = "/api/internal/universal-gift-cards/confirm"
	needTokenUniversalGiftCardReleasePath = "/api/internal/universal-gift-cards/release"
)

type universalGiftCardClaimReq struct {
	Code    string `json:"code"`
	Target  string `json:"target"`
	UserRef string `json:"user_ref"`
	ClaimID string `json:"claim_id"`
}

type universalGiftCardClaimResp struct {
	OK           bool   `json:"ok"`
	CardID       string `json:"card_id"`
	BatchNo      string `json:"batch_no"`
	Name         string `json:"name"`
	Code         string `json:"code"`
	ClaimID      string `json:"claim_id"`
	FaceUSDCents int64  `json:"face_usd_cents"`
	ErrorCode    string `json:"error_code,omitempty"`
}

type universalGiftCardConfirmReq struct {
	Code          string `json:"code"`
	ClaimID       string `json:"claim_id"`
	TxnRef        string `json:"txn_ref"`
	CreditDisplay string `json:"credit_display,omitempty"`
}

type universalGiftCardReleaseReq struct {
	Code    string `json:"code"`
	ClaimID string `json:"claim_id"`
	Reason  string `json:"reason,omitempty"`
}

// Package-level hooks overridden only in focused unit tests.
var (
	universalGiftCardHTTPClient = &http.Client{
		Timeout: 6 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	universalGiftCardCreditFn = func(ctx context.Context, userId int, quota int, claimID, cardCode string) error {
		opID := "ugc_claim:" + strings.TrimSpace(claimID)
		remark := "Universal gift card"
		if trimmedCode := strings.TrimSpace(cardCode); trimmedCode != "" {
			remark = fmt.Sprintf("Universal gift card (%s)", trimmedCode)
		}
		_, err := model.ApplyExternalWalletCredit(ctx, userId, quota, opID, remark)
		return err
	}
)

func universalGiftCardConfig() (baseURL string, secret []byte, ok bool) {
	rawURL := strings.TrimSpace(os.Getenv("NEEDTOKEN_GLUE_URL"))
	if rawURL == "" {
		rawURL = strings.TrimSpace(os.Getenv("GLUE_PUBLIC_URL"))
	}
	rawURL = strings.TrimRight(rawURL, "/")
	parsed, err := url.Parse(rawURL)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.Host == "" || parsed.User != nil {
		return "", nil, false
	}
	sec, err := accountLinkSecret()
	if err != nil {
		return "", nil, false
	}
	return rawURL, sec, true
}

func computeUniversalGiftCardSignature(secret []byte, timestamp, path string, rawBody []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write([]byte(needTokenUniversalGiftCardDomain))
	mac.Write([]byte{0})
	mac.Write([]byte(timestamp))
	mac.Write([]byte("."))
	mac.Write([]byte(path))
	mac.Write([]byte("."))
	mac.Write(rawBody)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func newUniversalGiftCardClaimID() string {
	var buf [12]byte
	_, _ = rand.Read(buf[:])
	return "na_" + hex.EncodeToString(buf[:])
}

func postUniversalGiftCardGlue(ctx context.Context, baseURL string, secret []byte, path string, payload any, out any) (int, error) {
	raw, err := common.Marshal(payload)
	if err != nil {
		return 0, err
	}
	reqCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, baseURL+path, bytes.NewReader(raw))
	if err != nil {
		return 0, err
	}
	ts := strconv.FormatInt(accountLinkNow().UTC().Unix(), 10)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("X-NeedToken-Timestamp", ts)
	req.Header.Set("X-NeedToken-Signature", computeUniversalGiftCardSignature(secret, ts, path, raw))

	resp, err := universalGiftCardHTTPClient.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(body) > 65536 {
		return resp.StatusCode, errors.New("glue response too large or unreadable")
	}
	if out != nil && len(body) > 0 {
		_ = common.Unmarshal(body, out)
	}
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, fmt.Errorf("glue universal gift card http %d", resp.StatusCode)
	}
	return resp.StatusCode, nil
}

// tryRedeemUniversalGiftCard attempts two-phase atomic redemption against the
// NeedToken Glue authoritative universal gift card ledger when local redemptions
// did not match. Returns handled=false when Glue universal cards are not configured.
func tryRedeemUniversalGiftCard(ctx context.Context, rawKey string, userID int) (handled bool, quota int, err error) {
	baseURL, secret, configured := universalGiftCardConfig()
	if !configured {
		return false, 0, nil
	}
	code := strings.TrimSpace(strings.ToUpper(rawKey))
	if code == "" || userID <= 0 {
		return false, 0, nil
	}

	claimID := newUniversalGiftCardClaimID()
	var claimResp universalGiftCardClaimResp
	_, claimErr := postUniversalGiftCardGlue(ctx, baseURL, secret, needTokenUniversalGiftCardClaimPath, universalGiftCardClaimReq{
		Code:    code,
		Target:  "newapi",
		UserRef: fmt.Sprintf("newapi:user:%d", userID),
		ClaimID: claimID,
	}, &claimResp)
	if claimErr != nil || !claimResp.OK || claimResp.FaceUSDCents <= 0 {
		if claimErr == nil {
			claimErr = errors.New("universal gift card claim rejected")
		}
		return true, 0, claimErr
	}

	resolvedCode := strings.TrimSpace(claimResp.Code)
	if resolvedCode == "" {
		resolvedCode = code
	}
	resolvedClaimID := strings.TrimSpace(claimResp.ClaimID)
	if resolvedClaimID == "" {
		resolvedClaimID = claimID
	}

	if common.QuotaPerUnit <= 0 {
		_, _ = postUniversalGiftCardGlue(context.Background(), baseURL, secret, needTokenUniversalGiftCardReleasePath, universalGiftCardReleaseReq{
			Code:    resolvedCode,
			ClaimID: resolvedClaimID,
			Reason:  "invalid_quota_per_unit",
		}, nil)
		return true, 0, errors.New("invalid QuotaPerUnit")
	}

	usd := decimal.New(claimResp.FaceUSDCents, -2)
	quotaDec := usd.Mul(decimal.NewFromFloat(common.QuotaPerUnit))
	creditedQuota, convErr := common.WalletQuotaFromDecimalStrict(quotaDec)
	if convErr != nil || creditedQuota <= 0 || common.ValidateWalletQuota(creditedQuota) != nil {
		_, _ = postUniversalGiftCardGlue(context.Background(), baseURL, secret, needTokenUniversalGiftCardReleasePath, universalGiftCardReleaseReq{
			Code:    resolvedCode,
			ClaimID: resolvedClaimID,
			Reason:  "quota_conversion_overflow",
		}, nil)
		if convErr == nil {
			convErr = errors.New("invalid universal gift card quota")
		}
		return true, 0, convErr
	}

	if creditErr := universalGiftCardCreditFn(ctx, userID, creditedQuota, resolvedClaimID, resolvedCode); creditErr != nil {
		_, _ = postUniversalGiftCardGlue(context.Background(), baseURL, secret, needTokenUniversalGiftCardReleasePath, universalGiftCardReleaseReq{
			Code:    resolvedCode,
			ClaimID: resolvedClaimID,
			Reason:  "newapi_wallet_credit_failed",
		}, nil)
		return true, 0, creditErr
	}

	confirmPayload := universalGiftCardConfirmReq{
		Code:          resolvedCode,
		ClaimID:       resolvedClaimID,
		TxnRef:        fmt.Sprintf("newapi:topup:user:%d:%s", userID, resolvedClaimID),
		CreditDisplay: fmt.Sprintf("$%s USD", usd.StringFixed(2)),
	}
	var confirmErr error
	for attempt := range 3 {
		if _, err := postUniversalGiftCardGlue(context.Background(), baseURL, secret, needTokenUniversalGiftCardConfirmPath, confirmPayload, nil); err == nil {
			confirmErr = nil
			break
		} else {
			confirmErr = err
			if attempt < 2 {
				time.Sleep(time.Duration(attempt+1) * 150 * time.Millisecond)
			}
		}
	}
	if confirmErr != nil {
		common.SysError(common.LogText("universal gift card confirm failed after credit (code=%s claim_id=%s user_id=%d): %s", resolvedCode, resolvedClaimID, userID, confirmErr.Error()))
	}
	return true, creditedQuota, nil
}
