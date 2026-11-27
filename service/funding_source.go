package service

import (
	"errors"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
)

// ---------------------------------------------------------------------------
// FundingSource — 资金来源接口（钱包 or 订阅）
// ---------------------------------------------------------------------------

// FundingSource 抽象了预扣费的资金来源。
type FundingSource interface {
	// Source 返回资金来源标识："wallet" 或 "subscription"
	Source() string
	// PreConsume 从该资金来源预扣 amount 额度
	PreConsume(amount int) error
	// Settle 根据差额调整资金来源（正数补扣，负数退还）
	Settle(delta int) error
	// Refund 退还所有预扣费
	Refund() error
}

// ---------------------------------------------------------------------------
// WalletFunding — 钱包资金来源实现
// ---------------------------------------------------------------------------

// ErrInsufficientWalletQuota 钱包原子预扣失败（余额不足），未发生任何扣减。
// BillingSession 据此映射为 ErrorCodeInsufficientUserQuota，
// 使 wallet_first 等计费偏好可以回退到订阅。
var ErrInsufficientWalletQuota = errors.New("wallet quota insufficient")

type WalletFunding struct {
	userId   int
	consumed int // 实际预扣的用户额度
}

func (w *WalletFunding) Source() string { return BillingSourceWallet }

func (w *WalletFunding) PreConsume(amount int) error {
	if amount <= 0 {
		return nil
	}
	reserved, err := model.TryReserveUserQuota(w.userId, amount)
	if err != nil {
		return err
	}
	if !reserved {
		return ErrInsufficientWalletQuota
	}
	// 发送前补扣会再次预扣，累加后退款才能退回全部预扣。
	w.consumed += amount
	return nil
}

func (w *WalletFunding) Settle(delta int) error {
	if delta == 0 {
		return nil
	}
	if delta > 0 {
		return model.DecreaseUserQuota(w.userId, delta, false)
	}
	return model.IncreaseUserQuota(w.userId, -delta, false)
}

func (w *WalletFunding) Refund() error {
	if w.consumed <= 0 {
		return nil
	}
	// IncreaseUserQuota 是 quota += N 的非幂等操作，不能重试，否则会多退额度。
	// 订阅的 RefundSubscriptionPreConsume 有 requestId 幂等保护所以可以重试。
	return model.IncreaseUserQuota(w.userId, w.consumed, false)
}

// ---------------------------------------------------------------------------
// SubscriptionFunding — 订阅资金来源实现
// ---------------------------------------------------------------------------

type SubscriptionFunding struct {
	requestId         string
	userId            int
	modelName         string
	usingGroup        string
	billingPreference string
	amount            int64 // 预扣的订阅额度（subConsume）
	subscriptionId    int
	preConsumed       int64
	walletPreConsumed int64
	allocations       []model.SubscriptionPreConsumeAllocation
	// 以下字段在 PreConsume 成功后填充，供 RelayInfo 同步使用
	AmountTotal     int64
	AmountUsedAfter int64
	PlanId          int
	PlanTitle       string
	// 后结算拆分记录，供 BillingSession 精确同步日志
	lastPrimaryDelta   int64
	lastWalletDelta    int64
	lastSecondarySub   int64
	lastUnbilledDelta  int64
	lastUnbilledReason string
}

func (s *SubscriptionFunding) Source() string { return BillingSourceSubscription }

func (s *SubscriptionFunding) PreConsume(_ int) error {
	// amount 参数被忽略，使用内部 s.amount（已在构造时根据 preConsumedQuota 计算）
	res, err := model.PreConsumeSubscriptionWithWalletSpillover(s.requestId, s.userId, s.modelName, s.usingGroup, s.amount, s.billingPreference)
	if err != nil {
		return err
	}
	s.subscriptionId = res.UserSubscriptionId
	s.preConsumed = res.PreConsumed
	s.walletPreConsumed = res.WalletPreConsumed
	s.allocations = res.Allocations
	s.AmountTotal = res.AmountTotal
	s.AmountUsedAfter = res.AmountUsedAfter
	// 获取订阅计划信息
	if planInfo, err := model.GetSubscriptionPlanInfoByUserSubscriptionId(res.UserSubscriptionId); err == nil && planInfo != nil {
		s.PlanId = planInfo.PlanId
		s.PlanTitle = planInfo.PlanTitle
	}
	return nil
}

