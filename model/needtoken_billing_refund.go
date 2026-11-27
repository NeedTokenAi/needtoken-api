package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const (
	BillingRefundStateReserved = "reserved"
	BillingRefundStateSettled  = "settled"
	BillingRefundStatePending  = "refund_pending"
	BillingRefundStateRefunded = "refunded"
)

// NeedTokenBillingRefund persists the pre-consume snapshot and component-level
// refund completion state for synchronous BillingSession reservations so failed
// or interrupted refunds can recover safely without double-crediting.
type NeedTokenBillingRefund struct {
	OperationKey            string `json:"-" gorm:"type:char(64);primaryKey"`
	OperationId             string `json:"operation_id" gorm:"type:varchar(128);not null;index"`
	RequestId               string `json:"request_id" gorm:"type:varchar(128);index"`
	UserId                  int    `json:"user_id" gorm:"index;not null"`
	TokenId                 int    `json:"token_id" gorm:"index;not null"`
	IsPlayground            int    `json:"is_playground" gorm:"not null"`
	FundingSource           string `json:"funding_source" gorm:"type:varchar(32);not null"`
	WalletAmount            int    `json:"wallet_amount" gorm:"not null"`
	SubscriptionRequestId   string `json:"subscription_request_id" gorm:"type:varchar(128)"`
	SubscriptionId          int    `json:"subscription_id" gorm:"not null"`
	SubscriptionPreConsumed int64  `json:"subscription_pre_consumed" gorm:"not null"`
	ExtraReserved           int    `json:"extra_reserved" gorm:"not null"`
	TokenConsumed           int    `json:"token_consumed" gorm:"not null"`
	State                   string `json:"state" gorm:"type:varchar(24);index;not null"`
	FundingRefunded         int    `json:"funding_refunded" gorm:"not null"`
	ExtraRefunded           int    `json:"extra_refunded" gorm:"not null"`
	TokenRefunded           int    `json:"token_refunded" gorm:"not null"`
	Attempts                int    `json:"attempts" gorm:"not null"`
	LastError               string `json:"last_error,omitempty" gorm:"type:varchar(500)"`
	NextRetryAt             int64  `json:"next_retry_at" gorm:"index;not null"`
	CreatedAt               int64  `json:"created_at" gorm:"not null"`
	UpdatedAt               int64  `json:"updated_at" gorm:"index;not null"`
}

func (NeedTokenBillingRefund) TableName() string {
	return "needtoken_billing_refunds"
}

func BillingRefundKey(operationId string) string {
	sum := sha256.Sum256([]byte(operationId))
	return hex.EncodeToString(sum[:])
}

func truncateBillingRefundError(msg string) string {
	msg = strings.TrimSpace(msg)
	if len(msg) <= 480 {
		return msg
	}
	return msg[:480]
}

func billingRefundRetryBackoffSec(attempts int) int64 {
	if attempts <= 1 {
		return 15
	}
	sec := int64(15)
	for i := 1; i < attempts && sec < 900; i++ {
		sec *= 2
	}
	return min(sec, int64(900))
}

func applyNeedTokenBillingSnapshot(dst *NeedTokenBillingRefund, src NeedTokenBillingRefund) {
	if dst.RequestId == "" && src.RequestId != "" {
		dst.RequestId = src.RequestId
	}
	if dst.UserId == 0 {
		dst.UserId = src.UserId
	}
	if dst.TokenId == 0 {
		dst.TokenId = src.TokenId
	}
	if src.IsPlayground != 0 {
		dst.IsPlayground = src.IsPlayground
	}
	if dst.FundingSource == "" {
		dst.FundingSource = src.FundingSource
	}
	if src.WalletAmount > dst.WalletAmount {
		dst.WalletAmount = src.WalletAmount
	}
	if dst.SubscriptionRequestId == "" && src.SubscriptionRequestId != "" {
		dst.SubscriptionRequestId = src.SubscriptionRequestId
	}
	if dst.SubscriptionId == 0 && src.SubscriptionId != 0 {
		dst.SubscriptionId = src.SubscriptionId
	}
	if src.SubscriptionPreConsumed > dst.SubscriptionPreConsumed {
		dst.SubscriptionPreConsumed = src.SubscriptionPreConsumed
	}
	if src.ExtraReserved > dst.ExtraReserved {
		dst.ExtraReserved = src.ExtraReserved
	}
	if src.TokenConsumed > dst.TokenConsumed {
		dst.TokenConsumed = src.TokenConsumed
	}
}

