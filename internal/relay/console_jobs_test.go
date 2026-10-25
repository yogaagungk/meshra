package relay

import (
	"context"
	"testing"
)

func TestConsoleRevocationCancelsOnlyOwnedJobsAndPreservesCapacity(t *testing.T) {
	r, _ := openTest(t, nil)
	r.SetConsoleSessionValidator(func(id string) bool { return id == "first-owner" || id == "second-owner" })
	first, releaseFirst, err := r.beginConsoleJob(context.Background(), "first-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	second, releaseSecond, err := r.beginConsoleJob(context.Background(), "second-owner")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()
	r.RevokeConsoleSession("first-owner")
	select {
	case <-first.Done():
	default:
		t.Fatal("revoked owner retained an active job")
	}
	select {
	case <-second.Done():
		t.Fatal("revocation cancelled an unrelated owner")
	default:
	}
	releaseFirst()
	releaseFirst()
	_, _, err = r.beginConsoleJob(context.Background(), "first-owner")
	requireStatus(t, err, 403)
	for range 31 {
		_, release, err := r.beginConsoleJob(context.Background(), "second-owner")
		if err != nil {
			t.Fatal(err)
		}
		defer release()
	}
	_, _, err = r.beginConsoleJob(context.Background(), "second-owner")
	requireStatus(t, err, 429)
	r.RevokeConsoleSession("second-owner")
	select {
	case <-second.Done():
	default:
		t.Fatal("second revocation left its job active")
	}
}
