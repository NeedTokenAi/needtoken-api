package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"gorm.io/gorm"
	gormlogger "gorm.io/gorm/logger"
)

const (
	ExternalGrantSubscription = "subscription"
	ExternalGrantRedemption   = "redemption"
)

var (
	ErrExternalGrantInvalid      = errors.New("invalid external grant")
	ErrExternalGrantConflict     = errors.New("external grant payload conflict")
	ErrExternalGrantPlanRevision = errors.New("external grant plan revision mismatch")
	ErrExternalGrantQuotaUnit    = errors.New("external grant quota unit mismatch")
	ErrExternalGrantTokenLimit   = errors.New("external grant token limit reached")
)

// ExternalGrant is committed with the native resource. Pending rows are never
// committed: a missing operation can be retried, while a committed one is final.
// The hashed primary key gives operation IDs case-sensitive identity on every
// supported database, including MySQL installations with a CI collation.
type ExternalGrant struct {
	OperationKey         string `json:"-" gorm:"type:char(64);primaryKey"`
	OperationId          string `json:"operation_id" gorm:"type:varchar(128);not null"`
	Kind                 string `json:"kind" gorm:"type:varchar(16);not null"`
	State                string `json:"state" gorm:"type:varchar(16);not null"`
	PayloadHash          string `json:"payload_hash" gorm:"type:char(64);not null"`
	Payload              string `json:"-" gorm:"type:text;not null"`
	PlanSnapshot         string `json:"-" gorm:"type:text;not null"`
	CreatorUserId        int    `json:"-"`
	UserId               int    `json:"user_id"`
	PlanId               int    `json:"plan_id"`
	PlanRevision         string `json:"plan_revision" gorm:"type:char(64)"`
	Quota                int    `json:"quota" gorm:"type:bigint"`
	SubscriptionId       int    `json:"subscription_id"`
	RedemptionId         int    `json:"redemption_id"`
	ManagedTokenId       int    `json:"managed_token_id,omitempty"`
	CreatedAt            int64  `json:"created_at" gorm:"type:bigint"`
	ExpectedQuotaPerUnit int64  `json:"expected_quota_per_unit" gorm:"-"`
	IssueAPIKey          bool   `json:"issue_api_key,omitempty" gorm:"-"`
}

// ExternalGrantRequest deliberately excludes native resource IDs and keys.
// Subscription and redemption controllers each accept only their own fields.
type ExternalGrantRequest struct {
	OperationId          string `json:"operation_id"`
	Kind                 string `json:"kind"`
	UserId               int    `json:"user_id"`
	PlanId               int    `json:"plan_id"`
	PlanRevision         string `json:"plan_revision"`
	Name                 string `json:"name"`
	Quota                int    `json:"quota"`
	ExpiredTime          int64  `json:"expired_time"`
	ExpectedQuotaPerUnit int64  `json:"expected_quota_per_unit,omitempty"`
	IssueAPIKey          bool   `json:"issue_api_key,omitempty"`
}

// SubscriptionPlanRevision freezes the full native contract, including duration,
// reset rules, price and group behavior. Administrative timestamps are excluded.
func SubscriptionPlanRevision(plan SubscriptionPlan) (string, error) {
	plan.NormalizeDefaults()
	plan.CreatedAt, plan.UpdatedAt = 0, 0
	data, err := common.Marshal(plan)
	if err != nil {
		return "", err
	}
	return externalGrantDigest(data), nil
}

func externalGrantDigest(data []byte) string {
	digest := sha256.Sum256(data)
	return hex.EncodeToString(digest[:])
}

func validExternalGrantOperationID(id string) bool {
	if len(id) == 0 || len(id) > 128 {
		return false
	}
	for _, c := range id {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_:-", c) {
			continue
		}
		return false
	}
	return true
}

func GetExternalGrant(ctx context.Context, operationId, kind string) (*ExternalGrant, error) {
	if !validExternalGrantOperationID(operationId) {
		return nil, ErrExternalGrantInvalid
	}
	var grant ExternalGrant
	err := DB.WithContext(ctx).Where("operation_key = ? AND kind = ?", externalGrantDigest([]byte(operationId)), kind).First(&grant).Error
	if err != nil {
		return nil, err
	}
	var payload ExternalGrantRequest
	if err := common.UnmarshalJsonStr(grant.Payload, &payload); err != nil {
		return nil, err
	}
	grant.ExpectedQuotaPerUnit = payload.ExpectedQuotaPerUnit
	grant.IssueAPIKey = payload.IssueAPIKey
	return &grant, nil
}