func (s *SubscriptionFunding) Settle(delta int) error {
	if delta == 0 {
		return nil
	}
	if delta < 0 && s.walletPreConsumed > 0 {
		refundTotal := int64(-delta)
		walletRefund := min(refundTotal, s.walletPreConsumed)
		if walletRefund > 0 {
			if err := model.IncreaseUserQuota(s.userId, int(walletRefund), true); err != nil {
				return err
			}
			s.lastWalletDelta = -walletRefund
		}
		remainingRefund := refundTotal - walletRefund
		if remainingRefund <= 0 {
			return nil
		}
		res, err := model.PostConsumeUserSubscriptionDeltaWithOptions(model.SubscriptionPostConsumeOptions{
			UserSubscriptionId: s.subscriptionId,
			UserId:             s.userId,
			UsingGroup:         s.usingGroup,
			Delta:              -remainingRefund,
			BillingPreference:  s.billingPreference,
			PreConsumeAllocs:   s.allocations,
		})
		s.lastPrimaryDelta = res.PrimaryDelta
		s.lastSecondarySub = res.SecondarySubDelta
		return err
	}
	res, err := model.PostConsumeUserSubscriptionDeltaWithOptions(model.SubscriptionPostConsumeOptions{
		UserSubscriptionId: s.subscriptionId,
		UserId:             s.userId,
		UsingGroup:         s.usingGroup,
		Delta:              int64(delta),
		BillingPreference:  s.billingPreference,
		PreConsumeAllocs:   s.allocations,
	})
	s.lastPrimaryDelta = res.PrimaryDelta
	s.lastSecondarySub = res.SecondarySubDelta
	s.lastWalletDelta = res.WalletOverflowDelta
	s.lastUnbilledDelta = res.UnbilledOverflow
	if err != nil {
		if errors.Is(err, model.ErrSubscriptionWalletOverflowDisabled) {
			s.lastUnbilledReason = "wallet_overflow_disabled"
			// 订阅剩余额度已原子扣满且严禁扣钱包；记录告警并视为结算完成，避免把已交付内容的请求误退费。
			common.SysLog(common.LogText("subscription post-consume overflow capped without wallet charge (userId=%d, subId=%d, pref=%s, unbilled=%d)",
				s.userId, s.subscriptionId, s.billingPreference, res.UnbilledOverflow))
			return nil
		}
		return err
	}
	if res.UnbilledOverflow > 0 {
		s.lastUnbilledReason = "wallet_exhausted"
		common.SysLog(common.LogText("subscription wallet overflow clamped at non-negative balance (userId=%d, subId=%d, wallet=%d, unbilled=%d)",
			s.userId, s.subscriptionId, res.WalletOverflowDelta, res.UnbilledOverflow))
	}
	return nil
}

func (s *SubscriptionFunding) Refund() error {
	if s.preConsumed > 0 {
		if err := refundWithRetry(func() error {
			return model.RefundSubscriptionPreConsume(s.requestId)
		}); err != nil {
			return err
		}
	}
	if s.walletPreConsumed > 0 {
		return model.IncreaseUserQuota(s.userId, int(s.walletPreConsumed), true)
	}
	return nil
}

// refundWithRetry 尝试多次执行退款操作以提高成功率，只能用于基于事务的退款函数！！！！！！
// try to refund with retries, only for refund functions based on transactions!!!
func refundWithRetry(fn func() error) error {
	if fn == nil {
		return nil
	}
	const maxAttempts = 3
	var lastErr error
	for i := range maxAttempts {
		if err := fn(); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if i < maxAttempts-1 {
			time.Sleep(time.Duration(200*(i+1)) * time.Millisecond)
		}
	}
	return lastErr
}
