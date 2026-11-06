package model

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

func TestExternalGrantDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			for _, upgrade := range []bool{false, true} {
				t.Run(fmt.Sprintf("upgrade=%v", upgrade), func(t *testing.T) {
					var driver gorm.Dialector
					switch dialect {
					case "sqlite":
						driver = sqlite.Open(":memory:")
					case "mysql":
						dsn := os.Getenv("TEST_MYSQL_DSN")
						if dsn == "" {
							t.Skip("TEST_MYSQL_DSN is not configured")
						}
						driver = mysql.Open(dsn)
					case "postgres":
						dsn := os.Getenv("TEST_POSTGRES_DSN")
						if dsn == "" {
							t.Skip("TEST_POSTGRES_DSN is not configured")
						}
						driver = postgres.Open(dsn)
					}
					db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "grant_test_"}})
					require.NoError(t, err)
					sqlDB, err := db.DB()
					require.NoError(t, err)
					sqlDB.SetMaxOpenConns(4)
					if dialect == "sqlite" {
						sqlDB.SetMaxOpenConns(1)
					}
					previousDB, previousType, previousRedis := DB, common.MainDatabaseType(), common.RedisEnabled
					DB = db
					common.SetMainDatabaseType(common.DatabaseType(dialect))
					common.RedisEnabled = false
					initCol()
					t.Cleanup(func() {
						require.NoError(t, db.Migrator().DropTable(&ExternalGrant{}, &Token{}, &Redemption{}, &UserSubscription{}, &SubscriptionPlan{}, &User{}))
						DB = previousDB
						common.SetMainDatabaseType(previousType)
						common.RedisEnabled = previousRedis
						initCol()
						assert.NoError(t, sqlDB.Close())
					})
					var version string
					versionSQL := "SELECT version()"
					if dialect == "sqlite" {
						versionSQL = "SELECT sqlite_version()"
					}
					require.NoError(t, db.Raw(versionSQL).Scan(&version).Error)
					t.Logf("database version: %s", version)
					require.False(t, db.Migrator().HasTable(&ExternalGrant{}), "use isolated test tables")
					nativeTables := []any{&User{}, &SubscriptionPlan{}, &UserSubscription{}, &Redemption{}, &Token{}}
					require.NoError(t, db.AutoMigrate(nativeTables...))
					legacy := User{Username: "preserved-user", Password: "never-expose-password", Status: common.UserStatusEnabled, Group: "default", Quota: 47, AffCode: "legacy-aff"}
					if upgrade {
						require.NoError(t, db.Create(&legacy).Error)
					}
					for range 2 {
						require.NoError(t, db.AutoMigrate(&ExternalGrant{}))
					}
					if upgrade {
						var preserved User
						require.NoError(t, db.First(&preserved, legacy.Id).Error)
						assert.Equal(t, 47, preserved.Quota)
						testExternalGrantLegacyUpgrade(t, db, legacy)
					}
					testExternalGrantContracts(t, db)
				})
			}
		})
	}
}

