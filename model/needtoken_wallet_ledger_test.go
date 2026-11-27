package model

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/postgres"
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

	// Once a debit operation has been compensated via :rollback, replaying the original
	// operation_id MUST fail closed instead of returning a stale receipt.
	rbRes, err := ApplyExternalWalletCredit(ctx, user.Id, 3500, "order:202:pay:rollback", "order rollback")
	require.NoError(t, err)
	assert.False(t, rbRes.Replayed)
	assert.Equal(t, 3500, rbRes.BalanceQuota)

	_, err = ApplyExternalWalletDebit(ctx, user.Id, 4000, true, "order:202:pay", "order partial")
	assert.ErrorIs(t, err, ErrExternalGrantRolledBack)
	balAfter, err := GetUserQuota(user.Id, false)
	require.NoError(t, err)
	assert.Equal(t, 3500, balAfter)

	// Universal gift card claim credits are durably indexed by ugc_claim:<claim_id>.
	gcRes, err := ApplyExternalWalletCredit(ctx, user.Id, 500, "ugc_claim:na_test123", "Universal gift card (NTGC-TEST)")
	require.NoError(t, err)
	assert.False(t, gcRes.Replayed)
	grant, err := GetExternalGrant(ctx, "ugc_claim:na_test123", ExternalGrantWalletCredit)
	require.NoError(t, err)
	assert.Equal(t, user.Id, grant.UserId)
	assert.Equal(t, 500, grant.Quota)
	assert.Equal(t, "succeeded", grant.State)
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
	assert.ErrorIs(t, err, ErrSubscriptionWalletOverflowDisabled)
	assert.Contains(t, err.Error(), "wallet_overflow_disabled")
	// Primary remaining 10 must still be consumed while wallet balance stays untouched.
	var refreshedNoOv UserSubscription
	require.NoError(t, db.First(&refreshedNoOv, subNoOverflow.Id).Error)
	assert.Equal(t, int64(100), refreshedNoOv.AmountUsed)
	require.NoError(t, db.First(&refreshedUser, user.Id).Error)
	assert.Equal(t, 430, refreshedUser.Quota)

	// subscription_only MUST forbid wallet overflow even when subscription AllowWalletOverflow=true.
	subPref := UserSubscription{
		UserId: user.Id, PlanId: plan.Id, Status: "active",
		AllowedGroups: "gemini", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 90, StartTime: now - 100, EndTime: now + 3600,
	}
	require.NoError(t, db.Create(&subPref).Error)
	prefRes, err := PostConsumeUserSubscriptionDeltaWithOptions(SubscriptionPostConsumeOptions{
		UserSubscriptionId: subPref.Id,
		UserId:             user.Id,
		UsingGroup:         "gemini",
		Delta:              50,
		BillingPreference:  "subscription_only",
	})
	require.ErrorIs(t, err, ErrSubscriptionWalletOverflowDisabled)
	assert.Equal(t, int64(10), prefRes.PrimaryDelta)
	assert.Equal(t, int64(0), prefRes.WalletOverflowDelta)
	assert.Equal(t, int64(40), prefRes.UnbilledOverflow)
	require.NoError(t, db.First(&refreshedUser, user.Id).Error)
	assert.Equal(t, 430, refreshedUser.Quota, "subscription_only must never deduct wallet")

	// Wallet overflow must clamp at zero and never drive User.Quota negative.
	require.NoError(t, db.Model(&User{}).Where("id = ?", user.Id).Update("quota", 15).Error)
	subClamp := UserSubscription{
		UserId: user.Id, PlanId: plan.Id, Status: "active",
		AllowedGroups: "deepseek", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 95, StartTime: now - 100, EndTime: now + 3600,
	}
	require.NoError(t, db.Create(&subClamp).Error)
	clampRes, err := PostConsumeUserSubscriptionDeltaWithOptions(SubscriptionPostConsumeOptions{
		UserSubscriptionId: subClamp.Id,
		UserId:             user.Id,
		UsingGroup:         "deepseek",
		Delta:              50,
		BillingPreference:  "subscription_first",
	})
	require.NoError(t, err)
	assert.Equal(t, int64(5), clampRes.PrimaryDelta)
	assert.Equal(t, int64(15), clampRes.WalletOverflowDelta)
	assert.Equal(t, int64(30), clampRes.UnbilledOverflow)
	require.NoError(t, db.First(&refreshedUser, user.Id).Error)
	assert.Equal(t, 0, refreshedUser.Quota, "wallet quota must never go negative")
}

