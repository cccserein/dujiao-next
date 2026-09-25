package shop

import (
	"fmt"
	"time"
)

type User struct {
	ID    int64
	Email string
	Role  string
}

type Product struct {
	ID          int64
	Title       string
	Description string
	PriceCents  int64
	Currency    string
	Active      bool
	Stock       int64
}

type Order struct {
	ID              int64
	UserID          int64
	ProductID       int64
	Title           string
	AmountCents     int64
	Currency        string
	Status          string
	ActivePaymentID string
	CardID          int64
	CreatedAt       time.Time
	ExpiresAt       time.Time
	PayURL          string
	Card            string
}

type PaymentException struct {
	ID            string
	OrderID       int64
	CustomerEmail string
	Provider      string
	ExpectedCents int64
	PaidCents     int64
	Currency      string
	Reason        string
	CreatedAt     time.Time
}

func Money(cents int64, currency string) string {
	return fmt.Sprintf("%s %d.%02d", currency, cents/100, cents%100)
}
