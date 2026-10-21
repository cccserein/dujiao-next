package orderhttp

import (
	"encoding/base64"
	"net/http/httptest"
	"strings"
	"testing"

	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	reseller "github.com/dujiao-next/internal/modules/reseller/contract"

	"github.com/gin-gonic/gin"
)

type cacheUserOrderStub struct {
	UserOrderQuery
	order *orderdomain.Order
}

func (s cacheUserOrderStub) GetOrderByUserOrderNoForTenant(_ reseller.TenantContext, _ string, _ uint) (*orderdomain.Order, error) {
	return s.order, nil
}

type cacheGuestOrderStub struct {
	GuestOrderQuery
	order *orderdomain.Order
}

func (s cacheGuestOrderStub) GetOrderByGuestOrderNoForTenant(_ reseller.TenantContext, _, _, _ string) (*orderdomain.Order, error) {
	return s.order, nil
}

func TestOrderDetailSecretsAreNotCacheable(t *testing.T) {
	order := &orderdomain.Order{OrderNo: "FAKE-PAID", Status: "completed", Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "FAKE-OWN-CARD"}}
	for _, guest := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(recorder)
		ctx.Request = httptest.NewRequest("GET", "/orders/FAKE-PAID", nil)
		ctx.Params = gin.Params{{Key: "order_no", Value: order.OrderNo}}
		if guest {
			ctx.Request.Header.Set("Authorization", "Guest "+base64.RawURLEncoding.EncodeToString([]byte("buyer@example.test\nsynthetic-password")))
			NewGuestHandler(cacheGuestOrderStub{order: order}, nil, nil).GetGuestOrderByOrderNo(ctx)
		} else {
			ctx.Set("user_id", uint(7))
			NewUserHandler(cacheUserOrderStub{order: order}, nil, nil, nil).GetOrderByOrderNo(ctx)
		}
		if !strings.Contains(recorder.Body.String(), "FAKE-OWN-CARD") || recorder.Header().Get("Cache-Control") != "no-store" {
			t.Fatalf("guest=%v: expected authorized card delivery with no-store", guest)
		}
	}
}

func TestFulfillmentDownloadRejectsRefundedOrder(t *testing.T) {
	for _, status := range []string{"pending_payment", "canceled", "refunded"} {
		t.Run(status, func(t *testing.T) {
			order := &orderdomain.Order{
				OrderNo:     "TEST-BLOCKED",
				Status:      status,
				Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "FAKE-CARD"},
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest("GET", "/", nil)
			respondFulfillmentDownload(ctx, order)
			if strings.Contains(recorder.Body.String(), "FAKE-CARD") || !strings.Contains(recorder.Body.String(), `"status_code":404`) {
				t.Fatalf("%s delivery was not blocked: %s", status, recorder.Body.String())
			}
		})
	}
}

func TestFulfillmentDownloadSkipsRefundedChild(t *testing.T) {
	order := &orderdomain.Order{
		OrderNo: "TEST-PARTIAL",
		Status:  "partially_refunded",
		Children: []orderdomain.Order{
			{Status: "refunded", Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "REFUNDED-CARD"}},
			{Status: "delivered", Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "DELIVERED-CARD"}},
		},
	}
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest("GET", "/", nil)
	respondFulfillmentDownload(ctx, order)
	if recorder.Body.String() != "DELIVERED-CARD" || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("download included a refunded child or lost cache protection: %q", recorder.Body.String())
	}
}