func testExternalGrantContracts(t *testing.T, db *gorm.DB) {
	t.Helper()
	ctx := context.Background()
	previousUnit := common.QuotaPerUnit
	common.QuotaPerUnit = 500000
	t.Cleanup(func() { common.QuotaPerUnit = previousUnit })
	user := User{Username: "grant-recipient", Password: "never-expose-password", Status: common.UserStatusEnabled, Group: "default", Quota: 123, UsedQuota: 7, AffCode: "recipient-aff"}
	require.NoError(t, db.Create(&user).Error)
	plan := SubscriptionPlan{Title: "frozen plan", Enabled: true, DurationUnit: SubscriptionDurationDay, DurationValue: 30, TotalAmount: 5000, Currency: "USD", PriceAmount: 19.99}
	require.NoError(t, db.Create(&plan).Error)
	revision, err := SubscriptionPlanRevision(plan)
	require.NoError(t, err)
	request := ExternalGrantRequest{OperationId: "subscription:order-1", Kind: ExternalGrantSubscription, UserId: user.Id, PlanId: plan.Id, PlanRevision: revision}
	first, err := CreateExternalGrant(ctx, user.Id, request)
	require.NoError(t, err)
	require.Positive(t, first.SubscriptionId)
	assert.Equal(t, "succeeded", first.State)

	// Discarding the first response models a committed grant whose HTTP response
	// is lost. Query/retry must return the native resource originally committed.
	recovered, err := GetExternalGrant(ctx, request.OperationId, request.Kind)
	require.NoError(t, err)
	assert.Equal(t, first.SubscriptionId, recovered.SubscriptionId)
	request.UserId++
	_, err = CreateExternalGrant(ctx, user.Id, request)
	assert.ErrorIs(t, err, ErrExternalGrantConflict)
	request.UserId--
	require.NoError(t, db.Model(&plan).Update("total_amount", 9999).Error)
	replay, err := CreateExternalGrant(ctx, user.Id, request)
	require.NoError(t, err)
	assert.Equal(t, first.SubscriptionId, replay.SubscriptionId)
	request.OperationId = "subscription:drift"
	_, err = CreateExternalGrant(ctx, user.Id, request)
	assert.ErrorIs(t, err, ErrExternalGrantPlanRevision)
	_, err = GetExternalGrant(ctx, request.OperationId, request.Kind)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var count int64
	require.NoError(t, db.Model(&UserSubscription{}).Where("user_id = ?", user.Id).Count(&count).Error)
	assert.EqualValues(t, 1, count)

	// Failure after native creation must roll back the grant, operation and any
	// group change; retrying the identical operation can then safely succeed.
	require.NoError(t, db.Model(&plan).Updates(map[string]any{"total_amount": 5000, "upgrade_group": "pro"}).Error)
	require.NoError(t, db.First(&plan, plan.Id).Error)
	request.PlanRevision, err = SubscriptionPlanRevision(plan)
	require.NoError(t, err)
	request.OperationId = "subscription:rollback"
	forced := errors.New("forced operation persistence failure")
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:grant_rollback", func(tx *gorm.DB) {
		if grant, ok := tx.Statement.Dest.(*ExternalGrant); ok && grant.OperationId == request.OperationId {
			tx.AddError(forced)
		}
	}))
	_, err = CreateExternalGrant(ctx, user.Id, request)
	assert.ErrorIs(t, err, forced)
	require.NoError(t, db.Callback().Update().Remove("test:grant_rollback"))
	_, err = GetExternalGrant(ctx, request.OperationId, request.Kind)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var storedUser User
	require.NoError(t, db.First(&storedUser, user.Id).Error)
	assert.Equal(t, "default", storedUser.Group)
	require.NoError(t, db.Model(&UserSubscription{}).Where("user_id = ?", user.Id).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	_, err = CreateExternalGrant(ctx, user.Id, request)
	require.NoError(t, err)
	require.NoError(t, db.First(&storedUser, user.Id).Error)
	assert.Equal(t, "pro", storedUser.Group)

	// Two workers concurrently processing one payment still mint one code.
	redemptionRequest := ExternalGrantRequest{OperationId: "redemption:order-2", Kind: ExternalGrantRedemption, Name: "external order", Quota: common.MaxWalletQuota, ExpectedQuotaPerUnit: 500000}
	mismatchedUnit := redemptionRequest
	mismatchedUnit.OperationId = "redemption:unit-drift"
	mismatchedUnit.ExpectedQuotaPerUnit = 1000000
	_, err = CreateExternalGrant(ctx, user.Id, mismatchedUnit)
	assert.ErrorIs(t, err, ErrExternalGrantQuotaUnit)
	_, err = GetExternalGrant(ctx, mismatchedUnit.OperationId, ExternalGrantRedemption)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var outcomes [2]*ExternalGrant
	var failures [2]error
	var workers sync.WaitGroup
	for i := range 2 {
		workers.Go(func() { outcomes[i], failures[i] = CreateExternalGrant(ctx, user.Id, redemptionRequest) })
	}
	workers.Wait()
	for i := range 2 {
		require.NoError(t, failures[i])
		require.NotNil(t, outcomes[i])
	}
	assert.Equal(t, outcomes[0].RedemptionId, outcomes[1].RedemptionId)
	common.QuotaPerUnit = 1000000
	unitReplay, err := CreateExternalGrant(ctx, user.Id, redemptionRequest)
	require.NoError(t, err)
	assert.Equal(t, outcomes[0].RedemptionId, unitReplay.RedemptionId)
	unitQuery, err := GetExternalGrant(ctx, redemptionRequest.OperationId, ExternalGrantRedemption)
	require.NoError(t, err)
	assert.EqualValues(t, 500000, unitQuery.ExpectedQuotaPerUnit)
	common.QuotaPerUnit = 500000
	require.NoError(t, db.Model(&Redemption{}).Count(&count).Error)
	assert.EqualValues(t, 1, count)
	code, err := GetRedemptionById(outcomes[0].RedemptionId)
	require.NoError(t, err)
	assert.Len(t, code.Key, 32)
	assert.Equal(t, common.MaxWalletQuota, code.Quota)
	redemptionRequest.Quota--
	_, err = CreateExternalGrant(ctx, user.Id, redemptionRequest)
	assert.ErrorIs(t, err, ErrExternalGrantConflict)
	for _, quota := range []int{0, -1, common.MaxWalletQuota + 1} {
		redemptionRequest.Quota = quota
		_, err = CreateExternalGrant(ctx, user.Id, redemptionRequest)
		assert.ErrorIs(t, err, ErrExternalGrantInvalid)
	}
	_, err = GetExternalGrant(ctx, redemptionRequest.OperationId, ExternalGrantSubscription)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "billing permission must not reveal redemption operations")
	encoded, err := common.Marshal(outcomes[0])
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), code.Key)
	assert.NotContains(t, string(encoded), "payload\"")

	token := Token{UserId: user.Id, Key: "never-expose-api-key", Name: "customer token", RemainQuota: 81, UsedQuota: 4, Status: common.TokenStatusEnabled, ExpiredTime: -1}
	require.NoError(t, db.Create(&token).Error)
	summary, err := GetCustomerQuotaSummary(ctx, user.Id)
	require.NoError(t, err)
	assert.Equal(t, 123, summary.Wallet.Quota, "code issuance does not mutate the wallet")
	require.Len(t, summary.Subscriptions, 2)
	assert.EqualValues(t, 5000, summary.Subscriptions[0].AmountTotal)
	require.Len(t, summary.TokenCaps, 1)
	assert.Equal(t, 81, summary.TokenCaps[0].RemainQuota)
	encoded, err = common.Marshal(summary)
	require.NoError(t, err)
	assert.NotContains(t, string(encoded), token.Key)
	assert.NotContains(t, string(encoded), user.Password)
	assert.NotContains(t, string(encoded), "email")
	assert.Positive(t, summary.CollectedAt)
	testCustomerSubscriptionTimeZone(t, user, token)

	// Repeated startup preserves the committed association and uniqueness.
	for range 2 {
		require.NoError(t, db.AutoMigrate(&ExternalGrant{}))
	}
	again, err := GetExternalGrant(ctx, first.OperationId, first.Kind)
	require.NoError(t, err)
	assert.Equal(t, first.SubscriptionId, again.SubscriptionId)
	duplicate := *first
	assert.Error(t, db.Create(&duplicate).Error)
	testManagedSubscriptionGrant(t, db, user, plan)
}

