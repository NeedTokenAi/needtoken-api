package model

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/logger"
	"gorm.io/gorm"
)

const (
	ExternalGrantWalletCredit = "wallet_credit"
	ExternalGrantWalletDebit  = "wallet_debit"
)

var (
	ErrWalletInsufficientBalance = errors.New("insufficient unified wallet balance")
	ErrExternalGrantRolledBack   = errors.New("external wallet debit operation was already rolled back")
)

func hasRolledBackDebitGrant(ctx context.Context, operationId string) (bool, error) {
	rollbackOpID := operationId + ":rollback"
	if !validExternalGrantOperationID(rollbackOpID) {
		return false, nil
	}
	var count int64
	err := DB.WithContext(ctx).
		Model(&ExternalGrant{}).
		Where("operation_key = ? AND kind = ?", externalGrantDigest([]byte(rollbackOpID)), ExternalGrantWalletCredit).
		Count(&count).Error
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

type ExternalWalletMutationResult struct {
	OperationId  string `json:"operation_id"`
	AppliedQuota int    `json:"applied_quota"`
	BalanceQuota int    `json:"balance_quota"`
	Replayed     bool   `json:"replayed"`
}

type externalWalletPayload struct {
	OperationId  string `json:"operation_id"`
	Kind         string `json:"kind"`
	UserId       int    `json:"user_id"`
	Quota        int    `json:"quota"`
	AllowPartial bool   `json:"allow_partial,omitempty"`
	Remark       string `json:"remark,omitempty"`
}

// ApplyExternalWalletCredit atomically and idempotently credits a user's native
// User.Quota using the existing ExternalGrant table as the idempotency ledger.
func ApplyExternalWalletCredit(ctx context.Context, userId, quota int, operationId, remark string) (*ExternalWalletMutationResult, error) {
	if userId <= 0 || quota <= 0 || !validExternalGrantOperationID(operationId) {
		return nil, ErrExternalGrantInvalid
	}
	if err := common.ValidateWalletQuota(quota); err != nil {
		return nil, err
	}
	payloadObj := externalWalletPayload{
		OperationId: operationId,
		Kind:        ExternalGrantWalletCredit,
		UserId:      userId,
		Quota:       quota,
		Remark:      strings.TrimSpace(remark),
	}
	payloadBytes, err := common.Marshal(payloadObj)
	if err != nil {
		return nil, err
	}
	opKey := externalGrantDigest([]byte(operationId))
	payloadHash := externalGrantDigest(payloadBytes)

	var existing ExternalGrant
	if err := DB.WithContext(ctx).Where("operation_key = ?", opKey).First(&existing).Error; err == nil {
		if existing.Kind != ExternalGrantWalletCredit || existing.PayloadHash != payloadHash {
			return nil, ErrExternalGrantConflict
		}
		balance, balErr := GetUserQuota(userId, false)
		if balErr != nil {
			balance = 0
		}
		return &ExternalWalletMutationResult{
			OperationId:  operationId,
			AppliedQuota: existing.Quota,
			BalanceQuota: balance,
			Replayed:     true,
		}, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	var balanceAfter int
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		grant := ExternalGrant{
			OperationKey: opKey,
			OperationId:  operationId,
			Kind:         ExternalGrantWalletCredit,
			State:        "succeeded",
			PayloadHash:  payloadHash,
			Payload:      string(payloadBytes),
			UserId:       userId,
			Quota:        quota,
			CreatedAt:    common.GetTimestamp(),
		}
		if err := tx.Create(&grant).Error; err != nil {
			return err
		}
		if err := creditTopUpQuota(tx, userId, quota, nil); err != nil {
			return err
		}
		var user User
		if err := tx.Select("quota").First(&user, userId).Error; err != nil {
			return err
		}
		balanceAfter = user.Quota
		return nil
	})
	if err != nil {
		var recovered ExternalGrant
		if lookupErr := DB.WithContext(ctx).Where("operation_key = ?", opKey).First(&recovered).Error; lookupErr == nil {
			if recovered.Kind != ExternalGrantWalletCredit || recovered.PayloadHash != payloadHash {
				return nil, ErrExternalGrantConflict
			}
			balance, _ := GetUserQuota(userId, false)
			return &ExternalWalletMutationResult{
				OperationId:  operationId,
				AppliedQuota: recovered.Quota,
				BalanceQuota: balance,
				Replayed:     true,
			}, nil
		}
		return nil, err
	}

	syncCreditUserQuotaCache(userId, quota, common.LogText("unified_wallet_credit"))
	msgRemark := strings.TrimSpace(remark)
	if msgRemark == "" {
		msgRemark = operationId
	}
	RecordLog(userId, LogTypeTopup, common.NewMessage("Unified wallet credited {{quota}} ({{remark}})", map[string]any{
		"quota":  logger.FormatQuota(quota),
		"remark": msgRemark,
	}))
	return &ExternalWalletMutationResult{
		OperationId:  operationId,
		AppliedQuota: quota,
		BalanceQuota: balanceAfter,
		Replayed:     false,
	}, nil
}

