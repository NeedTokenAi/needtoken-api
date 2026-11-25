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
)

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

// SubscriptionDeltaOverflowResult records how a positive subscription post-consume
// delta was absorbed across the primary subscription, stacked sibling subscriptions,
// and (when allowed) the user's unified wallet balance.
type SubscriptionDeltaOverflowResult struct {
	PrimaryDelta        int64
	SecondarySubDelta   int64
	WalletOverflowDelta int64
}

// PostConsumeUserSubscriptionDeltaWithOverflow settles a subscription delta cleanly.
// Negative deltas refund the primary subscription directly. Positive deltas that
// exceed the primary subscription's remaining quota cap the primary subscription
// at its total and spill the remainder into other eligible active subscriptions
// (scoped first, earliest expiry first) and finally into User.Quota when wallet
// overflow is permitted for the group.
func PostConsumeUserSubscriptionDeltaWithOverflow(userSubscriptionId, userId int, usingGroup string, delta int64) (SubscriptionDeltaOverflowResult, error) {
	if userSubscriptionId <= 0 {
		return SubscriptionDeltaOverflowResult{}, errors.New("invalid userSubscriptionId")
	}
	if delta == 0 {
		return SubscriptionDeltaOverflowResult{}, nil
	}
	if delta < 0 {
		if err := PostConsumeUserSubscriptionDelta(userSubscriptionId, delta); err != nil {
			return SubscriptionDeltaOverflowResult{}, err
		}
		return SubscriptionDeltaOverflowResult{PrimaryDelta: delta}, nil
	}

	var result SubscriptionDeltaOverflowResult
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

		// Check whether wallet overflow is permitted across matching active subscriptions.
		allowOverflow := primary.AllowWalletOverflow
		if allowOverflow {
			for _, candidate := range append(scoped, general...) {
				if !candidate.AllowWalletOverflow {
					allowOverflow = false
					break
				}
			}
		}
		if !allowOverflow {
			return fmt.Errorf("subscription used exceeds total, overflow=%d wallet_overflow_disabled", overflow)
		}
		if overflow > int64(common.MaxWalletQuota) {
			return fmt.Errorf("subscription overflow exceeds wallet quota bound: %d", overflow)
		}
		walletDelta := int(overflow)
		if err := tx.Model(&User{}).Where("id = ?", effectiveUserId).
			Update("quota", gorm.Expr("quota - ?", walletDelta)).Error; err != nil {
			return err
		}
		result.WalletOverflowDelta = overflow
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
	return result, nil
}
