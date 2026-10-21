package orderreader

import (
	"testing"
	"time"

	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"

	"github.com/dujiao-next/internal/shared/jsonmap"
)

type sourceStub struct {
	order *orderdomain.Order
}

func TestReaderDoesNotProjectRevokedDelivery(t *testing.T) {
	for _, status := range []string{"refunded", "canceled", "pending_payment", "unknown"} {
		t.Run(status, func(t *testing.T) {
			order := &orderdomain.Order{ID: 1, Status: status,
				Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "FAKE-CARD", LogisticsJSON: jsonmap.JSON{"code": "FAKE-CODE"}},
				Children:    []orderdomain.Order{{ID: 2, Status: "completed", Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "FAKE-CHILD"}}},
			}
			got, err := New(sourceStub{order: order}).GetByID(1)
			if err != nil || got == nil {
				t.Fatalf("read: %v", err)
			}
			if got.Fulfillment != nil || got.Children[0].Fulfillment != nil {
				t.Error("revoked parent still exposes delivery through callback projection")
			}
		})
	}
	order := &orderdomain.Order{Status: "partially_refunded", Children: []orderdomain.Order{
		{Status: "refunded", Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "FAKE-REVOKED"}},
		{Status: "completed", Fulfillment: &fulfillmentdomain.Fulfillment{Payload: "FAKE-VALID"}},
	}}
	got, err := New(sourceStub{order: order}).GetByID(1)
	if err != nil || got.Children[0].Fulfillment != nil || got.Children[1].Fulfillment == nil {
		t.Fatal("partially refunded callback must retain only entitled children")
	}
}

func (s sourceStub) GetByID(uint) (*orderdomain.Order, error) {
	return s.order, nil
}

func TestReaderProjectsParentAndChildFulfillment(t *testing.T) {
	parentID := uint(8)
	deliveredAt := time.Unix(1_700_000_000, 0).UTC()
	source := sourceStub{order: &orderdomain.Order{
		ID:      parentID,
		OrderNo: "DJ-8",
		Status:  "completed",
		Children: []orderdomain.Order{{
			ID:       9,
			ParentID: &parentID,
			Status:   "delivered",
			Fulfillment: &fulfillmentdomain.Fulfillment{
				Type:          "manual",
				Status:        "delivered",
				Payload:       "code-123",
				LogisticsJSON: jsonmap.JSON{"tracking_no": "TRACK-1"},
				DeliveredAt:   &deliveredAt,
			},
		}},
	}}

	projection, err := New(source).GetByID(parentID)
	if err != nil {
		t.Fatalf("GetByID() error = %v", err)
	}
	if projection.ID != parentID || projection.OrderNo != "DJ-8" || len(projection.Children) != 1 {
		t.Fatalf("order projection mismatch: %#v", projection)
	}
	fulfillment := projection.Children[0].Fulfillment
	if fulfillment == nil || fulfillment.Payload != "code-123" || fulfillment.DeliveryData["tracking_no"] != "TRACK-1" {
		t.Fatalf("fulfillment projection mismatch: %#v", fulfillment)
	}
	if fulfillment.DeliveredAt == nil || !fulfillment.DeliveredAt.Equal(deliveredAt) {
		t.Fatalf("delivered time mismatch: %#v", fulfillment.DeliveredAt)
	}
}
