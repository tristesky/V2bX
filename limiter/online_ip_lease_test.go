package limiter

import (
	"sync"
	"testing"
	"time"
)

type fakeOnlineIPLeaseStore struct {
	mu          sync.Mutex
	interval    time.Duration
	rejectRenew bool
	renews      []onlineIPLease
}

func (s *fakeOnlineIPLeaseStore) Allow(identity onlineIPIdentity, ip string, limit int) bool {
	return true
}

func (s *fakeOnlineIPLeaseStore) Renew(identity onlineIPIdentity, ip string, limit int, admittedAtUnixMicro int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renews = append(s.renews, onlineIPLease{identity: identity, ip: ip, limit: limit, admittedAtUnixMicro: admittedAtUnixMicro})
	return !s.rejectRenew
}

func (s *fakeOnlineIPLeaseStore) RenewInterval() time.Duration {
	return s.interval
}

func (s *fakeOnlineIPLeaseStore) Close() error {
	return nil
}

func (s *fakeOnlineIPLeaseStore) Renewals() []onlineIPLease {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]onlineIPLease(nil), s.renews...)
}

func TestOnlineIPLeaseTrackerRenewsWhileReferencesRemain(t *testing.T) {
	store := &fakeOnlineIPLeaseStore{interval: time.Hour}
	tracker := newOnlineIPLeaseTracker(store)
	defer tracker.Close()

	identity := onlineIPIdentity{UID: 7, UUID: "uuid-7"}
	releaseFirst := tracker.Acquire(identity, "192.0.2.7", 3, func() {})
	releaseSecond := tracker.Acquire(identity, "192.0.2.7", 3, func() {})

	tracker.renewAll()
	if renewals := store.Renewals(); len(renewals) != 1 {
		t.Fatalf("renewals after acquiring duplicate leases = %d, want 1", len(renewals))
	}

	releaseFirst()
	releaseFirst()
	tracker.renewAll()
	if renewals := store.Renewals(); len(renewals) != 2 {
		t.Fatalf("renewals while one reference remains = %d, want 2", len(renewals))
	}

	releaseSecond()
	tracker.renewAll()
	if renewals := store.Renewals(); len(renewals) != 2 {
		t.Fatalf("renewals after all releases = %d, want 2", len(renewals))
	}
}

func TestOnlineIPLeaseTrackerDisconnectsRejectedRenewal(t *testing.T) {
	store := &fakeOnlineIPLeaseStore{interval: time.Hour, rejectRenew: true}
	tracker := newOnlineIPLeaseTracker(store)
	defer tracker.Close()

	var disconnected int
	tracker.Acquire(onlineIPIdentity{UID: 9}, "192.0.2.9", 1, func() {
		disconnected++
	})

	tracker.renewAll()
	if disconnected != 1 {
		t.Fatalf("disconnect callbacks after rejected renewal = %d, want 1", disconnected)
	}

	tracker.renewAll()
	if renewals := store.Renewals(); len(renewals) != 1 {
		t.Fatalf("renewals after rejected lease removal = %d, want 1", len(renewals))
	}
}

func TestOnlineIPLeaseTrackerOnlineDevices(t *testing.T) {
	store := &fakeOnlineIPLeaseStore{interval: time.Hour}
	tracker := newOnlineIPLeaseTracker(store)
	defer tracker.Close()

	release := tracker.Acquire(onlineIPIdentity{UID: 11}, "192.0.2.11", 2, nil)
	devices := tracker.OnlineDevices()
	if len(devices) != 1 {
		t.Fatalf("online devices = %d, want 1", len(devices))
	}
	if devices[0].UID != 11 || devices[0].IP != "192.0.2.11" {
		t.Fatalf("online device = %+v, want UID 11 IP 192.0.2.11", devices[0])
	}

	release()
	if devices := tracker.OnlineDevices(); len(devices) != 0 {
		t.Fatalf("online devices after release = %d, want 0", len(devices))
	}
}
