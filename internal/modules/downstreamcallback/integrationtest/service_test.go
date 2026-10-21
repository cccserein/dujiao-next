package integrationtest

import (
	"context"
	"errors"
	"testing"
	"time"

	credentialdomain "github.com/dujiao-next/internal/modules/apicredential/domain"
	credentialreader "github.com/dujiao-next/internal/modules/downstreamcallback/infrastructure/credentialreader"
	orderreader "github.com/dujiao-next/internal/modules/downstreamcallback/infrastructure/orderreader"
	fulfillmentdomain "github.com/dujiao-next/internal/modules/fulfillment/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
	orderdomain "github.com/dujiao-next/internal/modules/order/domain"

	downstreamapp "github.com/dujiao-next/internal/modules/downstreamcallback/application"
	downstreamcontract "github.com/dujiao-next/internal/modules/downstreamcallback/contract"
	downstreamdomain "github.com/dujiao-next/internal/modules/downstreamcallback/domain"
)

type liveOrderSource struct{ order *orderdomain.Order }

func (s *liveOrderSource) GetByID(uint) (*orderdomain.Order, error) { return s.order, nil }

type liveCredentialSource struct {
	credential *credentialdomain.ApiCredential
}

func (s *liveCredentialSource) GetByID(uint) (*credentialdomain.ApiCredential, error) {
	return s.credential, nil
}
func (s *liveCredentialSource) GetByApiKey(string) (*credentialdomain.ApiCredential, error) {
	return s.credential, nil
}

