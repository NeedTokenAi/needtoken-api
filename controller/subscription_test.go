package controller

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestAdminCreateSubscriptionPlanEnabledFlag(t *testing.T) {
	gin.SetMode(gin.TestMode)

	origDB := model.DB
	db, err := gorm.Open(sqlite.Open("file:sub_plan_create_enabled?mode=memory&cache=shared"), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.SubscriptionPlan{}))
	model.DB = db
	t.Cleanup(func() {
		model.DB = origDB
	})

	ps := operation_setting.GetPaymentSetting()
	origConfirmed := ps.ComplianceConfirmed
	origVersion := ps.ComplianceTermsVersion
	ps.ComplianceConfirmed = true
	ps.ComplianceTermsVersion = operation_setting.CurrentComplianceTermsVersion
	t.Cleanup(func() {
		ps.ComplianceConfirmed = origConfirmed
		ps.ComplianceTermsVersion = origVersion
	})

	cases := []struct {
		name        string
		body        string
		wantEnabled bool
	}{
		{
			name:        "explicit false persists as disabled",
			body:        `{"plan":{"title":"Disabled Plan","price_amount":10,"total_amount":5000000,"enabled":false}}`,
			wantEnabled: false,
		},
		{
			name:        "explicit true persists as enabled",
			body:        `{"plan":{"title":"Enabled Plan","price_amount":10,"total_amount":5000000,"enabled":true}}`,
			wantEnabled: true,
		},
		{
			name:        "omitted enabled defaults to true",
			body:        `{"plan":{"title":"Default Enabled Plan","price_amount":10,"total_amount":5000000}}`,
			wantEnabled: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(rec)
			c.Request = httptest.NewRequest(http.MethodPost, "/api/subscription/admin/plans", bytes.NewBufferString(tc.body))
			c.Request.Header.Set("Content-Type", "application/json")

			AdminCreateSubscriptionPlan(c)
			require.Equal(t, http.StatusOK, rec.Code)

			var resp struct {
				Success bool                   `json:"success"`
				Data    model.SubscriptionPlan `json:"data"`
			}
			require.NoError(t, common.Unmarshal(rec.Body.Bytes(), &resp))
			require.True(t, resp.Success)
			require.Greater(t, resp.Data.Id, 0)
			assert.Equal(t, tc.wantEnabled, resp.Data.Enabled)

			var persisted model.SubscriptionPlan
			require.NoError(t, db.First(&persisted, resp.Data.Id).Error)
			assert.Equal(t, tc.wantEnabled, persisted.Enabled)
		})
	}
}
