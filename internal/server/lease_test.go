package server

import (
	"context"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"
)

// newTestLease builds a lease with a named holder and a short confirm delay so
// tests stay fast.
func newTestLease(t *testing.T, store Storage, holder string, ttl time.Duration) *Lease {
	t.Helper()
	l := NewLease(store, zaptest.NewLogger(t), ttl)
	l.holder = holder
	return l
}

func withFastLeaseConfirm(t *testing.T) {
	t.Helper()
	prev := leaseConfirmDelay
	leaseConfirmDelay = time.Millisecond
	t.Cleanup(func() { leaseConfirmDelay = prev })
}

func TestLeaseAcquiredWhenFree(t *testing.T) {
	withFastLeaseConfirm(t)
	store := newMemStore()
	l := newTestLease(t, store, "pod-a", time.Minute)

	ok, err := l.TryAcquire(context.Background())
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !ok {
		t.Fatal("expected an unheld lease to be acquired")
	}
}

func TestLeaseDeniedWhileHeldByAnother(t *testing.T) {
	withFastLeaseConfirm(t)
	store := newMemStore()
	ctx := context.Background()

	a := newTestLease(t, store, "pod-a", time.Minute)
	if ok, err := a.TryAcquire(ctx); err != nil || !ok {
		t.Fatalf("pod-a should hold the lease: ok=%v err=%v", ok, err)
	}

	b := newTestLease(t, store, "pod-b", time.Minute)
	ok, err := b.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if ok {
		t.Fatal("a second replica must not acquire a lease that is still held")
	}
}

func TestLeaseAcquiredAfterExpiry(t *testing.T) {
	withFastLeaseConfirm(t)
	store := newMemStore()
	ctx := context.Background()

	a := newTestLease(t, store, "pod-a", time.Minute)
	// Pretend pod-a acquired the lease well in the past and then died.
	a.now = func() time.Time { return time.Now().Add(-2 * time.Hour) }
	if ok, err := a.TryAcquire(ctx); err != nil || !ok {
		t.Fatalf("pod-a should hold the lease: ok=%v err=%v", ok, err)
	}

	b := newTestLease(t, store, "pod-b", time.Minute)
	ok, err := b.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !ok {
		t.Fatal("an expired lease must be reclaimable, otherwise a dead replica stops the scanner forever")
	}
}

func TestLeaseRenewedByCurrentHolder(t *testing.T) {
	withFastLeaseConfirm(t)
	store := newMemStore()
	ctx := context.Background()

	a := newTestLease(t, store, "pod-a", time.Minute)
	for i := 0; i < 3; i++ {
		ok, err := a.TryAcquire(ctx)
		if err != nil {
			t.Fatalf("acquire %d: %v", i, err)
		}
		if !ok {
			t.Fatalf("the current holder must keep the lease on tick %d", i)
		}
	}
}

func TestLeaseTreatsCorruptRecordAsFree(t *testing.T) {
	withFastLeaseConfirm(t)
	store := newMemStore()
	ctx := context.Background()

	store.data[leaseKey] = memObj{body: []byte("not json"), contentType: "application/json"}

	l := newTestLease(t, store, "pod-a", time.Minute)
	ok, err := l.TryAcquire(ctx)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !ok {
		t.Fatal("a corrupt lease record must not wedge the scanner permanently")
	}
}