func TestRetriedCallbackRechecksOrderAndCredential(t *testing.T) {
	ref := &downstreamdomain.OrderRef{ID: 3, OrderID: 8, ApiCredentialID: 5, CallbackURL: "https://callback.example.test"}
	references := &refRepositoryStub{byID: map[uint]*downstreamdomain.OrderRef{3: ref}}
	orders := &liveOrderSource{order: &orderdomain.Order{ID: 8, Status: "completed", Fulfillment: &fulfillmentdomain.Fulfillment{Status: "delivered", Payload: "FAKE-CARD"}}}
	credentials := &liveCredentialSource{credential: &credentialdomain.ApiCredential{ID: 5, UserID: 7, Status: "approved", IsActive: true, ApiKey: "fake-key", ApiSecret: "fake-secret", User: &userdomain.User{ID: 7, Status: "active"}}}
	deliverer := &delivererStub{err: errors.New("synthetic network outage")}
	service := newService(references, orderreader.New(orders), credentialreader.New(credentials), &callbackQueueStub{}, deliverer)
	if err := service.SendCallback(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 1 || deliverer.request.Payload.Fulfillment == nil {
		t.Fatal("valid delivery did not reach sender")
	}
	// The first send failed; refund before the queued retry is consumed.
	orders.order.Status = "refunded"
	deliverer.err = nil
	if err := service.SendCallback(context.Background(), 3); err != nil {
		t.Fatal(err)
	}
	if deliverer.calls != 2 || deliverer.request.Payload.Fulfillment != nil || deliverer.request.Payload.Status != "refunded" {
		t.Fatal("refund retry leaked the previously stored card")
	}
	// A credential disabled while queued must stop all subsequent delivery.
	orders.order.Status = "completed"
	credentials.credential.IsActive = false
	if err := service.SendCallback(context.Background(), 3); err == nil {
		t.Fatal("disabled callback credential should fail closed")
	}
	if deliverer.calls != 2 {
		t.Fatal("callback sent after credential revocation")
	}
}

type refRepositoryStub struct {
	byID      map[uint]*downstreamdomain.OrderRef
	byOrderID map[uint]*downstreamdomain.OrderRef
	created   *downstreamdomain.OrderRef
	updated   []*downstreamdomain.OrderRef
}

func (r *refRepositoryStub) GetByID(id uint) (*downstreamdomain.OrderRef, error) {
	return r.byID[id], nil
}

func (r *refRepositoryStub) GetByOrderID(orderID uint) (*downstreamdomain.OrderRef, error) {
	return r.byOrderID[orderID], nil
}

func (r *refRepositoryStub) GetByCredentialAndDownstreamNo(uint, string) (*downstreamdomain.OrderRef, error) {
	return nil, nil
}

func (r *refRepositoryStub) Create(ref *downstreamdomain.OrderRef) error {
	r.created = ref
	return nil
}

func (r *refRepositoryStub) Update(ref *downstreamdomain.OrderRef) error {
	r.updated = append(r.updated, ref)
	return nil
}

func (r *refRepositoryStub) ListPendingCallbacks(int) ([]downstreamdomain.OrderRef, error) {
	return nil, nil
}

func (r *refRepositoryStub) ListByCredentialID(uint, downstreamcontract.RefListFilter) ([]downstreamdomain.OrderRef, int64, error) {
	return nil, 0, nil
}

type orderReaderStub struct {
	orders map[uint]*downstreamcontract.OrderSnapshot
}

func (r orderReaderStub) GetByID(id uint) (*downstreamcontract.OrderSnapshot, error) {
	return r.orders[id], nil
}

type credentialReaderStub struct {
	credentials map[uint]*downstreamcontract.Credential
}

func (r credentialReaderStub) GetByID(id uint) (*downstreamcontract.Credential, error) {
	return r.credentials[id], nil
}

type queuedCallback struct {
	refID uint
	delay time.Duration
}

type callbackQueueStub struct {
	callbacks []queuedCallback
}

func (q *callbackQueueStub) EnqueueCallback(refID uint, delay time.Duration) error {
	q.callbacks = append(q.callbacks, queuedCallback{refID: refID, delay: delay})
	return nil
}

type delivererStub struct {
	request downstreamcontract.DeliveryRequest
	err     error
	calls   int
}

func (d *delivererStub) Send(_ context.Context, request downstreamcontract.DeliveryRequest) error {
	d.calls++
	d.request = request
	return d.err
}

func newService(references downstreamcontract.Repository, orders downstreamcontract.OrderReader, credentials downstreamcontract.CredentialReader, queue downstreamcontract.CallbackQueue, deliverer downstreamcontract.Deliverer) *downstreamapp.Service {
	return downstreamapp.NewService(downstreamapp.Options{
		References:  references,
		Orders:      orders,
		Credentials: credentials,
		Queue:       queue,
		Deliverer:   deliverer,
		Now:         func() time.Time { return time.Unix(1_700_000_000, 0).UTC() },
	})
}

func TestCreateRefInitializesPendingStatus(t *testing.T) {
	references := &refRepositoryStub{}
	service := newService(references, orderReaderStub{}, credentialReaderStub{}, nil, &delivererStub{})
	ref := &downstreamdomain.OrderRef{OrderID: 42, CallbackStatus: downstreamdomain.StatusFailed}

	if err := service.CreateRef(ref); err != nil {
		t.Fatalf("CreateRef() error = %v", err)
	}
	if references.created != ref || ref.CallbackStatus != downstreamdomain.StatusPending {
		t.Fatalf("created ref mismatch: %#v", references.created)
	}
	if err := service.CreateRef(nil); !errors.Is(err, downstreamcontract.ErrInvalidRef) {
		t.Fatalf("CreateRef(nil) error = %v", err)
	}
}

func TestEnqueueCallbackResolvesParentAndResetsRetryState(t *testing.T) {
	parentID := uint(17)
	ref := &downstreamdomain.OrderRef{
		ID:                 9,
		OrderID:            parentID,
		CallbackURL:        "https://callback.example.test",
		CallbackStatus:     downstreamdomain.StatusFailed,
		CallbackRetryCount: 4,
	}
	references := &refRepositoryStub{byOrderID: map[uint]*downstreamdomain.OrderRef{parentID: ref}}
	orders := orderReaderStub{orders: map[uint]*downstreamcontract.OrderSnapshot{21: {ID: 21, ParentID: &parentID}}}
	queue := &callbackQueueStub{}
	service := newService(references, orders, credentialReaderStub{}, queue, &delivererStub{})

	service.EnqueueCallback(21)

	if len(queue.callbacks) != 1 || queue.callbacks[0].refID != ref.ID || queue.callbacks[0].delay != 0 {
		t.Fatalf("queued callbacks = %#v", queue.callbacks)
	}
	if ref.CallbackStatus != downstreamdomain.StatusPending || ref.CallbackRetryCount != 0 {
		t.Fatalf("ref was not reset: %#v", ref)
	}
	if len(references.updated) != 1 {
		t.Fatalf("updates = %d, want 1", len(references.updated))
	}
}

func TestSendCallbackBuildsPayloadAndPersistsSentStatus(t *testing.T) {
	ref := &downstreamdomain.OrderRef{
		ID:                3,
		OrderID:           8,
		ApiCredentialID:   5,
		DownstreamOrderNo: "remote-8",
		CallbackURL:       "https://callback.example.test",
		CallbackStatus:    downstreamdomain.StatusPending,
	}
	references := &refRepositoryStub{byID: map[uint]*downstreamdomain.OrderRef{ref.ID: ref}}
	orders := orderReaderStub{orders: map[uint]*downstreamcontract.OrderSnapshot{8: {
		ID:      8,
		OrderNo: "DJ-8",
		Status:  "completed",
	}}}
	credentials := credentialReaderStub{credentials: map[uint]*downstreamcontract.Credential{5: {
		ID:        5,
		APIKey:    "downstream-key",
		APISecret: "downstream-secret",
	}}}
	deliverer := &delivererStub{}
	service := newService(references, orders, credentials, nil, deliverer)

	if err := service.SendCallback(context.Background(), ref.ID); err != nil {
		t.Fatalf("SendCallback() error = %v", err)
	}
	if deliverer.calls != 1 || deliverer.request.Payload.Event != "order.fulfilled" {
		t.Fatalf("delivery request mismatch: %#v", deliverer.request)
	}
	if deliverer.request.APIKey != "downstream-key" || deliverer.request.Payload.Timestamp != 1_700_000_000 {
		t.Fatalf("signed delivery inputs mismatch: %#v", deliverer.request)
	}
	if ref.CallbackStatus != downstreamdomain.StatusSent || ref.LastCallbackAt == nil {
		t.Fatalf("callback result mismatch: %#v", ref)
	}
	if len(references.updated) != 1 {
		t.Fatalf("updates = %d, want 1", len(references.updated))
	}
}

func TestSendCallbackFailureSchedulesBackoffAndPersistsRetry(t *testing.T) {
	ref := &downstreamdomain.OrderRef{ID: 3, OrderID: 8, ApiCredentialID: 5, CallbackURL: "https://callback.example.test"}
	references := &refRepositoryStub{byID: map[uint]*downstreamdomain.OrderRef{ref.ID: ref}}
	orders := orderReaderStub{orders: map[uint]*downstreamcontract.OrderSnapshot{8: {ID: 8, Status: "paid"}}}
	credentials := credentialReaderStub{credentials: map[uint]*downstreamcontract.Credential{5: {ID: 5}}}
	queue := &callbackQueueStub{}
	service := newService(references, orders, credentials, queue, &delivererStub{err: errors.New("network unavailable")})

	if err := service.SendCallback(context.Background(), ref.ID); err != nil {
		t.Fatalf("SendCallback() should persist retry state, got %v", err)
	}
	if ref.CallbackRetryCount != 1 || len(queue.callbacks) != 1 || queue.callbacks[0].delay != 30*time.Second {
		t.Fatalf("retry state mismatch: ref=%#v queue=%#v", ref, queue.callbacks)
	}
}

func TestSendCallbackReturnsStableNotFoundSentinel(t *testing.T) {
	service := newService(&refRepositoryStub{}, orderReaderStub{}, credentialReaderStub{}, nil, &delivererStub{})
	if err := service.SendCallback(context.Background(), 404); !errors.Is(err, downstreamcontract.ErrRefNotFound) {
		t.Fatalf("error = %v, want ErrRefNotFound", err)
	}
}