// ApplyExternalWalletDebit atomically and idempotently debits a user's native
// User.Quota for storefront checkout or external cash purchases.
func ApplyExternalWalletDebit(ctx context.Context, userId, maxQuota int, allowPartial bool, operationId, remark string) (*ExternalWalletMutationResult, error) {
	if userId <= 0 || maxQuota <= 0 || !validExternalGrantOperationID(operationId) {
		return nil, ErrExternalGrantInvalid
	}
	if err := common.ValidateWalletQuota(maxQuota); err != nil {
		return nil, err
	}
	payloadObj := externalWalletPayload{
		OperationId:  operationId,
		Kind:         ExternalGrantWalletDebit,
		UserId:       userId,
		Quota:        maxQuota,
		AllowPartial: allowPartial,
		Remark:       strings.TrimSpace(remark),
	}
	payloadBytes, err := common.Marshal(payloadObj)
	if err != nil {
		return nil, err
	}
	opKey := externalGrantDigest([]byte(operationId))
	payloadHash := externalGrantDigest(payloadBytes)

	var existing ExternalGrant
	if err := DB.WithContext(ctx).Where("operation_key = ?", opKey).First(&existing).Error; err == nil {
		if existing.Kind != ExternalGrantWalletDebit || existing.PayloadHash != payloadHash {
			return nil, ErrExternalGrantConflict
		}
		rolledBack, rbErr := hasRolledBackDebitGrant(ctx, operationId)
		if rbErr != nil {
			return nil, rbErr
		}
		if rolledBack {
			return nil, ErrExternalGrantRolledBack
		}
		balance, balErr := GetUserQuota(userId, false)
		if balErr != nil {
			balance = 0
		}
		return &ExternalWalletMutationResult{
			OperationId:  operationId,
			AppliedQuota: existing.Quota,
			BalanceQuota: balance,
			Replayed:     true,
		}, nil
	} else if !errors.Is(err, gorm.ErrRecordNotFound) {
		return nil, err
	}

	var applied int
	var balanceAfter int
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var user User
		if err := lockForUpdate(tx).Select("id", "quota", "status").First(&user, userId).Error; err != nil {
			return err
		}
		if user.Status != common.UserStatusEnabled {
			return ErrExternalGrantInvalid
		}
		available := max(user.Quota, 0)
		deduct := min(available, maxQuota)
		if !allowPartial && deduct < maxQuota {
			return ErrWalletInsufficientBalance
		}
		if deduct == 0 {
			applied = 0
			balanceAfter = user.Quota
			return nil
		}

		grant := ExternalGrant{
			OperationKey: opKey,
			OperationId:  operationId,
			Kind:         ExternalGrantWalletDebit,
			State:        "succeeded",
			PayloadHash:  payloadHash,
			Payload:      string(payloadBytes),
			UserId:       userId,
			Quota:        deduct,
			CreatedAt:    common.GetTimestamp(),
		}
		if err := tx.Create(&grant).Error; err != nil {
			return err
		}
		res := tx.Model(&User{}).
			Where("id = ? AND quota >= ?", userId, deduct).
			Update("quota", gorm.Expr("quota - ?", deduct))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return ErrWalletInsufficientBalance
		}
		applied = deduct
		balanceAfter = user.Quota - deduct
		return nil
	})
	if err != nil {
		if !errors.Is(err, ErrWalletInsufficientBalance) {
			var recovered ExternalGrant
			if lookupErr := DB.WithContext(ctx).Where("operation_key = ?", opKey).First(&recovered).Error; lookupErr == nil {
				if recovered.Kind != ExternalGrantWalletDebit || recovered.PayloadHash != payloadHash {
					return nil, ErrExternalGrantConflict
				}
				rolledBack, rbErr := hasRolledBackDebitGrant(ctx, operationId)
				if rbErr != nil {
					return nil, rbErr
				}
				if rolledBack {
					return nil, ErrExternalGrantRolledBack
				}
				balance, _ := GetUserQuota(userId, false)
				return &ExternalWalletMutationResult{
					OperationId:  operationId,
					AppliedQuota: recovered.Quota,
					BalanceQuota: balance,
					Replayed:     true,
				}, nil
			}
		}
		return nil, err
	}

	if applied > 0 {
		if common.RedisEnabled {
			if status, cacheErr := cacheApplyUserQuotaDelta(userId, -int64(applied)); cacheErr != nil || status != cacheQuotaOK {
				_ = invalidateUserCache(userId)
			}
		}
		msgRemark := strings.TrimSpace(remark)
		if msgRemark == "" {
			msgRemark = operationId
		}
		RecordLog(userId, LogTypeSystem, common.NewMessage("Unified wallet debited {{quota}} ({{remark}})", map[string]any{
			"quota":  logger.FormatQuota(applied),
			"remark": msgRemark,
		}))
	}

	return &ExternalWalletMutationResult{
		OperationId:  operationId,
		AppliedQuota: applied,
		BalanceQuota: balanceAfter,
		Replayed:     false,
	}, nil
}