func testCustomerSubscriptionTimeZone(t *testing.T, user User, token Token) {
	t.Helper()
	newYork, err := time.LoadLocation("America/New_York")
	require.NoError(t, err)
	shanghai, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	utcReloaded, err := time.LoadLocation("UTC")
	require.NoError(t, err)
	alias, err := time.LoadLocation("US/Eastern")
	require.NoError(t, err)
	previous := time.Local
	t.Cleanup(func() { time.Local = previous })
	monthStart := time.Date(2026, time.February, 8, 17, 0, 0, 0, time.UTC).Unix()
	yearStart := time.Date(2025, time.March, 8, 17, 0, 0, 0, time.UTC).Unix()
	for _, test := range []struct {
		name     string
		location *time.Location
		want     string
		monthEnd time.Time
		yearEnd  time.Time
	}{
		{"UTC", time.UTC, "UTC", time.Date(2026, time.March, 8, 17, 0, 0, 0, time.UTC), time.Date(2026, time.March, 8, 17, 0, 0, 0, time.UTC)},
		{"reloaded_UTC", utcReloaded, "UTC", time.Date(2026, time.March, 8, 17, 0, 0, 0, time.UTC), time.Date(2026, time.March, 8, 17, 0, 0, 0, time.UTC)},
		{"New_York_DST", newYork, "America/New_York", time.Date(2026, time.March, 8, 16, 0, 0, 0, time.UTC), time.Date(2026, time.March, 8, 16, 0, 0, 0, time.UTC)},
		{"Shanghai", shanghai, "Asia/Shanghai", time.Date(2026, time.March, 8, 17, 0, 0, 0, time.UTC), time.Date(2026, time.March, 8, 17, 0, 0, 0, time.UTC)},
		{"reloadable_IANA_alias", alias, "US/Eastern", time.Date(2026, time.March, 8, 16, 0, 0, 0, time.UTC), time.Date(2026, time.March, 8, 16, 0, 0, 0, time.UTC)},
		{"unknown_Local", time.FixedZone("Local", 0), "", time.Time{}, time.Time{}},
		{"unknown_fixed_zone", time.FixedZone("fixed-offset", 8*3600), "", time.Time{}, time.Time{}},
		{"forged_IANA_name", time.FixedZone("America/New_York", -5*3600), "", time.Time{}, time.Time{}},
		{"forged_UTC_name", time.FixedZone("UTC", 3600), "", time.Time{}, time.Time{}},
	} {
		t.Run("subscription_time_zone/"+test.name, func(t *testing.T) {
			time.Local = test.location
			t.Cleanup(func() { time.Local = previous })
			summary, err := GetCustomerQuotaSummary(context.Background(), user.Id)
			require.NoError(t, err)
			assert.Equal(t, test.want, summary.SubscriptionTimeZone)
			encoded, err := common.Marshal(summary)
			require.NoError(t, err)
			var data map[string]any
			require.NoError(t, common.Unmarshal(encoded, &data))
			assert.Equal(t, test.want, data["subscription_time_zone"], "unknown must remain an explicit empty field")
			assert.NotContains(t, string(encoded), token.Key)
			assert.NotContains(t, string(encoded), user.Password)
			assert.NotContains(t, string(encoded), "email")
			if test.want == "" {
				return
			}
			location, err := time.LoadLocation(summary.SubscriptionTimeZone)
			require.NoError(t, err)
			for _, duration := range []struct {
				unit  string
				start int64
				end   time.Time
			}{
				{SubscriptionDurationMonth, monthStart, test.monthEnd},
				{SubscriptionDurationYear, yearStart, test.yearEnd},
			} {
				plan := SubscriptionPlan{DurationUnit: duration.unit, DurationValue: 1}
				nativeEnd, err := calcPlanEndTime(time.Unix(duration.start, 0), &plan)
				require.NoError(t, err)
				assert.Equal(t, duration.end.Unix(), nativeEnd, "native %s duration", duration.unit)
				verifiedEnd, err := calcPlanEndTime(time.Unix(duration.start, 0).In(location), &plan)
				require.NoError(t, err)
				assert.Equal(t, nativeEnd, verifiedEnd, "reported rules must reproduce native calendar arithmetic")
			}
		})
	}
}

