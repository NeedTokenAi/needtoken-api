package controller

import (
	"errors"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

type memberConsoleWalletCreditReq struct {
	OperationId string `json:"operation_id"`
	Quota       int    `json:"quota"`
	Remark      string `json:"remark"`
}

type memberConsoleWalletDebitReq struct {
	OperationId  string `json:"operation_id"`
	Quota        int    `json:"quota"`
	AllowPartial bool   `json:"allow_partial"`
	Remark       string `json:"remark"`
}

type memberConsoleWalletRedeemReq struct {
	Code string `json:"code"`
}

// MemberConsoleWalletCredit atomically and idempotently credits a member's unified User.Quota.
func MemberConsoleWalletCredit(c *gin.Context) {
	user, rawBody, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}
	var req memberConsoleWalletCreditReq
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request payload"), gin.H{"code": "invalid_request"})
		return
	}
	res, err := model.ApplyExternalWalletCredit(c.Request.Context(), user.Id, req.Quota, strings.TrimSpace(req.OperationId), req.Remark)
	if err != nil {
		writeUnifiedWalletError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": res})
}

// MemberConsoleWalletDebit atomically and idempotently debits a member's unified User.Quota.
func MemberConsoleWalletDebit(c *gin.Context) {
	user, rawBody, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}
	var req memberConsoleWalletDebitReq
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request payload"), gin.H{"code": "invalid_request"})
		return
	}
	res, err := model.ApplyExternalWalletDebit(c.Request.Context(), user.Id, req.Quota, req.AllowPartial, strings.TrimSpace(req.OperationId), req.Remark)
	if err != nil {
		writeUnifiedWalletError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{"success": true, "message": "", "data": res})
}

// MemberConsoleWalletRedeem redeems either a native redemption code or a Glue universal
// gift card directly into the member's unified User.Quota.
func MemberConsoleWalletRedeem(c *gin.Context) {
	user, rawBody, ok := verifyMemberConsoleRequest(c)
	if !ok {
		return
	}
	var req memberConsoleWalletRedeemReq
	if err := common.Unmarshal(rawBody, &req); err != nil {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid request payload"), gin.H{"code": "invalid_request"})
		return
	}
	code := strings.TrimSpace(req.Code)
	if code == "" {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Redemption code is required"), gin.H{"code": "invalid_request"})
		return
	}

	source := "redemption"
	creditedQuota, err := model.Redeem(code, user.Id)
	if err != nil {
		handled, universalQuota, universalErr := tryRedeemUniversalGiftCard(c.Request.Context(), code, user.Id)
		if !handled {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid or already redeemed code"), gin.H{"code": "redeem_failed"})
			return
		}
		if universalErr != nil {
			common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid or already redeemed code"), gin.H{"code": "redeem_failed", "detail": universalErr.Error()})
			return
		}
		source = "universal_gift_card"
		creditedQuota = universalQuota
	}

	balanceQuota, balErr := model.GetUserQuota(user.Id, false)
	if balErr != nil {
		balanceQuota = user.Quota + creditedQuota
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data": gin.H{
			"source":         source,
			"credited_quota": creditedQuota,
			"balance_quota":  balanceQuota,
			"quota_per_unit": common.QuotaPerUnit,
		},
	})
}

func writeUnifiedWalletError(c *gin.Context, err error) {
	switch {
	case errors.Is(err, model.ErrExternalGrantRolledBack):
		common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Operation was already rolled back"), gin.H{"code": "operation_rolled_back"})
	case errors.Is(err, model.ErrExternalGrantConflict):
		common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Operation payload conflict"), gin.H{"code": "operation_conflict"})
	case errors.Is(err, model.ErrWalletInsufficientBalance):
		common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Insufficient wallet balance"), gin.H{"code": "insufficient_balance"})
	case errors.Is(err, model.ErrExternalGrantInvalid):
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid wallet operation"), gin.H{"code": "invalid_request"})
	default:
		common.ApiErrorStatus(c, http.StatusInternalServerError, common.NewMessage("Wallet operation failed"), gin.H{"code": "wallet_error"})
	}
}