// ExternalWalletOpStatus describes the authoritative gateway state of an external wallet operation.
type ExternalWalletOpStatus struct {
	OperationId  string `json:"operation_id"`
	Kind         string `json:"kind,omitempty"`
	Found        bool   `json:"found"`
	Status       string `json:"status"`
	AppliedQuota int    `json:"applied_quota"`
	BalanceQuota int    `json:"balance_quota"`
}

// LookupExternalWalletOp queries the committed gateway ledger for an external wallet operation
// and reads the authoritative User.Quota directly from the database.
func LookupExternalWalletOp(ctx context.Context, userId int, operationId, kind string) (*ExternalWalletOpStatus, error) {
	if userId <= 0 || !validExternalGrantOperationID(operationId) {
		return nil, ErrExternalGrantInvalid
	}
	kind = strings.TrimSpace(kind)
	if kind != "" && kind != ExternalGrantWalletDebit && kind != ExternalGrantWalletCredit {
		return nil, ErrExternalGrantInvalid
	}

	var user User
	if err := DB.WithContext(ctx).Select("id", "quota").First(&user, userId).Error; err != nil {
		return nil, err
	}

	var existing ExternalGrant
	query := DB.WithContext(ctx).Where("operation_key = ?", externalGrantDigest([]byte(operationId)))
	if kind != "" {
		query = query.Where("kind = ?", kind)
	}
	err := query.First(&existing).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &ExternalWalletOpStatus{
			OperationId:  operationId,
			Kind:         kind,
			Found:        false,
			Status:       "not_found",
			AppliedQuota: 0,
			BalanceQuota: user.Quota,
		}, nil
	}
	if err != nil {
		return nil, err
	}
	if existing.UserId != userId {
		return nil, ErrExternalGrantConflict
	}

	status := ""
	switch existing.Kind {
	case ExternalGrantWalletDebit:
		rolledBack, rbErr := hasRolledBackDebitGrant(ctx, operationId)
		if rbErr != nil {
			return nil, rbErr
		}
		if rolledBack {
			status = "rolled_back"
		} else {
			status = "debited"
		}
	case ExternalGrantWalletCredit:
		status = "credited"
	default:
		return nil, ErrExternalGrantConflict
	}

	return &ExternalWalletOpStatus{
		OperationId:  operationId,
		Kind:         existing.Kind,
		Found:        true,
		Status:       status,
		AppliedQuota: existing.Quota,
		BalanceQuota: user.Quota,
	}, nil
}

// ErrSubscriptionWalletOverflowDisabled indicates positive subscription delta exceeded
// all eligible active subscriptions and wallet spillover was disallowed by either
// the user's subscription_only preference or a subscription's allow_wallet_overflow=false.
var ErrSubscriptionWalletOverflowDisabled = errors.New("subscription wallet_overflow_disabled")

// ErrSubscriptionHybridQuotaInsufficient indicates that even combining remaining
// eligible subscription quota and wallet balance cannot satisfy the requested pre-consume.
var ErrSubscriptionHybridQuotaInsufficient = errors.New("subscription and wallet quota insufficient")

