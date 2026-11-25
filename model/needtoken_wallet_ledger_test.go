package model

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupUnifiedWalletTestDB(t *testing.T) *gorm.DB {
	t.Helper()
	prevDB, prevLogDB := DB, LOG_DB
	prevMain, prevLog := common.MainDatabaseType(), common.LogDatabaseType()
	prevRedis := common.RedisEnabled
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	common.RedisEnabled = false
	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	DB, LOG_DB = db, db
	initCol()
	require.NoError(t, db.AutoMigrate(&User{}, &ExternalGrant{}, &SubscriptionPlan{}, &UserSubscription{}, &Log{}))
	t.Cleanup(func() {
		DB, LOG_DB = prevDB, prevLogDB
		common.SetDatabaseTypes(prevMain, prevLog)
		common.RedisEnabled = prevRedis
		initCol()
		_ = sqlDB.Close()
	})
	return db
}

func TestExternalWalletCreditDebitIdempotencyAndBounds(t *testing.T) {
	db := setupUnifiedWalletTestDB(t)
	ctx := context.Background()
	user := User{Username: "unified-wallet-user", Password: "x", Status: common.UserStatusEnabled, Group: "default", Quota: 1000, AffCode: "uw1"}
	require.NoError(t, db.Create(&user).Error)

	// Credit is atomic and idempotent.
	res1, err := ApplyExternalWalletCredit(ctx, user.Id, 2500, "recharge:101", "storefront recharge")
	require.NoError(t, err)
	assert.False(t, res1.Replayed)
	assert.Equal(t, 2500, res1.AppliedQuota)
	assert.Equal(t, 3500, res1.BalanceQuota)

	resReplay, err := ApplyExternalWalletCredit(ctx, user.Id, 2500, "recharge:101", "storefront recharge")
	require.NoError(t, err)
	assert.True(t, resReplay.Replayed)
	assert.Equal(t, 2500, resReplay.AppliedQuota)
	assert.Equal(t, 3500, resReplay.BalanceQuota)

	// Reusing the same operation_id with a different quota must conflict.
	_, err = ApplyExternalWalletCredit(ctx, user.Id, 9999, "recharge:101", "storefront recharge")
	assert.ErrorIs(t, err, ErrExternalGrantConflict)

	// Full debit fails when balance is insufficient.
	_, err = ApplyExternalWalletDebit(ctx, user.Id, 4000, false, "order:201:pay", "too large")
	assert.ErrorIs(t, err, ErrWalletInsufficientBalance)

	// Partial debit drains available balance and replays idempotently.
	debit1, err := ApplyExternalWalletDebit(ctx, user.Id, 4000, true, "order:202:pay", "order partial")
	require.NoError(t, err)
	assert.False(t, debit1.Replayed)
	assert.Equal(t, 3500, debit1.AppliedQuota)
	assert.Equal(t, 0, debit1.BalanceQuota)

	debitReplay, err := ApplyExternalWalletDebit(ctx, user.Id, 4000, true, "order:202:pay", "order partial")
	require.NoError(t, err)
	assert.True(t, debitReplay.Replayed)
	assert.Equal(t, 3500, debitReplay.AppliedQuota)
	assert.Equal(t, 0, debitReplay.BalanceQuota)
}

func TestPostConsumeUserSubscriptionDeltaWithOverflow(t *testing.T) {
	db := setupUnifiedWalletTestDB(t)
	now := time.Now().Unix()

	user := User{Username: "overflow-user", Password: "x", Status: common.UserStatusEnabled, Group: "default", Quota: 500, AffCode: "ov1"}
	require.NoError(t, db.Create(&user).Error)

	plan := SubscriptionPlan{Title: "Claude Pack", Enabled: true, DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 200}
	require.NoError(t, db.Create(&plan).Error)

	sub1 := UserSubscription{
		UserId: user.Id, PlanId: plan.Id, Status: "active",
		AllowedGroups: "claude", AllowWalletOverflow: true,
		AmountTotal: 200, AmountUsed: 150, StartTime: now - 100, EndTime: now + 3600,
	}
	sub2 := UserSubscription{
		UserId: user.Id, PlanId: plan.Id, Status: "active",
		AllowedGroups: "", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 40, StartTime: now - 100, EndTime: now + 7200,
	}
	require.NoError(t, db.Create(&sub1).Error)
	require.NoError(t, db.Create(&sub2).Error)

	// Delta=180 -> 50 fills sub1, 60 fills sub2, remaining 70 spills seamlessly into User.Quota (500 -> 430).
	res, err := PostConsumeUserSubscriptionDeltaWithOverflow(sub1.Id, user.Id, "claude", 180)
	require.NoError(t, err)
	assert.Equal(t, int64(50), res.PrimaryDelta)
	assert.Equal(t, int64(60), res.SecondarySubDelta)
	assert.Equal(t, int64(70), res.WalletOverflowDelta)

	var refreshed1, refreshed2 UserSubscription
	require.NoError(t, db.First(&refreshed1, sub1.Id).Error)
	require.NoError(t, db.First(&refreshed2, sub2.Id).Error)
	assert.Equal(t, int64(200), refreshed1.AmountUsed)
	assert.Equal(t, int64(100), refreshed2.AmountUsed)

	var refreshedUser User
	require.NoError(t, db.First(&refreshedUser, user.Id).Error)
	assert.Equal(t, 430, refreshedUser.Quota)

	// Disabling wallet overflow rejects additional spillover cleanly.
	subNoOverflow := UserSubscription{
		UserId: user.Id, PlanId: plan.Id, Status: "active",
		AllowedGroups: "openai", AllowWalletOverflow: false,
		AmountTotal: 100, AmountUsed: 90, StartTime: now - 100, EndTime: now + 3600,
	}
	require.NoError(t, db.Create(&subNoOverflow).Error)
	_, err = PostConsumeUserSubscriptionDeltaWithOverflow(subNoOverflow.Id, user.Id, "openai", 50)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "wallet_overflow_disabled")
}