func TestPreConsumeUserSubscriptionMultiCardSplitAndRefund(t *testing.T) {
	db := setupUnifiedWalletTestDB(t)
	require.NoError(t, db.AutoMigrate(&SubscriptionPreConsumeRecord{}))
	now := time.Now().Unix()

	user := User{Username: "split-sub-user", Password: "x", Status: common.UserStatusEnabled, Group: "default", Quota: 1000, AffCode: "sp1"}
	require.NoError(t, db.Create(&user).Error)

	plan := SubscriptionPlan{Title: "Claude Split Pack", Enabled: true, DurationUnit: SubscriptionDurationMonth, DurationValue: 1, TotalAmount: 100}
	require.NoError(t, db.Create(&plan).Error)

	sub1 := UserSubscription{
		UserId: user.Id, PlanId: plan.Id, Status: "active",
		AllowedGroups: "claude-vip", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 60, StartTime: now - 100, EndTime: now + 3600,
	}
	sub2 := UserSubscription{
		UserId: user.Id, PlanId: plan.Id, Status: "active",
		AllowedGroups: "claude-vip", AllowWalletOverflow: true,
		AmountTotal: 100, AmountUsed: 60, StartTime: now - 100, EndTime: now + 7200,
	}
	require.NoError(t, db.Create(&sub1).Error)
	require.NoError(t, db.Create(&sub2).Error)

	// Each card has 40 remaining; pre-consuming 60 must split across sub1 (40) + sub2 (20).
	res, err := PreConsumeUserSubscriptionForGroup("req-split-1", user.Id, "claude-opus-4-6", "claude-vip", 0, 60)
	require.NoError(t, err)
	assert.Equal(t, sub1.Id, res.UserSubscriptionId)
	assert.Equal(t, int64(60), res.PreConsumed)
	assert.Equal(t, int64(40), res.PrimaryPreConsumed)
	require.Len(t, res.Allocations, 2)
	assert.Equal(t, sub1.Id, res.Allocations[0].UserSubscriptionId)
	assert.Equal(t, int64(40), res.Allocations[0].PreConsumed)
	assert.Equal(t, sub2.Id, res.Allocations[1].UserSubscriptionId)
	assert.Equal(t, int64(20), res.Allocations[1].PreConsumed)

	// Idempotent replay with same requestId must return identical total without double debit.
	replay, err := PreConsumeUserSubscriptionForGroup("req-split-1", user.Id, "claude-opus-4-6", "claude-vip", 0, 60)
	require.NoError(t, err)
	assert.Equal(t, int64(60), replay.PreConsumed)
	require.Len(t, replay.Allocations, 2)

	var s1, s2 UserSubscription
	require.NoError(t, db.First(&s1, sub1.Id).Error)
	require.NoError(t, db.First(&s2, sub2.Id).Error)
	assert.Equal(t, int64(100), s1.AmountUsed)
	assert.Equal(t, int64(80), s2.AmountUsed)

	// Full refund must restore both split cards cleanly and remain idempotent.
	require.NoError(t, RefundSubscriptionPreConsume("req-split-1"))
	require.NoError(t, RefundSubscriptionPreConsume("req-split-1"))
	require.NoError(t, db.First(&s1, sub1.Id).Error)
	require.NoError(t, db.First(&s2, sub2.Id).Error)
	assert.Equal(t, int64(60), s1.AmountUsed)
	assert.Equal(t, int64(60), s2.AmountUsed)

	// Split pre-consume 60 again, then settle actual usage 10 (delta=-50): must LIFO-refund sub2 (-20) then sub1 (-30).
	res2, err := PreConsumeUserSubscriptionForGroup("req-split-2", user.Id, "claude-opus-4-6", "claude-vip", 0, 60)
	require.NoError(t, err)
	settleRes, err := PostConsumeUserSubscriptionDeltaWithOptions(SubscriptionPostConsumeOptions{
		UserSubscriptionId: res2.UserSubscriptionId,
		UserId:             user.Id,
		UsingGroup:         "claude-vip",
		Delta:              -50,
		PreConsumeAllocs:   res2.Allocations,
	})
	require.NoError(t, err)
	assert.Equal(t, int64(-30), settleRes.PrimaryDelta)
	assert.Equal(t, int64(-20), settleRes.SecondarySubDelta)
	require.NoError(t, db.First(&s1, sub1.Id).Error)
	require.NoError(t, db.First(&s2, sub2.Id).Error)
	assert.Equal(t, int64(70), s1.AmountUsed, "sub1 net charge should be 10 (60 -> 70)")
	assert.Equal(t, int64(60), s2.AmountUsed, "sub2 should be fully refunded back to 60")
}