// PreConsumeSubscriptionWithWalletSpillover atomically pre-consumes from eligible
// active subscriptions and, when allowed by user preference and subscription settings,
// absorbs any remaining shortfall from the user's wallet within the same DB transaction.
// When combined funds are still insufficient, the entire transaction rolls back cleanly.
func PreConsumeSubscriptionWithWalletSpillover(
	requestId string,
	userId int,
	modelName string,
	usingGroup string,
	amount int64,
	billingPreference string,
) (*SubscriptionPreConsumeResult, error) {
	if userId <= 0 {
		return nil, errors.New("invalid userId")
	}
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return nil, errors.New("requestId is empty")
	}
	if amount <= 0 {
		return nil, errors.New("amount must be > 0")
	}
	now := GetDBTimestamp()
	splitPrefix := subscriptionSplitRequestPrefix(requestId)
	returnValue := &SubscriptionPreConsumeResult{}

	err := DB.Transaction(func(tx *gorm.DB) error {
		var existingRecords []SubscriptionPreConsumeRecord
		if err := tx.Where("request_id = ? OR request_id LIKE ?", requestId, splitPrefix+"%").
			Order("id asc").
			Find(&existingRecords).Error; err != nil {
			return err
		}
		if len(existingRecords) > 0 {
			var primaryRecord SubscriptionPreConsumeRecord
			var totalPre int64
			allocs := make([]SubscriptionPreConsumeAllocation, 0, len(existingRecords))
			for _, rec := range existingRecords {
				if rec.Status == "refunded" {
					return errors.New("subscription pre-consume already refunded")
				}
				if rec.RequestId == requestId || primaryRecord.Id == 0 {
					primaryRecord = rec
				}
				totalPre += rec.PreConsumed
				allocs = append(allocs, SubscriptionPreConsumeAllocation{
					UserSubscriptionId: rec.UserSubscriptionId,
					PreConsumed:        rec.PreConsumed,
				})
			}
			var sub UserSubscription
			if err := tx.Where("id = ?", primaryRecord.UserSubscriptionId).First(&sub).Error; err != nil {
				return err
			}
			returnValue.UserSubscriptionId = sub.Id
			returnValue.PreConsumed = totalPre
			returnValue.PrimaryPreConsumed = primaryRecord.PreConsumed
			returnValue.AmountTotal = sub.AmountTotal
			returnValue.AmountUsedBefore = sub.AmountUsed
			returnValue.AmountUsedAfter = sub.AmountUsed
			returnValue.Allocations = allocs
			return nil
		}

		var subs []UserSubscription
		if err := lockForUpdate(tx).
			Where("user_id = ? AND status = ? AND end_time > ?", userId, "active", now).
			Order("end_time asc, id asc").
			Find(&subs).Error; err != nil {
			return errors.New("no active subscription")
		}
		if len(subs) == 0 {
			return errors.New("no active subscription")
		}
		scoped := make([]UserSubscription, 0, len(subs))
		general := make([]UserSubscription, 0, len(subs))
		for _, candidate := range subs {
			if !SubscriptionMatchesGroup(candidate.AllowedGroups, usingGroup) {
				continue
			}
			if NormalizeAllowedGroups(candidate.AllowedGroups) != "" {
				scoped = append(scoped, candidate)
			} else {
				general = append(general, candidate)
			}
		}
		eligible := append(scoped, general...)
		if len(eligible) == 0 {
			return errors.New("no active subscription")
		}

		allowOverflow := common.NormalizeBillingPreference(billingPreference) != "subscription_only"
		for i := range eligible {
			plan, err := getSubscriptionPlanByIdTx(tx, eligible[i].PlanId)
			if err != nil {
				return err
			}
			if err := maybeResetUserSubscriptionWithPlanTx(tx, &eligible[i], plan, now); err != nil {
				return err
			}
			if !eligible[i].AllowWalletOverflow {
				allowOverflow = false
			}
		}

		var totalRemain int64
		hasUnlimited := false
		for i := range eligible {
			if eligible[i].AmountTotal <= 0 {
				hasUnlimited = true
				break
			}
			if rem := eligible[i].AmountTotal - eligible[i].AmountUsed; rem > 0 {
				totalRemain += rem
			}
		}
		if !hasUnlimited && totalRemain == 0 {
			return fmt.Errorf("subscription quota insufficient, need=%d", amount)
		}

		subTarget := amount
		var walletNeed int64
		if !hasUnlimited && totalRemain < amount {
			if !allowOverflow {
				return fmt.Errorf("subscription quota insufficient, need=%d", amount)
			}
			walletNeed = amount - totalRemain
			subTarget = totalRemain
			if walletNeed > int64(common.MaxWalletQuota) {
				return fmt.Errorf("%w: overflow=%d", ErrSubscriptionHybridQuotaInsufficient, walletNeed)
			}
			var user User
			if err := lockForUpdate(tx).Select("id", "quota", "status").First(&user, userId).Error; err != nil {
				return err
			}
			if user.Status != common.UserStatusEnabled {
				return errors.New("user is disabled")
			}
			if int64(user.Quota) < walletNeed {
				return fmt.Errorf("%w: need=%d subRemain=%d wallet=%d", ErrSubscriptionHybridQuotaInsufficient, amount, totalRemain, user.Quota)
			}
			res := tx.Model(&User{}).Where("id = ? AND quota >= ?", userId, int(walletNeed)).
				Update("quota", gorm.Expr("quota - ?", int(walletNeed)))
			if res.Error != nil {
				return res.Error
			}
			if res.RowsAffected != 1 {
				return fmt.Errorf("%w: concurrent wallet debit insufficient", ErrSubscriptionHybridQuotaInsufficient)
			}
		}

		remaining := subTarget
		allocs := make([]SubscriptionPreConsumeAllocation, 0, len(eligible))
		var primarySub *UserSubscription
		var primaryUsedBefore int64
		var primaryTake int64
		for i := range eligible {
			if remaining <= 0 {
				break
			}
			sub := &eligible[i]
			var take int64
			if sub.AmountTotal <= 0 {
				take = remaining
			} else {
				rem := sub.AmountTotal - sub.AmountUsed
				if rem <= 0 {
					continue
				}
				take = min(rem, remaining)
			}
			recReqID := requestId
			if len(allocs) > 0 {
				recReqID = subscriptionSplitRequestId(requestId, len(allocs))
			} else {
				primarySub = sub
				primaryUsedBefore = sub.AmountUsed
				primaryTake = take
			}
			record := &SubscriptionPreConsumeRecord{
				RequestId:          recReqID,
				UserId:             userId,
				UserSubscriptionId: sub.Id,
				PreConsumed:        take,
				Status:             "consumed",
			}
			if err := tx.Create(record).Error; err != nil {
				return err
			}
			sub.AmountUsed += take
			if err := tx.Save(sub).Error; err != nil {
				return err
			}
			allocs = append(allocs, SubscriptionPreConsumeAllocation{
				UserSubscriptionId: sub.Id,
				PreConsumed:        take,
			})
			remaining -= take
		}
		if remaining > 0 || primarySub == nil {
			return fmt.Errorf("subscription quota insufficient, need=%d", amount)
		}

		returnValue.UserSubscriptionId = primarySub.Id
		returnValue.PreConsumed = subTarget
		returnValue.PrimaryPreConsumed = primaryTake
		returnValue.WalletPreConsumed = walletNeed
		returnValue.AmountTotal = primarySub.AmountTotal
		returnValue.AmountUsedBefore = primaryUsedBefore
		returnValue.AmountUsedAfter = primarySub.AmountUsed
		returnValue.Allocations = allocs
		return nil
	})
	if err != nil {
		return nil, err
	}
	if returnValue.WalletPreConsumed > 0 && common.RedisEnabled {
		if status, cacheErr := cacheApplyUserQuotaDelta(userId, -returnValue.WalletPreConsumed); cacheErr != nil || status != cacheQuotaOK {
			_ = invalidateUserCache(userId)
		}
	}
	return returnValue, nil
}

