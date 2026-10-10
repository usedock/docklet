package host

import (
	"context"
	"errors"
	"sync"
	"testing"
)

func TestProvisionRefusesWhenFloorsExceedBudget(t *testing.T) {
	ctx := context.Background()
	h, driver, _ := testHost(t)
	h.cfg.CapacityVCPU = 8
	h.cfg.CapacityMemMiB = 16384
	if _, err := h.Provision(ctx, "big", resXLarge, "", ""); err != nil {
		t.Fatal(err)
	}
	_, err := h.Provision(ctx, "extra", resSmall, "", "")
	if !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want capacity_exhausted", err)
	}
	if driver.boots != 1 {
		t.Fatalf("boots = %d, refused provision must not boot", driver.boots)
	}
	if err := h.Destroy(ctx, "big"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Provision(ctx, "extra", resSmall, "", ""); err != nil {
		t.Fatalf("after destroy: %v", err)
	}
}

func TestSmallDocksFillBudgetByWeightNotCount(t *testing.T) {
	ctx := context.Background()
	h, _, _ := testHost(t)
	h.cfg.CapacityVCPU = 4
	if _, err := h.Provision(ctx, "a", resLarge, "", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Provision(ctx, "b", resDefault, "", ""); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want capacity_exhausted", err)
	}
}

func TestConcurrentReservationsNeverOvercommit(t *testing.T) {
	h, _, _ := testHost(t)
	h.cfg.CapacityVCPU = 4
	var wg sync.WaitGroup
	var mu sync.Mutex
	admitted := 0
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			release, err := h.reserve(string(rune('a'+i)), resDefault)
			if err != nil {
				return
			}
			_ = release
			mu.Lock()
			admitted++
			mu.Unlock()
		}()
	}
	wg.Wait()
	if admitted != 2 {
		t.Fatalf("admitted = %d, want 2 default docks in 4 vCPU", admitted)
	}
}

func TestUnsetBudgetAdmitsEverything(t *testing.T) {
	h, _, _ := testHost(t)
	for _, id := range []string{"a", "b", "c"} {
		if _, err := h.Provision(context.Background(), id, resXLarge, "", ""); err != nil {
			t.Fatal(err)
		}
	}
}

func TestNegativeMemoryBudgetRefusesEveryBoot(t *testing.T) {
	h, driver, _ := testHost(t)
	h.cfg.CapacityMemMiB = -1
	if _, err := h.Provision(context.Background(), "a", resSmall, "", ""); !errors.Is(err, ErrCapacityExhausted) {
		t.Fatalf("err = %v, want capacity_exhausted", err)
	}
	if driver.boots != 0 {
		t.Fatalf("boots = %d, want 0", driver.boots)
	}
}
