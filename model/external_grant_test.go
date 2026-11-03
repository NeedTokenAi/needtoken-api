package model

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
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
	require.NoError(t, db.Model(&UserSubscription{}).Count(&count).Error)
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
	require.NoError(t, db.Model(&UserSubscription{}).Count(&count).Error)
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

	// Repeated startup preserves the committed association and uniqueness.
	for range 2 {
		require.NoError(t, db.AutoMigrate(&ExternalGrant{}))
	}
	again, err := GetExternalGrant(ctx, first.OperationId, first.Kind)
	require.NoError(t, err)
	assert.Equal(t, first.SubscriptionId, again.SubscriptionId)
	duplicate := *first
	assert.Error(t, db.Create(&duplicate).Error)
}