// SubscriptionDeltaOverflowResult records how a subscription post-consume
// delta was absorbed across the primary subscription, stacked sibling subscriptions,
// and (when allowed) the user's unified wallet balance.
type SubscriptionDeltaOverflowResult struct {
	PrimaryDelta        int64
	SecondarySubDelta   int64
	WalletOverflowDelta int64
	UnbilledOverflow    int64
}

type SubscriptionPostConsumeOptions struct {
	UserSubscriptionId int
	UserId             int
	UsingGroup         string
	Delta              int64
	BillingPreference  string
	PreConsumeAllocs   []SubscriptionPreConsumeAllocation
}

// PostConsumeUserSubscriptionDeltaWithOverflow settles a subscription delta cleanly.
// Retained for backward compatibility; delegates to PostConsumeUserSubscriptionDeltaWithOptions.
func PostConsumeUserSubscriptionDeltaWithOverflow(userSubscriptionId, userId int, usingGroup string, delta int64) (SubscriptionDeltaOverflowResult, error) {
	return PostConsumeUserSubscriptionDeltaWithOptions(SubscriptionPostConsumeOptions{
		UserSubscriptionId: userSubscriptionId,
		UserId:             userId,
		UsingGroup:         usingGroup,
		Delta:              delta,
	})
}

