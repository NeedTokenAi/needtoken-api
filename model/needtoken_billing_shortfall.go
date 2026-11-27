package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"time"

	"gorm.io/gorm"
)

const (
	BillingShortfallStatusPending  = "pending"
	BillingShortfallStatusResolved = "resolved"
)

// NeedTokenBillingShortfall persists uncollected billing shortfalls when a
// post-consume settlement exceeds available subscription and/or wallet funds
// without imposing unauthorized negative wallet balances.
type NeedTokenBillingShortfall struct {
	Id                  int    `json:"id" gorm:"primaryKey"`
	OperationKey        string `json:"-" gorm:"type:char(64);uniqueIndex;not null"`
	OperationId         string `json:"operation_id" gorm:"type:varchar(128);not null;index"`
	RequestId           string `json:"request_id" gorm:"type:varchar(128);index"`
	UserId              int    `json:"user_id" gorm:"index;not null"`
	ChannelId           int    `json:"channel_id" gorm:"index;not null"`
	ModelName           string `json:"model_name" gorm:"type:varchar(128);index"`
	UsingGroup          string `json:"using_group" gorm:"type:varchar(64)"`
	BillingPreference   string `json:"billing_preference" gorm:"type:varchar(32)"`
	SubscriptionId      int    `json:"subscription_id" gorm:"index;not null"`
	ActualQuota         int64  `json:"actual_quota" gorm:"not null"`
	SubscriptionCharged int64  `json:"subscription_charged" gorm:"not null"`
	WalletCharged       int64  `json:"wallet_charged" gorm:"not null"`
	CollectedQuota      int64  `json:"collected_quota" gorm:"not null"`
	UnbilledQuota       int64  `json:"unbilled_quota" gorm:"not null"`
	Reason              string `json:"reason" gorm:"type:varchar(64);not null"`
	Status              string `json:"status" gorm:"type:varchar(32);index;not null"`
	Note                string `json:"note,omitempty" gorm:"type:varchar(500)"`
	CreatedAt           int64  `json:"created_at" gorm:"index;not null"`
	UpdatedAt           int64  `json:"updated_at" gorm:"not null"`
}

func (NeedTokenBillingShortfall) TableName() string {
	return "needtoken_billing_shortfalls"
}

type BillingShortfallFilter struct {
	Status   string
	UserId   int
	Page     int
	PageSize int
}

type BillingShortfallQueryResult struct {
	Items               []NeedTokenBillingShortfall `json:"items"`
	Total               int64                       `json:"total"`
	TotalActualQuota    int64                       `json:"total_actual_quota"`
	TotalCollectedQuota int64                       `json:"total_collected_quota"`
	TotalUnbilledQuota  int64                       `json:"total_unbilled_quota"`
	PendingCount        int64                       `json:"pending_count"`
}

func billingShortfallKey(operationId string) string {
	sum := sha256.Sum256([]byte(operationId))
	return hex.EncodeToString(sum[:])
}

// RecordBillingShortfall persists an operating shortfall idempotently by OperationId.
func RecordBillingShortfall(ctx context.Context, rec NeedTokenBillingShortfall) error {
	if DB == nil || rec.UnbilledQuota <= 0 {
		return nil
	}
	opID := strings.TrimSpace(rec.OperationId)
	if opID == "" {
		return errors.New("operation_id is required")
	}
	opKey := billingShortfallKey(opID)
	now := time.Now().Unix()

	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var existing NeedTokenBillingShortfall
		err := lockForUpdate(tx).Where("operation_key = ?", opKey).First(&existing).Error
		if err == nil {
			return nil
		}
		if !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		rec.OperationKey = opKey
		rec.OperationId = opID
		if strings.TrimSpace(rec.Status) == "" {
			rec.Status = BillingShortfallStatusPending
		}
		if strings.TrimSpace(rec.Reason) == "" {
			rec.Reason = "unbilled_overflow"
		}
		rec.CreatedAt = now
		rec.UpdatedAt = now
		return tx.Create(&rec).Error
	})
}

// QueryBillingShortfalls lists persisted shortfalls alongside aggregated financial totals.
func QueryBillingShortfalls(ctx context.Context, filter BillingShortfallFilter) (*BillingShortfallQueryResult, error) {
	if DB == nil {
		return &BillingShortfallQueryResult{Items: []NeedTokenBillingShortfall{}}, nil
	}
	page := max(filter.Page, 1)
	pageSize := filter.PageSize
	if pageSize <= 0 {
		pageSize = 20
	}
	pageSize = min(pageSize, 100)

	base := DB.WithContext(ctx).Model(&NeedTokenBillingShortfall{})
	if status := strings.TrimSpace(filter.Status); status != "" && status != "all" {
		base = base.Where("status = ?", status)
	}
	if filter.UserId > 0 {
		base = base.Where("user_id = ?", filter.UserId)
	}

	var total int64
	if err := base.Session(&gorm.Session{}).Count(&total).Error; err != nil {
		return nil, err
	}

	var agg struct {
		Actual    int64
		Collected int64
		Unbilled  int64
	}
	if err := base.Session(&gorm.Session{}).Select(
		"COALESCE(SUM(actual_quota), 0) AS actual, COALESCE(SUM(collected_quota), 0) AS collected, COALESCE(SUM(unbilled_quota), 0) AS unbilled",
	).Scan(&agg).Error; err != nil {
		return nil, err
	}

	var pendingCount int64
	if err := DB.WithContext(ctx).Model(&NeedTokenBillingShortfall{}).
		Where("status = ?", BillingShortfallStatusPending).
		Count(&pendingCount).Error; err != nil {
		return nil, err
	}

	items := make([]NeedTokenBillingShortfall, 0, pageSize)
	if err := base.Session(&gorm.Session{}).Order("id desc").Limit(pageSize).Offset((page - 1) * pageSize).Find(&items).Error; err != nil {
		return nil, err
	}

	return &BillingShortfallQueryResult{
		Items:               items,
		Total:               total,
		TotalActualQuota:    agg.Actual,
		TotalCollectedQuota: agg.Collected,
		TotalUnbilledQuota:  agg.Unbilled,
		PendingCount:        pendingCount,
	}, nil
}