// UpsertBillingRefundRecord persists or updates the pre-consume snapshot while
// the session is active so process loss never erases an un-settled reservation.
func UpsertBillingRefundRecord(ctx context.Context, rec NeedTokenBillingRefund) error {
	opID := strings.TrimSpace(rec.OperationId)
	if opID == "" || DB == nil {
		return nil
	}
	opKey := BillingRefundKey(opID)
	now := time.Now().Unix()

	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing NeedTokenBillingRefund
		err := lockForUpdate(tx).Where("operation_key = ?", opKey).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			rec.OperationKey = opKey
			rec.OperationId = opID
			rec.State = BillingRefundStateReserved
			rec.FundingRefunded = 0
			rec.ExtraRefunded = 0
			rec.TokenRefunded = 0
			rec.Attempts = 0
			rec.LastError = ""
			rec.NextRetryAt = 0
			rec.CreatedAt = now
			rec.UpdatedAt = now
			return tx.Create(&rec).Error
		}
		if err != nil {
			return err
		}
		if existing.State == BillingRefundStateSettled || existing.State == BillingRefundStateRefunded {
			return nil
		}
		applyNeedTokenBillingSnapshot(&existing, rec)
		existing.UpdatedAt = now
		return tx.Save(&existing).Error
	})
}

// MarkBillingRefundSettled marks the persisted reservation settled after
// funding settlement commits, preventing any subsequent refund recovery.
func MarkBillingRefundSettled(ctx context.Context, operationId string) error {
	opID := strings.TrimSpace(operationId)
	if opID == "" || DB == nil {
		return nil
	}
	opKey := BillingRefundKey(opID)
	now := time.Now().Unix()

	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing NeedTokenBillingRefund
		err := lockForUpdate(tx).Where("operation_key = ?", opKey).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if existing.State == BillingRefundStateRefunded || existing.State == BillingRefundStateSettled {
			return nil
		}
		existing.State = BillingRefundStateSettled
		existing.NextRetryAt = 0
		existing.LastError = ""
		existing.UpdatedAt = now
		return tx.Save(&existing).Error
	})
}

// MarkBillingRefundPending durably records that a reservation failed and must be
// refunded, without marking it completed before the database refund commits.
func MarkBillingRefundPending(ctx context.Context, rec NeedTokenBillingRefund) (*NeedTokenBillingRefund, error) {
	opID := strings.TrimSpace(rec.OperationId)
	if opID == "" {
		return nil, errors.New("operation_id is required")
	}
	if DB == nil {
		return nil, errors.New("database is not initialized")
	}
	opKey := BillingRefundKey(opID)
	now := time.Now().Unix()

	var result NeedTokenBillingRefund
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing NeedTokenBillingRefund
		err := lockForUpdate(tx).Where("operation_key = ?", opKey).First(&existing).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			rec.OperationKey = opKey
			rec.OperationId = opID
			rec.State = BillingRefundStatePending
			rec.FundingRefunded = 0
			rec.ExtraRefunded = 0
			rec.TokenRefunded = 0
			rec.Attempts = 0
			rec.LastError = ""
			rec.NextRetryAt = now
			rec.CreatedAt = now
			rec.UpdatedAt = now
			if createErr := tx.Create(&rec).Error; createErr != nil {
				return createErr
			}
			result = rec
			return nil
		}
		if err != nil {
			return err
		}
		if existing.State == BillingRefundStateSettled || existing.State == BillingRefundStateRefunded {
			result = existing
			return nil
		}
		applyNeedTokenBillingSnapshot(&existing, rec)
		existing.State = BillingRefundStatePending
		existing.NextRetryAt = now
		existing.UpdatedAt = now
		if saveErr := tx.Save(&existing).Error; saveErr != nil {
			return saveErr
		}
		result = existing
		return nil
	})
	if err != nil {
		return nil, err
	}
	return &result, nil
}

func refundSubscriptionPreConsumeInTx(tx *gorm.DB, requestId string) error {
	requestId = strings.TrimSpace(requestId)
	if requestId == "" {
		return errors.New("subscription requestId is empty")
	}
	splitPrefix := subscriptionSplitRequestPrefix(requestId)
	var records []SubscriptionPreConsumeRecord
	if err := lockForUpdate(tx).
		Where("request_id = ? OR request_id LIKE ?", requestId, splitPrefix+"%").
		Order("id asc").
		Find(&records).Error; err != nil {
		return err
	}
	if len(records) == 0 {
		return gorm.ErrRecordNotFound
	}
	for i := range records {
		rec := &records[i]
		if rec.Status == "refunded" {
			continue
		}
		if rec.PreConsumed > 0 {
			var sub UserSubscription
			if err := lockForUpdate(tx).Where("id = ?", rec.UserSubscriptionId).First(&sub).Error; err != nil {
				return err
			}
			sub.AmountUsed = max(sub.AmountUsed-rec.PreConsumed, int64(0))
			if err := tx.Save(&sub).Error; err != nil {
				return err
			}
		}
		rec.Status = "refunded"
		if err := tx.Save(rec).Error; err != nil {
			return err
		}
	}
	return nil
}

