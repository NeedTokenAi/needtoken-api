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

// ErrSubscriptionWalletOverflowDisabled indicates positive subscription delta exceeded
// all eligible active subscriptions and wallet spillover was disallowed by either
// the user's subscription_only preference or a subscription's allow_wallet_overflow=false.
var ErrSubscriptionWalletOverflowDisabled = errors.New("subscription wallet_overflow_disabled")

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