// Simulate the released receipt schema before managed_token_id existed. The
// existing native subscription and immutable payload must survive two startups.
func testExternalGrantLegacyUpgrade(t *testing.T, db *gorm.DB, user User) {
	t.Helper()
	plan := SubscriptionPlan{Title: "legacy grant plan", Enabled: true, DurationUnit: SubscriptionDurationDay, DurationValue: 7, TotalAmount: 70}
	require.NoError(t, db.Create(&plan).Error)
	revision, err := SubscriptionPlanRevision(plan)
	require.NoError(t, err)
	request := ExternalGrantRequest{OperationId: "subscription:legacy-receipt", Kind: ExternalGrantSubscription, UserId: user.Id, PlanId: plan.Id, PlanRevision: revision}
	// A literal old payload ensures adding the optional flag does not change the
	// bytes (and therefore the operation hash) accepted by previous releases.
	oldPayload := fmt.Sprintf(`{"operation_id":"subscription:legacy-receipt","kind":"subscription","user_id":%d,"plan_id":%d,"plan_revision":"%s","name":"","quota":0,"expired_time":0}`, user.Id, plan.Id, revision)
	payload, err := common.Marshal(request)
	require.NoError(t, err)
	assert.Equal(t, oldPayload, string(payload))
	planJSON, err := common.Marshal(plan)
	require.NoError(t, err)
	require.NoError(t, db.Migrator().DropColumn(&ExternalGrant{}, "ManagedTokenId"))
	assert.False(t, db.Migrator().HasColumn(&ExternalGrant{}, "ManagedTokenId"))
	legacy := ExternalGrant{
		OperationKey: externalGrantDigest([]byte(request.OperationId)), OperationId: request.OperationId, Kind: request.Kind, State: "succeeded",
		PayloadHash: externalGrantDigest(payload), Payload: string(payload), PlanSnapshot: string(planJSON), CreatorUserId: user.Id, UserId: user.Id, PlanId: plan.Id, PlanRevision: revision,
		CreatedAt: common.GetTimestamp(),
	}
	// Seed the old release without issuing a SELECT * using the new schema:
	// PostgreSQL cached statements correctly reject a changed result column list.
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		subscription, err := CreateUserSubscriptionFromPlanTx(tx, user.Id, &plan, "admin")
		if err != nil {
			return err
		}
		legacy.SubscriptionId = subscription.Id
		return tx.Omit("ManagedTokenId").Create(&legacy).Error
	}))
	for range 2 {
		require.NoError(t, db.AutoMigrate(&ExternalGrant{}))
	}
	assert.True(t, db.Migrator().HasColumn(&ExternalGrant{}, "ManagedTokenId"))
	recovered, err := GetExternalGrant(context.Background(), request.OperationId, request.Kind)
	require.NoError(t, err)
	assert.Equal(t, legacy.SubscriptionId, recovered.SubscriptionId)
	assert.Equal(t, legacy.PayloadHash, recovered.PayloadHash)
	assert.Equal(t, legacy.PlanSnapshot, recovered.PlanSnapshot)
	assert.Zero(t, recovered.ManagedTokenId)
	assert.False(t, recovered.IssueAPIKey)
	replay, err := CreateExternalGrant(context.Background(), user.Id, request)
	require.NoError(t, err)
	assert.Equal(t, recovered.SubscriptionId, replay.SubscriptionId)
	var subscription UserSubscription
	require.NoError(t, db.First(&subscription, recovered.SubscriptionId).Error)
	assert.Equal(t, user.Id, subscription.UserId)
	var preserved User
	require.NoError(t, db.First(&preserved, user.Id).Error)
	assert.Equal(t, 47, preserved.Quota)
}

