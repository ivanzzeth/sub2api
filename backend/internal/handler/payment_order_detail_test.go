package handler

import (
	"testing"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	"github.com/Wei-Shaw/sub2api/internal/payment"
)

func TestPaymentOrderDetailOnlyExposesPendingPaymentInstructions(t *testing.T) {
	payURL := "https://pay.example.test/checkout"
	qrCode := "weixin://wxpay/bizpayurl?pr=test"
	order := &dbent.PaymentOrder{
		ID: 41, UserID: 5, Status: payment.OrderStatusPending,
		PayURL: &payURL, QrCode: &qrCode, CreatedAt: time.Now(), ExpiresAt: time.Now().Add(time.Hour),
	}
	result := paymentOrderDetailForResponse(order)
	if result.PayURL == nil || *result.PayURL != payURL || result.QRCode == nil || *result.QRCode != qrCode {
		t.Fatalf("pending order payment instructions missing: %+v", result)
	}
	listed := sanitizePaymentOrdersForResponse([]*dbent.PaymentOrder{order})
	if len(listed) != 1 || listed[0].PayURL != nil || listed[0].QRCode != nil {
		t.Fatalf("order list exposed payment instructions: %+v", listed)
	}
	order.ExpiresAt = time.Now().Add(-time.Minute)
	result = paymentOrderDetailForResponse(order)
	if result.PayURL != nil || result.QRCode != nil {
		t.Fatalf("expired pending order exposed payment instructions: %+v", result)
	}
	for _, status := range []string{payment.OrderStatusPaid, payment.OrderStatusCompleted, payment.OrderStatusCancelled} {
		order.Status = status
		result = paymentOrderDetailForResponse(order)
		if result.PayURL != nil || result.QRCode != nil {
			t.Fatalf("%s order exposed payment instructions: %+v", status, result)
		}
	}
}
