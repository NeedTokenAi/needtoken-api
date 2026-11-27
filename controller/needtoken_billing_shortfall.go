package controller

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/gin-gonic/gin"
)

// AdminListBillingShortfalls returns persisted unbilled operating shortfalls and totals.
func AdminListBillingShortfalls(c *gin.Context) {
	p, _ := strconv.Atoi(c.Query("p"))
	pageSize, _ := strconv.Atoi(c.Query("page_size"))
	userID, _ := strconv.Atoi(c.Query("user_id"))
	status := strings.TrimSpace(c.Query("status"))

	res, err := model.QueryBillingShortfalls(c.Request.Context(), model.BillingShortfallFilter{
		Status:   status,
		UserId:   userID,
		Page:     p,
		PageSize: pageSize,
	})
	if err != nil {
		common.ApiError(c, err)
		return
	}
	c.JSON(http.StatusOK, gin.H{
		"success": true,
		"message": "",
		"data":    res,
	})
}
