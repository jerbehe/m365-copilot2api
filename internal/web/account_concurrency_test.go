package web

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestAccountConcurrencyLimitsAndReleasesSlots(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "2")
	limiter := newAccountConcurrency()
	release1, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	release2, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	if limiter.Available("account-a") {
		t.Fatal("account remained available at its configured limit")
	}
	if !limiter.Available("account-b") {
		t.Fatal("one full account must not block another account")
	}
	release1()
	if !limiter.Available("account-a") {
		t.Fatal("released slot was not returned")
	}
	release1()
	release2()
}

func TestAccountConcurrencyWaitHonorsCancellation(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "1")
	limiter := newAccountConcurrency()
	release, err := limiter.Acquire(context.Background(), "account-a")
	if err != nil {
		t.Fatal(err)
	}
	defer release()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := limiter.Acquire(ctx, "account-a"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire() error = %v, want deadline exceeded", err)
	}
}

func TestAccountConcurrencyUsesDocumentedDefault(t *testing.T) {
	t.Setenv("M365_ACCOUNT_DEFAULT_CONCURRENCY", "")
	limiter := newAccountConcurrency()
	if limiter.limit != defaultAccountConcurrency {
		t.Fatalf("limit = %d, want %d", limiter.limit, defaultAccountConcurrency)
	}
}
