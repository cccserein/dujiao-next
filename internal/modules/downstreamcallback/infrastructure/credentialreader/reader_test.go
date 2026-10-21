package credentialreader

import (
	"testing"
	"time"

	credentialdomain "github.com/dujiao-next/internal/modules/apicredential/domain"
	userdomain "github.com/dujiao-next/internal/modules/identity/user/domain"
)

type credentialSourceStub struct {
	credential *credentialdomain.ApiCredential
}

func (s credentialSourceStub) GetByID(uint) (*credentialdomain.ApiCredential, error) {
	return s.credential, nil
}
func (s credentialSourceStub) GetByApiKey(string) (*credentialdomain.ApiCredential, error) {
	return s.credential, nil
}

func TestReaderRejectsRevokedCallbackCredential(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name   string
		mutate func(*credentialdomain.ApiCredential)
	}{
		{"inactive", func(c *credentialdomain.ApiCredential) { c.IsActive = false }},
		{"disabled", func(c *credentialdomain.ApiCredential) { c.Status = "disabled" }},
		{"pending", func(c *credentialdomain.ApiCredential) { c.Status = "pending_review" }},
		{"deleted", func(c *credentialdomain.ApiCredential) { c.DeletedAt = &now }},
		{"user_disabled", func(c *credentialdomain.ApiCredential) { c.User.Status = "disabled" }},
		{"user_deleted", func(c *credentialdomain.ApiCredential) { c.User.DeletedAt = &now }},
		{"user_missing", func(c *credentialdomain.ApiCredential) { c.User = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &credentialdomain.ApiCredential{ID: 1, UserID: 7, ApiKey: "fake-key", ApiSecret: "fake-secret", Status: "approved", IsActive: true, User: &userdomain.User{ID: 7, Status: "active"}}
			tc.mutate(c)
			got, _ := New(credentialSourceStub{credential: c}).GetByID(1)
			if got != nil {
				t.Fatal("revoked credential or owner still authorizes secret-bearing callbacks")
			}
		})
	}
	c := &credentialdomain.ApiCredential{ID: 1, UserID: 7, ApiKey: "fake-key", ApiSecret: "fake-secret", Status: "approved", IsActive: true, User: &userdomain.User{ID: 7, Status: "active"}}
	got, err := New(credentialSourceStub{credential: c}).GetByID(1)
	if err != nil || got == nil || got.ID != 1 {
		t.Fatalf("valid callback credential rejected: %v", err)
	}
}