func recordBillingRefundFailure(ctx context.Context, opKey string, opErr error) {
	if DB == nil || opErr == nil {
		return
	}
	now := time.Now().Unix()
	_ = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rec NeedTokenBillingRefund
		if err := lockForUpdate(tx).Where("operation_key = ?", opKey).First(&rec).Error; err != nil {
			return err
		}
		if rec.State != BillingRefundStatePending {
			return nil
		}
		rec.Attempts++
		rec.LastError = truncateBillingRefundError(opErr.Error())
		rec.NextRetryAt = now + billingRefundRetryBackoffSec(rec.Attempts)
		rec.UpdatedAt = now
		return tx.Save(&rec).Error
	})
}

// ExecuteBillingRefund atomically executes and tracks each refund component
// (funding source, extra subscription reserve, token quota) directly against
// the primary database so partial progress is preserved across failures/crashes.
func ExecuteBillingRefund(ctx context.Context, operationId string) (bool, error) {
	opID := strings.TrimSpace(operationId)
	if opID == "" {
		return false, errors.New("operation_id is required")
	}
	if DB == nil {
		return false, errors.New("database is not initialized")
	}
	opKey := BillingRefundKey(opID)

	// Step 1: refund funding source atomically with FundingRefunded=1.
	var userCacheCredit int
	var userCacheUID int
	var earlyCompleted bool
	err := DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rec NeedTokenBillingRefund
		if err := lockForUpdate(tx).Where("operation_key = ?", opKey).First(&rec).Error; err != nil {
			return err
		}
		switch rec.State {
		case BillingRefundStateRefunded:
			earlyCompleted = true
			return nil
		case BillingRefundStatePending:
		default:
			return nil
		}
		if rec.FundingRefunded == 1 {
			return nil
		}

		switch rec.FundingSource {
		case "wallet":
			if rec.WalletAmount > 0 && rec.UserId > 0 {
				if err := common.ValidateWalletQuota(rec.WalletAmount); err != nil {
					return err
				}
				res := tx.Model(&User{}).
					Where("id = ? AND quota <= ?", rec.UserId, common.MaxWalletQuota-rec.WalletAmount).
					Update("quota", gorm.Expr("quota + ?", rec.WalletAmount))
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					var cnt int64
					if err := tx.Model(&User{}).Where("id = ?", rec.UserId).Count(&cnt).Error; err != nil {
						return err
					}
					if cnt == 0 {
						return gorm.ErrRecordNotFound
					}
					return ErrWalletQuotaLimitExceeded
				}
				userCacheUID = rec.UserId
				userCacheCredit = rec.WalletAmount
			}
		case "subscription":
			if rec.SubscriptionPreConsumed > 0 {
				if err := refundSubscriptionPreConsumeInTx(tx, rec.SubscriptionRequestId); err != nil {
					return err
				}
			}
			if rec.WalletAmount > 0 && rec.UserId > 0 {
				if err := common.ValidateWalletQuota(rec.WalletAmount); err != nil {
					return err
				}
				res := tx.Model(&User{}).
					Where("id = ? AND quota <= ?", rec.UserId, common.MaxWalletQuota-rec.WalletAmount).
					Update("quota", gorm.Expr("quota + ?", rec.WalletAmount))
				if res.Error != nil {
					return res.Error
				}
				if res.RowsAffected != 1 {
					var cnt int64
					if err := tx.Model(&User{}).Where("id = ?", rec.UserId).Count(&cnt).Error; err != nil {
						return err
					}
					if cnt == 0 {
						return gorm.ErrRecordNotFound
					}
					return ErrWalletQuotaLimitExceeded
				}
				userCacheUID = rec.UserId
				userCacheCredit = rec.WalletAmount
			}
		}

		rec.FundingRefunded = 1
		rec.UpdatedAt = time.Now().Unix()
		return tx.Save(&rec).Error
	})
	if err != nil {
		recordBillingRefundFailure(ctx, opKey, err)
		return false, err
	}
	if userCacheCredit > 0 && userCacheUID > 0 {
		if cacheErr := cacheIncrUserQuota(userCacheUID, int64(userCacheCredit)); cacheErr != nil {
			common.SysLog(common.LogText("failed to sync user quota cache during billing refund: %s", cacheErr.Error()))
		}
	}
	if earlyCompleted {
		return true, nil
	}

	// Step 2: refund extra pre-consume reserve on subscription if needed.
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rec NeedTokenBillingRefund
		if err := lockForUpdate(tx).Where("operation_key = ?", opKey).First(&rec).Error; err != nil {
			return err
		}
		if rec.State == BillingRefundStateRefunded {
			earlyCompleted = true
			return nil
		}
		if rec.State != BillingRefundStatePending || rec.ExtraRefunded == 1 {
			return nil
		}
		if rec.FundingSource == "subscription" && rec.ExtraReserved > 0 && rec.SubscriptionId > 0 {
			var sub UserSubscription
			if err := lockForUpdate(tx).Where("id = ?", rec.SubscriptionId).First(&sub).Error; err != nil {
				return err
			}
			sub.AmountUsed = max(sub.AmountUsed-int64(rec.ExtraReserved), int64(0))
			if err := tx.Save(&sub).Error; err != nil {
				return err
			}
		}
		rec.ExtraRefunded = 1
		rec.UpdatedAt = time.Now().Unix()
		return tx.Save(&rec).Error
	})
	if err != nil {
		recordBillingRefundFailure(ctx, opKey, err)
		return false, err
	}
	if earlyCompleted {
		return true, nil
	}

	// Step 3: refund token quota atomically and finalize record if all components done.
	var tokenCacheID int
	var tokenCacheCredit int
	var finalized bool
	err = DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var rec NeedTokenBillingRefund
		if err := lockForUpdate(tx).Where("operation_key = ?", opKey).First(&rec).Error; err != nil {
			return err
		}
		if rec.State == BillingRefundStateRefunded {
			finalized = true
			return nil
		}
		if rec.State != BillingRefundStatePending {
			return nil
		}
		now := time.Now().Unix()
		if rec.TokenRefunded == 0 {
			if rec.TokenConsumed > 0 && rec.IsPlayground == 0 && rec.TokenId > 0 {
				if err := tx.Model(&Token{}).Where("id = ?", rec.TokenId).Updates(map[string]any{
					"remain_quota":  gorm.Expr("remain_quota + ?", rec.TokenConsumed),
					"used_quota":    gorm.Expr("used_quota - ?", rec.TokenConsumed),
					"accessed_time": common.GetTimestamp(),
				}).Error; err != nil {
					return err
				}
				tokenCacheID = rec.TokenId
				tokenCacheCredit = rec.TokenConsumed
			}
			rec.TokenRefunded = 1
		}
		if rec.FundingRefunded == 1 && rec.ExtraRefunded == 1 && rec.TokenRefunded == 1 {
			rec.State = BillingRefundStateRefunded
			rec.NextRetryAt = 0
			rec.LastError = ""
			finalized = true
		}
		rec.UpdatedAt = now
		return tx.Save(&rec).Error
	})
	if err != nil {
		recordBillingRefundFailure(ctx, opKey, err)
		return false, err
	}
	if tokenCacheCredit > 0 && tokenCacheID > 0 && common.RedisEnabled {
		var tok Token
		if qErr := DB.WithContext(ctx).Select("id", "key").First(&tok, tokenCacheID).Error; qErr == nil && tok.Key != "" {
			if _, cacheErr := cacheApplyTokenQuotaDelta(tokenCacheID, tok.Key, int64(tokenCacheCredit)); cacheErr != nil {
				common.SysLog(common.LogText("failed to sync token quota cache during billing refund: %s", cacheErr.Error()))
			}
		}
	}
	if finalized {
		return true, nil
	}
	return false, fmt.Errorf("billing refund %s incomplete", opID)
}

// ListDueBillingRefunds returns persisted refund_pending operations ready for recovery.
func ListDueBillingRefunds(ctx context.Context, nowUnix int64, limit int) ([]NeedTokenBillingRefund, error) {
	if DB == nil {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	var rows []NeedTokenBillingRefund
	err := DB.WithContext(ctx).
		Where("state = ? AND next_retry_at <= ?", BillingRefundStatePending, nowUnix).
		Order("updated_at asc").
		Limit(limit).
		Find(&rows).Error
	return rows, err
}
