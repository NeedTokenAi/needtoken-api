package controller

import (
	"errors"
	"net/http"
	"strconv"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// External fulfillment is a server-to-server operation. AdminAuth still checks
// the owner, role, account status, PAT expiry and the route's narrow scope.
func requireExternalGrantPAT(c *gin.Context) bool {
	if c.GetInt("access_token_id") > 0 {
		return true
	}
	common.ApiErrorStatus(c, http.StatusForbidden, common.NewMessage("Unauthorized, insufficient privileges"))
	return false
}

func externalGrantError(c *gin.Context, err error) {
	status, code := http.StatusServiceUnavailable, "fulfillment_unavailable"
	message := "Operation failed"
	switch {
	case errors.Is(err, model.ErrExternalGrantConflict):
		status, code = http.StatusConflict, "operation_conflict"
	case errors.Is(err, model.ErrExternalGrantPlanRevision):
		status, code = http.StatusConflict, "plan_revision_mismatch"
	case errors.Is(err, model.ErrExternalGrantQuotaUnit):
		status, code = http.StatusConflict, "quota_unit_mismatch"
	case errors.Is(err, model.ErrExternalGrantTokenLimit):
		status, code = http.StatusConflict, "token_limit_reached"
	case errors.Is(err, model.ErrExternalGrantInvalid):
		status, code, message = http.StatusBadRequest, "invalid_fulfillment", "Invalid parameters"
	case errors.Is(err, gorm.ErrRecordNotFound):
		status, code = http.StatusNotFound, "fulfillment_not_found"
	}
	common.ApiErrorStatus(c, status, common.NewMessage(message), gin.H{"code": code})
}

func AdminCreateSubscriptionFulfillment(c *gin.Context) {
	if !requireExternalGrantPAT(c) || !requirePaymentCompliance(c) {
		return
	}
	var req struct {
		OperationId  string `json:"operation_id"`
		UserId       int    `json:"user_id"`
		PlanId       int    `json:"plan_id"`
		PlanRevision string `json:"plan_revision"`
		IssueAPIKey  bool   `json:"issue_api_key"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		externalGrantError(c, model.ErrExternalGrantInvalid)
		return
	}
	grant, err := model.CreateExternalGrant(c.Request.Context(), c.GetInt("id"), model.ExternalGrantRequest{
		Kind: model.ExternalGrantSubscription, OperationId: req.OperationId, UserId: req.UserId,
		PlanId: req.PlanId, PlanRevision: req.PlanRevision, IssueAPIKey: req.IssueAPIKey,
	})
	if err != nil {
		externalGrantError(c, err)
		return
	}
	common.ApiSuccess(c, grant)
}

// Capabilities are available only to the same scoped administrative PATs that
// can read subscription fulfillment receipts. No credentials are returned.
func AdminGetFulfillmentCapabilities(c *gin.Context) {
	if !requireExternalGrantPAT(c) {
		return
	}
	common.ApiSuccess(c, gin.H{
		"source": "new-api", "contract_version": 1,
		"managed_api_key_issuance": true, "delivery_mode": "native_console",
		"payment_compliance_confirmed":  operation_setting.IsPaymentComplianceConfirmed(),
		"subscription_write_authorized": service.AccessTokenScopeGranted(c.GetStringSlice("access_token_scopes"), "billing:write"),
		"collected_at":                  common.GetTimestamp(),
	})
}

func AdminGetSubscriptionFulfillment(c *gin.Context) {
	if !requireExternalGrantPAT(c) {
		return
	}
	grant, err := model.GetExternalGrant(c.Request.Context(), c.Param("operation_id"), model.ExternalGrantSubscription)
	if err != nil {
		externalGrantError(c, err)
		return
	}
	common.ApiSuccess(c, grant)
}

func AdminCreateRedemptionFulfillment(c *gin.Context) {
	if !requireExternalGrantPAT(c) || !requirePaymentCompliance(c) {
		return
	}
	var req struct {
		OperationId          string `json:"operation_id"`
		Name                 string `json:"name"`
		Quota                int    `json:"quota"`
		ExpiredTime          int64  `json:"expired_time"`
		ExpectedQuotaPerUnit int64  `json:"expected_quota_per_unit"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		externalGrantError(c, model.ErrExternalGrantInvalid)
		return
	}
	grant, err := model.CreateExternalGrant(c.Request.Context(), c.GetInt("id"), model.ExternalGrantRequest{
		Kind: model.ExternalGrantRedemption, OperationId: req.OperationId, Name: req.Name, Quota: req.Quota, ExpiredTime: req.ExpiredTime, ExpectedQuotaPerUnit: req.ExpectedQuotaPerUnit,
	})
	if err != nil {
		externalGrantError(c, err)
		return
	}
	common.ApiSuccess(c, grant)
}

func AdminGetRedemptionFulfillment(c *gin.Context) {
	if !requireExternalGrantPAT(c) {
		return
	}
	grant, err := model.GetExternalGrant(c.Request.Context(), c.Param("operation_id"), model.ExternalGrantRedemption)
	if err != nil {
		externalGrantError(c, err)
		return
	}
	common.ApiSuccess(c, grant)
}

func AdminGetCustomerQuotaSummary(c *gin.Context) {
	if !requireExternalGrantPAT(c) {
		return
	}
	userId, err := strconv.Atoi(c.Param("id"))
	if err != nil || userId <= 0 {
		externalGrantError(c, model.ErrExternalGrantInvalid)
		return
	}
	summary, err := model.GetCustomerQuotaSummary(c.Request.Context(), userId)
	if err != nil {
		externalGrantError(c, err)
		return
	}
	common.ApiSuccess(c, summary)
}