// PostConsumeUserSubscriptionDeltaWithOptions settles a subscription delta while
// honoring multi-card split pre-consume refunds, user billing preferences
// (subscription_only never spills to wallet), and non-negative wallet bounds.
func PostConsumeUserSubscriptionDeltaWithOptions(opts SubscriptionPostConsumeOptions) (SubscriptionDeltaOverflowResult, error) {
	userSubscriptionId := opts.UserSubscriptionId
	userId := opts.UserId
	usingGroup := opts.UsingGroup
	delta := opts.Delta
	if userSubscriptionId <= 0 {
		return SubscriptionDeltaOverflowResult{}, errors.New("invalid userSubscriptionId")
	}
	if delta == 0 {
		return SubscriptionDeltaOverflowResult{}, nil
	}
	if delta < 0 {
		if len(opts.PreConsumeAllocs) <= 1 {
			if err := PostConsumeUserSubscriptionDelta(userSubscriptionId, delta); err != nil {
				return SubscriptionDeltaOverflowResult{}, err
			}
			return SubscriptionDeltaOverflowResult{PrimaryDelta: delta}, nil
		}
		var result SubscriptionDeltaOverflowResult
		refundRemain := -delta
		err := DB.Transaction(func(tx *gorm.DB) error {
			// Refund in reverse allocation order (LIFO) so secondary split cards recover first.
			for i := len(opts.PreConsumeAllocs) - 1; i >= 0 && refundRemain > 0; i-- {
				alloc := opts.PreConsumeAllocs[i]
				if alloc.UserSubscriptionId <= 0 || alloc.PreConsumed <= 0 {
					continue
				}
				var sub UserSubscription
				if err := lockForUpdate(tx).Where("id = ?", alloc.UserSubscriptionId).First(&sub).Error; err != nil {
					return err
				}
				refundable := min(refundRemain, min(alloc.PreConsumed, sub.AmountUsed))
				if refundable <= 0 {
					continue
				}
				sub.AmountUsed = max(sub.AmountUsed-refundable, int64(0))
				if err := tx.Save(&sub).Error; err != nil {
					return err
				}
				if sub.Id == userSubscriptionId {
					result.PrimaryDelta -= refundable
				} else {
					result.SecondarySubDelta -= refundable
				}
				refundRemain -= refundable
			}
			if refundRemain > 0 {
				var primary UserSubscription
				if err := lockForUpdate(tx).Where("id = ?", userSubscriptionId).First(&primary).Error; err != nil {
					return err
				}
				extraRefund := min(refundRemain, primary.AmountUsed)
				if extraRefund > 0 {
					primary.AmountUsed = max(primary.AmountUsed-extraRefund, int64(0))
					if err := tx.Save(&primary).Error; err != nil {
						return err
					}
					result.PrimaryDelta -= extraRefund
				}
			}
			return nil
		})
		if err != nil {
			return SubscriptionDeltaOverflowResult{}, err
		}
		return result, nil
	}

	var result SubscriptionDeltaOverflowResult
	var overflowRejected bool
	now := GetDBTimestamp()
	err := DB.Transaction(func(tx *gorm.DB) error {
		var primary UserSubscription
		if err := lockForUpdate(tx).Where("id = ?", userSubscriptionId).First(&primary).Error; err != nil {
			return err
		}
		if primary.AmountTotal <= 0 || primary.AmountUsed+delta <= primary.AmountTotal {
			primary.AmountUsed = max(primary.AmountUsed+delta, 0)
			result.PrimaryDelta = delta
			return tx.Save(&primary).Error
		}

		// Cap primary subscription at its total and compute overflow.
		primaryRemain := max(primary.AmountTotal-primary.AmountUsed, int64(0))
		if primaryRemain > 0 {
			primary.AmountUsed = primary.AmountTotal
			if err := tx.Save(&primary).Error; err != nil {
				return err
			}
			result.PrimaryDelta = primaryRemain
		}
		overflow := delta - primaryRemain
		effectiveUserId := userId
		if effectiveUserId <= 0 {
			effectiveUserId = primary.UserId
		}

		// Try absorbing overflow with stacked sibling active subscriptions matching the group.
		var siblings []UserSubscription
		if err := lockForUpdate(tx).
			Where("user_id = ? AND status = ? AND end_time > ? AND id <> ?", effectiveUserId, "active", now, primary.Id).
			Order("end_time asc, id asc").
			Find(&siblings).Error; err != nil {
			return err
		}
		scoped := make([]UserSubscription, 0, len(siblings))
		general := make([]UserSubscription, 0, len(siblings))
		for _, candidate := range siblings {
			if !SubscriptionMatchesGroup(candidate.AllowedGroups, usingGroup) {
				continue
			}
			if NormalizeAllowedGroups(candidate.AllowedGroups) != "" {
				scoped = append(scoped, candidate)
			} else {
				general = append(general, candidate)
			}
		}
		for _, candidate := range append(scoped, general...) {
			if overflow <= 0 {
				break
			}
			sub := candidate
			if plan, planErr := getSubscriptionPlanByIdTx(tx, sub.PlanId); planErr == nil {
				_ = maybeResetUserSubscriptionWithPlanTx(tx, &sub, plan, now)
			}
			var absorb int64
			if sub.AmountTotal <= 0 {
				absorb = overflow
			} else {
				remain := max(sub.AmountTotal-sub.AmountUsed, int64(0))
				absorb = min(remain, overflow)
			}
			if absorb <= 0 {
				continue
			}
			sub.AmountUsed += absorb
			if err := tx.Save(&sub).Error; err != nil {
				return err
			}
			result.SecondarySubDelta += absorb
			overflow -= absorb
		}

		if overflow <= 0 {
			return nil
		}

		// Check whether wallet overflow is permitted by user preference and active subscriptions.
		allowOverflow := primary.AllowWalletOverflow
		if common.NormalizeBillingPreference(opts.BillingPreference) == "subscription_only" {
			allowOverflow = false
		}
		if allowOverflow {
			for _, candidate := range append(scoped, general...) {
				if !candidate.AllowWalletOverflow {
					allowOverflow = false
					break
				}
			}
		}
		if !allowOverflow {
			// Commit absorbed subscription quota and refuse any wallet charge.
			result.UnbilledOverflow = overflow
			overflowRejected = true
			return nil
		}
		if overflow > int64(common.MaxWalletQuota) {
			return fmt.Errorf("subscription overflow exceeds wallet quota bound: %d", overflow)
		}
		var user User
		if err := lockForUpdate(tx).Select("id", "quota").First(&user, effectiveUserId).Error; err != nil {
			return err
		}
		if user.Quota <= 0 {
			result.UnbilledOverflow = overflow
			return nil
		}
		walletCharge := min(overflow, int64(user.Quota))
		walletDelta := int(walletCharge)
		res := tx.Model(&User{}).Where("id = ? AND quota >= ?", effectiveUserId, walletDelta).
			Update("quota", gorm.Expr("quota - ?", walletDelta))
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected > 0 {
			result.WalletOverflowDelta = walletCharge
		}
		if result.WalletOverflowDelta < overflow {
			result.UnbilledOverflow = overflow - result.WalletOverflowDelta
		}
		return nil
	})
	if err != nil {
		return SubscriptionDeltaOverflowResult{}, err
	}
	if result.WalletOverflowDelta > 0 && common.RedisEnabled {
		effectiveUserId := userId
		if effectiveUserId > 0 {
			if status, cacheErr := cacheApplyUserQuotaDelta(effectiveUserId, -result.WalletOverflowDelta); cacheErr != nil || status != cacheQuotaOK {
				_ = invalidateUserCache(effectiveUserId)
			}
		}
	}
	if overflowRejected {
		return result, fmt.Errorf("%w: overflow=%d", ErrSubscriptionWalletOverflowDisabled, result.UnbilledOverflow)
	}
	return result, nil
}
