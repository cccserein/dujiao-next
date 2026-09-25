package orderhttp

import (
	"encoding/json"
	"strings"
	"testing"

	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"
	"github.com/dujiao-next/internal/shared/jsonmap"
)

func TestMaskOrderFulfillmentRemovesParentAndChildSecrets(t *testing.T) {
	order := &orderdomain.Order{
		Fulfillment: &fulfillmentdomain.Fulfillment{
			Status: "delivered", Payload: "PARENT-CARD-SECRET",
			LogisticsJSON: jsonmap.JSON{"code": "PARENT-DELIVERY-SECRET"},
		},
		Children: []orderdomain.Order{{
			Fulfillment: &fulfillmentdomain.Fulfillment{
				Status: "delivered", Payload: "CHILD-CARD-SECRET",
				LogisticsJSON: jsonmap.JSON{"code": "CHILD-DELIVERY-SECRET"},
			},
		}},
	}
	maskOrderFulfillment(order)
	encoded, err := json.Marshal(order)
	if err != nil {
		t.Fatalf("marshal order: %v", err)
	}
	for _, secret := range []string{"PARENT-CARD-SECRET", "PARENT-DELIVERY-SECRET", "CHILD-CARD-SECRET", "CHILD-DELIVERY-SECRET"} {
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("masked order leaked %s", secret)
		}
	}
	if order.Fulfillment == nil || order.Children[0].Fulfillment == nil || order.Fulfillment.Status != "delivered" {
		t.Fatal("fulfillment status must remain visible for admin workflows")
	}
}