// CreateExternalGrant grants an immutable external order exactly once. A failed
// transaction leaves neither an operation nor a native subscription/code. On a
// concurrent insert conflict, recovery happens only after the transaction ends
// (PostgreSQL aborts a transaction after a uniqueness error).
func CreateExternalGrant(ctx context.Context, creatorUserId int, request ExternalGrantRequest) (*ExternalGrant, error) {
	if creatorUserId <= 0 || !validExternalGrantOperationID(request.OperationId) {
		return nil, ErrExternalGrantInvalid
	}
	switch request.Kind {
	case ExternalGrantSubscription:
		decoded, err := hex.DecodeString(request.PlanRevision)
		if request.UserId <= 0 || request.PlanId <= 0 || err != nil || len(decoded) != sha256.Size || request.Name != "" || request.Quota != 0 || request.ExpiredTime != 0 || request.ExpectedQuotaPerUnit != 0 {
			return nil, ErrExternalGrantInvalid
		}
		request.PlanRevision = strings.ToLower(request.PlanRevision)
	case ExternalGrantRedemption:
		if request.UserId != 0 || request.PlanId != 0 || request.PlanRevision != "" || request.IssueAPIKey || utf8.RuneCountInString(request.Name) < 1 || utf8.RuneCountInString(request.Name) > 20 || request.Quota <= 0 || common.ValidateWalletQuota(request.Quota) != nil || request.ExpiredTime < 0 || request.ExpectedQuotaPerUnit <= 0 || request.ExpectedQuotaPerUnit > common.MaxWalletQuota {
			return nil, ErrExternalGrantInvalid
		}
	default:
		return nil, ErrExternalGrantInvalid
	}
	payload, err := common.Marshal(request)
	if err != nil {
		return nil, err
	}
	operation := ExternalGrant{
		OperationKey: externalGrantDigest([]byte(request.OperationId)), OperationId: request.OperationId,
		Kind: request.Kind, State: "pending", PayloadHash: externalGrantDigest(payload), Payload: string(payload),
		CreatorUserId: creatorUserId, UserId: request.UserId, PlanId: request.PlanId, PlanRevision: request.PlanRevision,
		Quota: request.Quota, CreatedAt: common.GetTimestamp(),
		ExpectedQuotaPerUnit: request.ExpectedQuotaPerUnit,
		IssueAPIKey:          request.IssueAPIKey,
	}
	var existing ExternalGrant
	err = DB.WithContext(ctx).Where("operation_key = ?", operation.OperationKey).First(&existing).Error
	if err == nil {
		if existing.PayloadHash != operation.PayloadHash {
			return nil, ErrExternalGrantConflict
		}
		existing.ExpectedQuotaPerUnit = request.ExpectedQuotaPerUnit
		existing.IssueAPIKey = request.IssueAPIKey
		return &existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}
	groupChanged := false
	// Read configuration before acquiring database locks. The token count below
	// uses this transaction and the locked recipient, never the global DB handle.
	maxTokens := operation_setting.GetMaxUserTokens()
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(&operation).Error; err != nil {
			return err
		}
		if request.Kind == ExternalGrantSubscription {
			// Bypass the plan cache and hold the plan row through creation so a
			// simultaneous edit cannot change the verified contract.
			var plan SubscriptionPlan
			if err := lockForUpdate(tx).First(&plan, request.PlanId).Error; err != nil {
				return err
			}
			plan.NormalizeDefaults()
			revision, err := SubscriptionPlanRevision(plan)
			if err != nil {
				return err
			}
			if revision != request.PlanRevision || !plan.Enabled {
				return ErrExternalGrantPlanRevision
			}
			if plan.TotalAmount < 0 || plan.TotalAmount > common.MaxWalletQuota {
				return ErrExternalGrantInvalid
			}
			var user User
			if err := lockForUpdate(tx).Select("id", "status").First(&user, request.UserId).Error; err != nil {
				return err
			}
			if user.Status != common.UserStatusEnabled {
				return ErrExternalGrantInvalid
			}
			subscription, err := CreateUserSubscriptionFromPlanTx(tx, request.UserId, &plan, "admin")
			if err != nil {
				return err
			}
			groupChanged = subscription.PrevUserGroup != ""
			operation.SubscriptionId = subscription.Id
			if request.IssueAPIKey {
				var count int64
				if err := tx.Model(&Token{}).Where("user_id = ?", request.UserId).Count(&count).Error; err != nil {
					return err
				}
				if count >= int64(maxTokens) {
					return ErrExternalGrantTokenLimit
				}
				key, err := common.GenerateKey()
				if err != nil {
					return err
				}
				// This removes only the additional per-token cap. Native wallet and
				// subscription limits, resets and billing preferences still govern
				// every request. An empty group follows the recipient's native group.
				token := Token{
					UserId: request.UserId, Key: key, Name: "API subscription",
					Status: common.TokenStatusEnabled, CreatedTime: common.GetTimestamp(), AccessedTime: common.GetTimestamp(),
					ExpiredTime: subscription.EndTime, UnlimitedQuota: true, AllowUserQuotaQuery: true,
				}
				// SQL tracing must never print the inserted credential, including
				// when a driver rejects the insert. The caller returns fixed errors.
				if err := tx.Session(&gorm.Session{Logger: gormlogger.Discard}).Create(&token).Error; err != nil {
					return err
				}
				operation.ManagedTokenId = token.Id
			}
			planJSON, err := common.Marshal(plan)
			if err != nil {
				return err
			}
			operation.PlanSnapshot = string(planJSON)
		} else {
			// Redemption quota is exact, but its advertised currency value also
			// depends on the native unit. Never silently reinterpret a frozen quote.
			if common.QuotaPerUnit != float64(request.ExpectedQuotaPerUnit) {
				return ErrExternalGrantQuotaUnit
			}
			if request.ExpiredTime != 0 && request.ExpiredTime <= common.GetTimestamp() {
				return ErrExternalGrantInvalid
			}
			redemption := Redemption{
				UserId: creatorUserId, Name: request.Name, Quota: request.Quota, Key: common.GetUUID(),
				Status: common.RedemptionCodeStatusEnabled, CreatedTime: common.GetTimestamp(), ExpiredTime: request.ExpiredTime,
			}
			if err := tx.Create(&redemption).Error; err != nil {
				return err
			}
			operation.RedemptionId = redemption.Id
		}
		operation.State = "succeeded"
		return tx.Save(&operation).Error
	})
	if err != nil {
		var recovered ExternalGrant
		if lookupErr := DB.WithContext(ctx).Where("operation_key = ?", operation.OperationKey).First(&recovered).Error; lookupErr == nil {
			if recovered.PayloadHash != operation.PayloadHash {
				return nil, ErrExternalGrantConflict
			}
			recovered.ExpectedQuotaPerUnit = request.ExpectedQuotaPerUnit
			recovered.IssueAPIKey = request.IssueAPIKey
			return &recovered, nil
		}
		return nil, err
	}
	if groupChanged {
		refreshSubscriptionUserGroupCache(request.UserId, "external subscription fulfillment")
	}
	return &operation, nil
}