func TestLookupExternalWalletOp(t *testing.T) {
	db := setupUnifiedWalletTestDB(t)

	user := User{Username: "op-lookup-user", Password: "hashed-password", DisplayName: "Op Lookup", AffCode: "AFFOP1", Status: common.UserStatusEnabled, Quota: 10000}
	other := User{Username: "op-lookup-other", Password: "hashed-password", DisplayName: "Op Other", AffCode: "AFFOP2", Status: common.UserStatusEnabled, Quota: 5000}
	require.NoError(t, db.Create(&user).Error)
	require.NoError(t, db.Create(&other).Error)

	ctx := context.Background()

	// 1. Not found op returns Found=false and authoritative User.Quota.
	missing, err := LookupExternalWalletOp(ctx, user.Id, "order:101:debit:missing", ExternalGrantWalletDebit)
	require.NoError(t, err)
	assert.False(t, missing.Found)
	assert.Equal(t, "not_found", missing.Status)
	assert.Equal(t, 10000, missing.BalanceQuota)

	// 2. Committed debit returns Status=debited and AppliedQuota.
	_, err = ApplyExternalWalletDebit(ctx, user.Id, 2500, true, "order:101:debit:ok", "pay order")
	require.NoError(t, err)
	debited, err := LookupExternalWalletOp(ctx, user.Id, "order:101:debit:ok", ExternalGrantWalletDebit)
	require.NoError(t, err)
	assert.True(t, debited.Found)
	assert.Equal(t, "debited", debited.Status)
	assert.Equal(t, 2500, debited.AppliedQuota)
	assert.Equal(t, 7500, debited.BalanceQuota)

	// 3. Cross-user lookup is rejected.
	_, err = LookupExternalWalletOp(ctx, other.Id, "order:101:debit:ok", ExternalGrantWalletDebit)
	assert.ErrorIs(t, err, ErrExternalGrantConflict)

	// 4. Rolled-back debit transitions lookup status to rolled_back.
	_, err = ApplyExternalWalletCredit(ctx, user.Id, 2500, "order:101:debit:ok:rollback", "rollback")
	require.NoError(t, err)
	rolled, err := LookupExternalWalletOp(ctx, user.Id, "order:101:debit:ok", ExternalGrantWalletDebit)
	require.NoError(t, err)
	assert.True(t, rolled.Found)
	assert.Equal(t, "rolled_back", rolled.Status)
	assert.Equal(t, 2500, rolled.AppliedQuota)
	assert.Equal(t, 10000, rolled.BalanceQuota)

	// 5. Credit lookup returns credited.
	creditedStatus, err := LookupExternalWalletOp(ctx, user.Id, "order:101:debit:ok:rollback", ExternalGrantWalletCredit)
	require.NoError(t, err)
	assert.True(t, creditedStatus.Found)
	assert.Equal(t, "credited", creditedStatus.Status)
	assert.Equal(t, 2500, creditedStatus.AppliedQuota)

	// 6. Invalid op ID / kind fails cleanly.
	_, err = LookupExternalWalletOp(ctx, user.Id, "bad op id!", ExternalGrantWalletDebit)
	assert.ErrorIs(t, err, ErrExternalGrantInvalid)
	_, err = LookupExternalWalletOp(ctx, user.Id, "order:101:debit:ok", "bad_kind")
	assert.ErrorIs(t, err, ErrExternalGrantInvalid)
}

