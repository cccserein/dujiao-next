package orderhttp

import (
	"net/http/httptest"
	"strings"
	"testing"

	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"

	"github.com/gin-gonic/gin"
)

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