type CustomerWalletSummary struct {
	Quota     int `json:"quota"`
	UsedQuota int `json:"used_quota"`
}

type CustomerSubscriptionSummary struct {
	Id                  int    `json:"id"`
	PlanId              int    `json:"plan_id"`
	AmountTotal         int64  `json:"amount_total"`
	AmountUsed          int64  `json:"amount_used"`
	StartTime           int64  `json:"start_time"`
	EndTime             int64  `json:"end_time"`
	Status              string `json:"status"`
	LastResetTime       int64  `json:"last_reset_time"`
	NextResetTime       int64  `json:"next_reset_time"`
	AllowWalletOverflow bool   `json:"allow_wallet_overflow"`
}

type CustomerTokenCapSummary struct {
	Id             int    `json:"id"`
	Name           string `json:"name"`
	Status         int    `json:"status"`
	RemainQuota    int    `json:"remain_quota"`
	UsedQuota      int    `json:"used_quota"`
	UnlimitedQuota bool   `json:"unlimited_quota"`
	ExpiredTime    int64  `json:"expired_time"`
}

type CustomerManagedAPIKeyCapacity struct {
	Limit     int   `json:"limit"`
	Used      int64 `json:"used"`
	Available bool  `json:"available"`
}

// CustomerQuotaSummary uses dedicated DTOs and SELECT lists. Wallet balances,
// subscription allowances and token caps remain three independent layers.
type CustomerQuotaSummary struct {
	Source                 string                        `json:"source"`
	CollectedAt            int64                         `json:"collected_at"`
	UserId                 int                           `json:"user_id"`
	Status                 int                           `json:"status"`
	Wallet                 CustomerWalletSummary         `json:"wallet"`
	Subscriptions          []CustomerSubscriptionSummary `json:"subscriptions"`
	TokenCaps              []CustomerTokenCapSummary     `json:"token_caps"`
	SubscriptionsTruncated bool                          `json:"subscriptions_truncated"`
	TokenCapsTruncated     bool                          `json:"token_caps_truncated"`
	ManagedAPIKeyCapacity  CustomerManagedAPIKeyCapacity `json:"managed_api_key_capacity"`
	// SubscriptionTimeZone describes this process's calendar calculation rules,
	// not a persisted historical transaction zone. Empty means unknown. A named
	// IANA location (including a reloadable alias) is reported only when its full
	// structure matches a same-name reload; Local and fixed-zone imitations are
	// not inferred from offsets, environment settings or sampled dates.
	SubscriptionTimeZone string `json:"subscription_time_zone"`
}

