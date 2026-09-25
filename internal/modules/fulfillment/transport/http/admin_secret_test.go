package fulfillmenthttp

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	"github.com/gin-gonic/gin"
)

type secretTestCreator struct{}

func (secretTestCreator) CreateManual(CreateManualInput) (*fulfillmentdomain.Fulfillment, error) {
	return nil, nil
}

type secretTestOrders struct{ reads int }

func (orders *secretTestOrders) GetOrderForAdmin(uint) (*orderdomain.Order, error) {
	orders.reads++
	return &orderdomain.Order{
		OrderNo:     "TEST-ORDER",
		Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "DISPOSABLE-TEST-CARD"},
	}, nil
}

type secretTestAuthorizer struct{ allowed bool }

func (authorizer secretTestAuthorizer) EnforceAdmin(_ uint, object, action string) (bool, error) {
	if object != "/admin/card-secrets" || action != "GET" {
		return false, nil
	}
	return authorizer.allowed, nil
}

func TestAdminDownloadFulfillmentRequiresCardSecretPermission(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, test := range []struct {
		name    string
		allowed bool
	}{
		{name: "denied", allowed: false},
		{name: "allowed", allowed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			orders := &secretTestOrders{}
			handler := NewAdminHandler(secretTestCreator{}, orders, secretTestAuthorizer{allowed: test.allowed})
			recorder := httptest.NewRecorder()
			context, _ := gin.CreateTestContext(recorder)
			context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/admin/orders/1/fulfillment/download", nil)
			context.Params = gin.Params{{Key: "id", Value: "1"}}
			context.Set("admin_id", uint(7))
			handler.AdminDownloadFulfillment(context)
			if !test.allowed {
				if orders.reads != 0 || strings.Contains(recorder.Body.String(), "DISPOSABLE-TEST-CARD") || !strings.Contains(recorder.Body.String(), `"status_code":403`) {
					t.Fatalf("forbidden request accessed delivery data: %s", recorder.Body.String())
				}
				return
			}
			if orders.reads != 1 || !strings.Contains(recorder.Body.String(), "DISPOSABLE-TEST-CARD") {
				t.Fatalf("authorized download failed: %s", recorder.Body.String())
			}
			if recorder.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("delivery download must not be cached")
			}
		})
	}
}
