package service

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/bytedance/gopkg/util/gopool"
)

// RecoverPendingBillingRefunds retries due persisted NeedTokenBillingRefund records.
func RecoverPendingBillingRefunds(ctx context.Context, limit int) (int, error) {
	if model.DB == nil {
		return 0, nil
	}
	rows, err := model.ListDueBillingRefunds(ctx, time.Now().Unix(), limit)
	if err != nil {
		return 0, err
	}
	recovered := 0
	for i := range rows {
		completed, execErr := model.ExecuteBillingRefund(ctx, rows[i].OperationId)
		if execErr != nil {
			common.SysLog(common.LogText("billing refund recovery failed (op=%s): %s", rows[i].OperationId, execErr.Error()))
			continue
		}
		if completed {
			recovered++
		}
	}
	return recovered, nil
}

// StartBillingRefundRecoveryTask launches the background recovery loop for failed
// synchronous billing refunds.
func StartBillingRefundRecoveryTask() {
	gopool.Go(func() {
		ticker := time.NewTicker(15 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			if _, err := RecoverPendingBillingRefunds(context.Background(), 50); err != nil {
				common.SysLog(common.LogText("billing refund recovery scan error: %s", err.Error()))
			}
		}
	})
}