// CalculationTimeZone identifies the time.Local rules used by native month/year
// AddDate calculations. Structural matching is deliberately conservative: even
// an otherwise equivalent location with different cached state remains unknown.
func CalculationTimeZone() string {
	location := time.Local
	if location == nil {
		return ""
	}
	name := location.String()
	if name == "" || name == "Local" {
		return ""
	}
	reloaded, err := time.LoadLocation(name)
	if err != nil || reloaded.String() != name || !reflect.DeepEqual(location, reloaded) {
		return ""
	}
	return name
}

func GetCustomerQuotaSummary(ctx context.Context, userId int) (*CustomerQuotaSummary, error) {
	if userId <= 0 {
		return nil, ErrExternalGrantInvalid
	}
	result := CustomerQuotaSummary{Source: "new-api", UserId: userId, Subscriptions: []CustomerSubscriptionSummary{}, TokenCaps: []CustomerTokenCapSummary{}, SubscriptionTimeZone: CalculationTimeZone()}
	result.ManagedAPIKeyCapacity.Limit = operation_setting.GetMaxUserTokens()
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := tx.Select("id", "status", "quota", "used_quota").First(&user, userId).Error; err != nil {
			return err
		}
		result.Status = user.Status
		result.Wallet = CustomerWalletSummary{Quota: user.Quota, UsedQuota: user.UsedQuota}
		// Count all native keys, independently of the capped display list or
		// monetary caps. Expired/disabled keys still occupy native token slots.
		if err := tx.Model(&Token{}).Where("user_id = ?", userId).Count(&result.ManagedAPIKeyCapacity.Used).Error; err != nil {
			return err
		}
		result.ManagedAPIKeyCapacity.Available = result.ManagedAPIKeyCapacity.Used < int64(result.ManagedAPIKeyCapacity.Limit)
		if err := tx.Model(&UserSubscription{}).
			Select("id", "plan_id", "amount_total", "amount_used", "start_time", "end_time", "status", "last_reset_time", "next_reset_time", "allow_wallet_overflow").
			Where("user_id = ?", userId).Order("id desc").Limit(1001).Scan(&result.Subscriptions).Error; err != nil {
			return err
		}
		if err := tx.Model(&Token{}).
			Select("id", "name", "status", "remain_quota", "used_quota", "unlimited_quota", "expired_time").
			Where("user_id = ?", userId).Order("id desc").Limit(1001).Scan(&result.TokenCaps).Error; err != nil {
			return err
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(result.Subscriptions) > 1000 {
		result.SubscriptionsTruncated = true
		result.Subscriptions = result.Subscriptions[:1000]
	}
	if len(result.TokenCaps) > 1000 {
		result.TokenCapsTruncated = true
		result.TokenCaps = result.TokenCaps[:1000]
	}
	result.CollectedAt = common.GetTimestamp()
	return &result, nil
}