func testManagedSubscriptionGrant(t *testing.T, db *gorm.DB, user User, plan SubscriptionPlan) {
	t.Helper()
	ctx := context.Background()
	previousLimit := operation_setting.GetMaxUserTokens()
	operation_setting.GetTokenSetting().MaxUserTokens = 1000
	t.Cleanup(func() { operation_setting.GetTokenSetting().MaxUserTokens = previousLimit })
	require.NoError(t, db.Model(&plan).Updates(map[string]any{
		"duration_unit": SubscriptionDurationYear, "duration_value": 1, "quota_reset_period": SubscriptionResetMonthly,
	}).Error)
	require.NoError(t, db.First(&plan, plan.Id).Error)
	revision, err := SubscriptionPlanRevision(plan)
	require.NoError(t, err)
	request := ExternalGrantRequest{OperationId: "subscription:managed", Kind: ExternalGrantSubscription, UserId: user.Id, PlanId: plan.Id, PlanRevision: revision, IssueAPIKey: true}

	// Two payment workers still commit exactly one subscription and owned key.
	var outcomes [2]*ExternalGrant
	var failures [2]error
	var workers sync.WaitGroup
	for i := range 2 {
		workers.Go(func() { outcomes[i], failures[i] = CreateExternalGrant(ctx, user.Id, request) })
	}
	workers.Wait()
	for i := range 2 {
		require.NoError(t, failures[i])
		require.NotNil(t, outcomes[i])
		assert.True(t, outcomes[i].IssueAPIKey)
		require.Positive(t, outcomes[i].ManagedTokenId)
	}
	first := outcomes[0]
	assert.Equal(t, first.ManagedTokenId, outcomes[1].ManagedTokenId)
	assert.Equal(t, first.SubscriptionId, outcomes[1].SubscriptionId)
	var token Token
	require.NoError(t, db.First(&token, first.ManagedTokenId).Error)
	assert.Equal(t, user.Id, token.UserId)
	assert.Len(t, token.Key, 48)
	owned, err := GetTokenByIds(token.Id, user.Id)
	require.NoError(t, err)
	assert.Equal(t, token.Key, owned.Key, "the existing owner console can reveal this native key")
	_, err = GetTokenByIds(token.Id, user.Id+1)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound, "another account cannot reveal the managed key")
	assert.Equal(t, common.TokenStatusEnabled, token.Status)
	assert.Empty(t, token.Group, "follow the recipient's native group")
	assert.False(t, token.CrossGroupRetry)
	assert.False(t, token.ModelLimitsEnabled)
	assert.True(t, token.UnlimitedQuota, "no additional token cap; native billing remains authoritative")
	assert.Zero(t, token.RemainQuota)
	var subscription UserSubscription
	require.NoError(t, db.First(&subscription, first.SubscriptionId).Error)
	assert.Equal(t, subscription.EndTime, token.ExpiredTime)
	assert.Equal(t, plan.TotalAmount, subscription.AmountTotal)
	assert.Zero(t, subscription.AmountUsed)
	assert.Greater(t, subscription.NextResetTime, subscription.StartTime)
	assert.Less(t, subscription.NextResetTime, subscription.EndTime)
	// Annual key validity must not prevent monthly native allowance resets. The
	// key carries no mirrored monthly budget that could become exhausted forever.
	require.NoError(t, db.Model(&subscription).Update("amount_used", subscription.AmountTotal-1).Error)
	require.NoError(t, db.First(&subscription, subscription.Id).Error)
	resetAt := subscription.NextResetTime
	require.NoError(t, db.Transaction(func(tx *gorm.DB) error {
		return maybeResetUserSubscriptionWithPlanTx(tx, &subscription, &plan, resetAt)
	}))
	require.NoError(t, db.First(&subscription, subscription.Id).Error)
	assert.Zero(t, subscription.AmountUsed)
	assert.Equal(t, plan.TotalAmount, subscription.AmountTotal)
	assert.Greater(t, subscription.NextResetTime, resetAt)
	require.NoError(t, db.First(&token, token.Id).Error)
	assert.Equal(t, subscription.EndTime, token.ExpiredTime)
	assert.True(t, token.UnlimitedQuota)
	var storedUser User
	require.NoError(t, db.First(&storedUser, user.Id).Error)
	assert.Equal(t, 123, storedUser.Quota)
	assert.Equal(t, 7, storedUser.UsedQuota)
	assert.Equal(t, "pro", storedUser.Group)
	assert.Equal(t, user.Setting, storedUser.Setting, "preserve native billing preference")

	// Recover a lost response, including after repeated startup migrations.
	for range 2 {
		require.NoError(t, db.AutoMigrate(&ExternalGrant{}))
	}
	recovered, err := GetExternalGrant(ctx, request.OperationId, request.Kind)
	require.NoError(t, err)
	assert.True(t, recovered.IssueAPIKey)
	assert.Equal(t, first.ManagedTokenId, recovered.ManagedTokenId)
	assert.Equal(t, first.SubscriptionId, recovered.SubscriptionId)
	for _, grant := range []*ExternalGrant{first, recovered} {
		encoded, err := common.Marshal(grant)
		require.NoError(t, err)
		assert.NotContains(t, string(encoded), token.Key)
		assert.NotContains(t, string(encoded), `"key"`)
		assert.NotContains(t, string(encoded), `"payload"`)
	}
	request.IssueAPIKey = false
	_, err = CreateExternalGrant(ctx, user.Id, request)
	assert.ErrorIs(t, err, ErrExternalGrantConflict)
	request.IssueAPIKey = true

	var tokenCount, subscriptionCount int64
	require.NoError(t, db.Model(&Token{}).Where("user_id = ?", user.Id).Count(&tokenCount).Error)
	require.NoError(t, db.Model(&UserSubscription{}).Where("user_id = ?", user.Id).Count(&subscriptionCount).Error)
	assert.EqualValues(t, 2, tokenCount)
	assert.EqualValues(t, 3, subscriptionCount)

	// Failure after inserting the key rolls back all three native records.
	request.OperationId = "subscription:managed-rollback"
	forced := errors.New("forced managed receipt failure")
	require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:managed_rollback", func(tx *gorm.DB) {
		if grant, ok := tx.Statement.Dest.(*ExternalGrant); ok && grant.OperationId == request.OperationId {
			tx.AddError(forced)
		}
	}))
	_, err = CreateExternalGrant(ctx, user.Id, request)
	assert.ErrorIs(t, err, forced)
	require.NoError(t, db.Callback().Update().Remove("test:managed_rollback"))
	_, err = GetExternalGrant(ctx, request.OperationId, request.Kind)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	var after int64
	require.NoError(t, db.Model(&Token{}).Where("user_id = ?", user.Id).Count(&after).Error)
	assert.Equal(t, tokenCount, after)
	require.NoError(t, db.Model(&UserSubscription{}).Where("user_id = ?", user.Id).Count(&after).Error)
	assert.Equal(t, subscriptionCount, after)
	request.OperationId = "subscription:key-insert-rollback"
	var sqlLog bytes.Buffer
	previousLogger := db.Logger
	db.Logger = gormlogger.New(log.New(&sqlLog, "", 0), gormlogger.Config{LogLevel: gormlogger.Info})
	var generatedKey string
	insertFailure := errors.New("forced key persistence failure")
	require.NoError(t, db.Callback().Create().After("gorm:create").Register("test:managed_key_rollback", func(tx *gorm.DB) {
		if token, ok := tx.Statement.Dest.(*Token); ok {
			generatedKey = token.Key
			tx.AddError(insertFailure)
		}
	}))
	_, err = CreateExternalGrant(ctx, user.Id, request)
	db.Logger = previousLogger
	require.NoError(t, db.Callback().Create().Remove("test:managed_key_rollback"))
	assert.ErrorIs(t, err, insertFailure)
	require.Len(t, generatedKey, 48)
	assert.NotContains(t, sqlLog.String(), generatedKey, "SQL trace failures must not disclose API keys")
	_, err = GetExternalGrant(ctx, request.OperationId, request.Kind)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.NoError(t, db.Model(&Token{}).Where("user_id = ?", user.Id).Count(&after).Error)
	assert.Equal(t, tokenCount, after)
	require.NoError(t, db.Model(&UserSubscription{}).Where("user_id = ?", user.Id).Count(&after).Error)
	assert.Equal(t, subscriptionCount, after)
	retried, err := CreateExternalGrant(ctx, user.Id, request)
	require.NoError(t, err)
	assert.NotEqual(t, first.ManagedTokenId, retried.ManagedTokenId, "each distinct grant issues its own key")

	// Key limits and disabled recipients must fail without accepting payment a
	// second time or leaving an untracked native subscription behind.
	operation_setting.GetTokenSetting().MaxUserTokens = int(tokenCount + 1)
	request.OperationId = "subscription:managed-limit"
	_, err = CreateExternalGrant(ctx, user.Id, request)
	assert.ErrorIs(t, err, ErrExternalGrantTokenLimit)
	_, err = GetExternalGrant(ctx, request.OperationId, request.Kind)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.NoError(t, db.Model(&Token{}).Where("user_id = ?", user.Id).Count(&after).Error)
	assert.Equal(t, tokenCount+1, after)
	require.NoError(t, db.Model(&UserSubscription{}).Where("user_id = ?", user.Id).Count(&after).Error)
	assert.Equal(t, subscriptionCount+1, after)
	summary, err := GetCustomerQuotaSummary(ctx, user.Id)
	require.NoError(t, err)
	assert.Equal(t, int(tokenCount+1), summary.ManagedAPIKeyCapacity.Limit)
	assert.Equal(t, tokenCount+1, summary.ManagedAPIKeyCapacity.Used)
	assert.False(t, summary.ManagedAPIKeyCapacity.Available)
	operation_setting.GetTokenSetting().MaxUserTokens++
	summary, err = GetCustomerQuotaSummary(ctx, user.Id)
	require.NoError(t, err)
	assert.True(t, summary.ManagedAPIKeyCapacity.Available)
	operation_setting.GetTokenSetting().MaxUserTokens--
	// A replay is accepted even when the user subsequently reaches the limit.
	request.OperationId = first.OperationId
	replay, err := CreateExternalGrant(ctx, user.Id, request)
	require.NoError(t, err)
	assert.Equal(t, first.ManagedTokenId, replay.ManagedTokenId)
	request.OperationId = "subscription:disabled-recipient"
	require.NoError(t, db.Model(&storedUser).Update("status", common.UserStatusDisabled).Error)
	_, err = CreateExternalGrant(ctx, user.Id, request)
	assert.ErrorIs(t, err, ErrExternalGrantInvalid)
	_, err = GetExternalGrant(ctx, request.OperationId, request.Kind)
	assert.ErrorIs(t, err, gorm.ErrRecordNotFound)
	require.NoError(t, db.Model(&storedUser).Update("status", common.UserStatusEnabled).Error)
	redemption := ExternalGrantRequest{OperationId: "redemption:no-managed-key", Kind: ExternalGrantRedemption, Name: "external order", Quota: 1, ExpectedQuotaPerUnit: 500000, IssueAPIKey: true}
	_, err = CreateExternalGrant(ctx, user.Id, redemption)
	assert.ErrorIs(t, err, ErrExternalGrantInvalid)
}
