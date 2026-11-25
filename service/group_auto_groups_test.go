package service

import (
	"fmt"
	"net/http/httptest"
	"testing"

	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func configureRequestAutoGroupsTest(t *testing.T) {
	t.Helper()
	originalMax := setting.GetMaxTokenAutoGroups()
	originalAutoGroups := setting.AutoGroups2JsonString()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalRatios := ratio_setting.GroupRatio2JSONString()
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("2"))
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["vip","default","svip"]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP","svip":"SVIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":1,"svip":1}`))
	t.Cleanup(func() {
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(fmt.Sprintf("%d", originalMax)))
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(originalAutoGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalRatios))
	})
}

func newRequestAutoGroupsContext() *gin.Context {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	return ctx
}

func TestGetRequestAutoGroupsInheritedListIsNotLimited(t *testing.T) {
	configureRequestAutoGroupsTest(t)
	ctx := newRequestAutoGroupsContext()

	groups := GetRequestAutoGroups(ctx, "default")

	assert.Equal(t, []string{"vip", "default", "svip"}, groups)
}

func TestGetRequestAutoGroupsFiltersBeforeApplyingCurrentLimit(t *testing.T) {
	configureRequestAutoGroupsTest(t)
	ctx := newRequestAutoGroupsContext()
	common.SetContextKey(ctx, constant.ContextKeyTokenAutoGroups, []string{"revoked", "vip", "default", "svip"})
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`[]`))

	groups := GetRequestAutoGroups(ctx, "default")

	assert.Equal(t, []string{"vip", "default"}, groups)
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("1"))
	assert.Equal(t, []string{"vip"}, GetRequestAutoGroups(ctx, "default"))
}

func TestGetRequestAutoGroupsDoesNotFallBackAfterPermissionChange(t *testing.T) {
	configureRequestAutoGroupsTest(t)
	ctx := newRequestAutoGroupsContext()
	common.SetContextKey(ctx, constant.ContextKeyTokenAutoGroups, []string{"vip"})
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default"}`))

	groups := GetRequestAutoGroups(ctx, "default")

	assert.Empty(t, groups)
}

func TestGetRoutableGlobalAutoGroupsExcludesUnpricedGroups(t *testing.T) {
	configureRequestAutoGroupsTest(t)
	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`["vip","no-ratio","default","auto","vip"]`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":1}`))

	assert.Equal(t, []string{"vip", "default"}, GetRoutableGlobalAutoGroups())
}

func TestGenerateTextOtherInfoRecordsSubscriptionSecondaryAndWalletOverflow(t *testing.T) {
	ctx := newRequestAutoGroupsContext()
	now := time.Now()
	info := &relaycommon.RelayInfo{
		ChannelMeta:                           &relaycommon.ChannelMeta{},
		StartTime:                             now,
		FirstResponseTime:                     now.Add(15 * time.Millisecond),
		BillingSource:                         "subscription",
		SubscriptionId:                        11,
		SubscriptionPlanId:                    3,
		SubscriptionPlanTitle:                 "Pro",
		SubscriptionPreConsumed:               40,
		SubscriptionPostDelta:                 20,
		SubscriptionSecondaryDelta:            30,
		SubscriptionWalletOverflow:            15,
		SubscriptionAmountTotal:               100,
		SubscriptionAmountUsedAfterPreConsume: 80,
	}

	other := GenerateTextOtherInfo(ctx, info, 1, 1, 1, 0, 0, 0, 1)
	snap := other.Snapshot()
	assert.Equal(t, int64(90), snap["subscription_consumed"])
	assert.Equal(t, int64(30), snap["subscription_secondary_delta"])
	assert.Equal(t, int64(15), snap["wallet_quota_deducted"])
	assert.Equal(t, int64(100), snap["subscription_used"])
	assert.Equal(t, int64(0), snap["subscription_remain"])
}