func TestNeedTokenBillingRefundAutoMigrateAndExecution(t *testing.T) {
	runCase := func(t *testing.T, db *gorm.DB, dialect common.DatabaseType) {
		t.Helper()
		prevDB, prevLogDB := DB, LOG_DB
		prevMain, prevLog := common.MainDatabaseType(), common.LogDatabaseType()
		prevRedis := common.RedisEnabled
		DB, LOG_DB = db, db
		common.SetDatabaseTypes(dialect, dialect)
		common.RedisEnabled = false
		initCol()
		t.Cleanup(func() {
			DB, LOG_DB = prevDB, prevLogDB
			common.SetDatabaseTypes(prevMain, prevLog)
			common.RedisEnabled = prevRedis
			initCol()
		})

		// Verify AutoMigrate idempotency by running twice.
		require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &NeedTokenBillingRefund{}, &NeedTokenBillingShortfall{}, &SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}))
		require.NoError(t, db.AutoMigrate(&User{}, &Token{}, &NeedTokenBillingRefund{}, &NeedTokenBillingShortfall{}, &SubscriptionPlan{}, &UserSubscription{}, &SubscriptionPreConsumeRecord{}))

		user := User{Username: "billing-refund-u-" + string(dialect), Password: "hashed-password", AffCode: "AFFBR_" + string(dialect), Status: common.UserStatusEnabled, Quota: 500}
		require.NoError(t, db.Create(&user).Error)
		tok := Token{UserId: user.Id, Key: "sk-br-test-" + string(dialect), Name: "br-test", RemainQuota: 500, UsedQuota: 100, Status: common.TokenStatusEnabled}
		require.NoError(t, db.Create(&tok).Error)

		ctx := context.Background()
		opID := "billing:test-model-op-" + string(dialect)
		require.NoError(t, UpsertBillingRefundRecord(ctx, NeedTokenBillingRefund{
			OperationId:   opID,
			RequestId:     "req-br-1",
			UserId:        user.Id,
			TokenId:       tok.Id,
			FundingSource: "wallet",
			WalletAmount:  100,
			TokenConsumed: 100,
		}))
		_, err := MarkBillingRefundPending(ctx, NeedTokenBillingRefund{OperationId: opID})
		require.NoError(t, err)

		completed, err := ExecuteBillingRefund(ctx, opID)
		require.NoError(t, err)
		assert.True(t, completed)

		// Re-executing must be strictly idempotent.
		completedAgain, err := ExecuteBillingRefund(ctx, opID)
		require.NoError(t, err)
		assert.True(t, completedAgain)

		var reloadedUser User
		require.NoError(t, db.First(&reloadedUser, user.Id).Error)
		assert.Equal(t, 600, reloadedUser.Quota)

		var reloadedTok Token
		require.NoError(t, db.First(&reloadedTok, tok.Id).Error)
		assert.Equal(t, 600, reloadedTok.RemainQuota)

		sfOp := "shortfall:test-model-" + string(dialect)
		sfRec := NeedTokenBillingShortfall{
			OperationId:         sfOp,
			RequestId:           "req-sf-1",
			UserId:              user.Id,
			ActualQuota:         60,
			SubscriptionCharged: 15,
			WalletCharged:       15,
			CollectedQuota:      30,
			UnbilledQuota:       30,
			Reason:              "wallet_exhausted",
		}
		require.NoError(t, RecordBillingShortfall(ctx, sfRec))
		require.NoError(t, RecordBillingShortfall(ctx, sfRec))
		sfRes, err := QueryBillingShortfalls(ctx, BillingShortfallFilter{UserId: user.Id})
		require.NoError(t, err)
		assert.Equal(t, int64(1), sfRes.Total)
		assert.Equal(t, int64(30), sfRes.TotalUnbilledQuota)
	}

	t.Run("sqlite", func(t *testing.T) {
		db := setupUnifiedWalletTestDB(t)
		runCase(t, db, common.DatabaseTypeSQLite)
	})

	t.Run("postgres", func(t *testing.T) {
		dsn := os.Getenv("TEST_POSTGRES_DSN")
		if dsn == "" {
			t.Skip("TEST_POSTGRES_DSN is not configured")
		}
		db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{})
		require.NoError(t, err)
		sqlDB, err := db.DB()
		require.NoError(t, err)
		t.Cleanup(func() {
			_ = db.Migrator().DropTable(&NeedTokenBillingShortfall{}, &NeedTokenBillingRefund{}, &SubscriptionPreConsumeRecord{}, &UserSubscription{}, &SubscriptionPlan{}, &Token{}, &User{})
			_ = sqlDB.Close()
		})
		_ = db.Migrator().DropTable(&NeedTokenBillingShortfall{}, &NeedTokenBillingRefund{}, &SubscriptionPreConsumeRecord{}, &UserSubscription{}, &SubscriptionPlan{}, &Token{}, &User{})
		runCase(t, db, common.DatabaseTypePostgreSQL)
	})
}
