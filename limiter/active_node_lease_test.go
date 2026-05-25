package limiter

import (
	"sync"
	"testing"
	"time"
)

type fakeActiveNodeStore struct {
	mu               sync.Mutex
	delay            time.Duration
	interval         time.Duration
	checkDecision    activeNodeDecision
	activateDecision activeNodeDecision
	renewAllowed     bool
	onActivate       func()
	checks           int
	activates        int
	renews           int
	releases         int
}

func (s *fakeActiveNodeStore) NormalizeIP(ip string) string { return ip }

func (s *fakeActiveNodeStore) Check(identity onlineIPIdentity, ip string, nodeID string, limit int) activeNodeDecision {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.checks++
	return s.checkDecision
}

func (s *fakeActiveNodeStore) Activate(identity onlineIPIdentity, ip string, nodeID string, limit int, admittedAtUnixMicro int64) activeNodeDecision {
	s.mu.Lock()
	s.activates++
	decision := s.activateDecision
	hook := s.onActivate
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return decision
}

func (s *fakeActiveNodeStore) Renew(identity onlineIPIdentity, ip string, nodeID string, limit int, admittedAtUnixMicro int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.renews++
	return s.renewAllowed
}

func (s *fakeActiveNodeStore) Release(identity onlineIPIdentity, ip string, nodeID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releases++
}

func (s *fakeActiveNodeStore) ActivationDelay() time.Duration { return s.delay }
func (s *fakeActiveNodeStore) RenewInterval() time.Duration   { return s.interval }
func (s *fakeActiveNodeStore) Close() error                   { return nil }

func (s *fakeActiveNodeStore) counts() (activates, renews, releases int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.activates, s.renews, s.releases
}

func TestActiveNodeShortConnectionDoesNotConsumeFormalLease(t *testing.T) {
	store := &fakeActiveNodeStore{
		delay:            time.Minute,
		interval:         time.Minute,
		checkDecision:    activeNodeObserve,
		activateDecision: activeNodeAdmitted,
		renewAllowed:     true,
	}
	tracker := newActiveNodeLeaseTracker(store, "us-1")
	defer tracker.Close()

	release := tracker.Acquire(onlineIPIdentity{UID: 1}, "192.0.2.1", 3, nil)
	release()
	tracker.process(time.Now().Add(2 * time.Minute))
	activates, _, releases := store.counts()
	if activates != 0 || releases != 0 {
		t.Fatalf("short connection activated=%d released=%d, want no Redis formal lease operations", activates, releases)
	}
}

func TestActiveNodeGroupsConnectionsAndActivatesOnce(t *testing.T) {
	store := &fakeActiveNodeStore{
		delay:            time.Minute,
		interval:         time.Minute,
		checkDecision:    activeNodeObserve,
		activateDecision: activeNodeAdmitted,
		renewAllowed:     true,
	}
	tracker := newActiveNodeLeaseTracker(store, "us-1")
	defer tracker.Close()

	identity := onlineIPIdentity{UID: 2}
	releaseFirst := tracker.Acquire(identity, "192.0.2.2", 3, nil)
	releaseSecond := tracker.Acquire(identity, "192.0.2.2", 3, nil)
	tracker.process(time.Now().Add(2 * time.Minute))
	activates, _, _ := store.counts()
	if activates != 1 {
		t.Fatalf("activations on same node = %d, want 1", activates)
	}
	releaseFirst()
	_, _, releases := store.counts()
	if releases != 0 {
		t.Fatalf("releases while connection remains = %d, want 0", releases)
	}
	releaseSecond()
	_, _, releases = store.counts()
	if releases != 1 {
		t.Fatalf("releases after final connection closes = %d, want 1", releases)
	}
}

func TestActiveNodeSeparatesConnectionsFromDifferentIPs(t *testing.T) {
	store := &fakeActiveNodeStore{
		delay:            time.Minute,
		interval:         time.Minute,
		checkDecision:    activeNodeObserve,
		activateDecision: activeNodeAdmitted,
		renewAllowed:     true,
	}
	tracker := newActiveNodeLeaseTracker(store, "us-1")
	defer tracker.Close()

	identity := onlineIPIdentity{UID: 2}
	tracker.Acquire(identity, "192.0.2.2", 3, nil)
	tracker.Acquire(identity, "198.51.100.2", 3, nil)
	tracker.process(time.Now().Add(2 * time.Minute))
	activates, _, _ := store.counts()
	if activates != 2 {
		t.Fatalf("activations from separate IPs = %d, want 2", activates)
	}
}

func TestActiveNodeRedisKeyGroupsByNormalizedSourceIP(t *testing.T) {
	store := &redisActiveNodeStore{
		keyPrefix:  "v2bx:same_ip_active_node",
		scopeHash:  "scope",
		ipv6Prefix: 64,
	}
	identity := onlineIPIdentity{UID: 8}
	if store.redisKey(identity, "192.0.2.8") == store.redisKey(identity, "198.51.100.8") {
		t.Fatal("different IPv4 addresses must not share an active node key")
	}
	if store.redisKey(identity, "2001:db8::1") != store.redisKey(identity, "2001:db8::2") {
		t.Fatal("IPv6 addresses in the configured /64 must share an active node key")
	}
}

func TestActiveNodeRejectedAfterObservationDisconnectsConnections(t *testing.T) {
	store := &fakeActiveNodeStore{
		delay:            time.Minute,
		interval:         time.Minute,
		checkDecision:    activeNodeObserve,
		activateDecision: activeNodeRejected,
		renewAllowed:     true,
	}
	tracker := newActiveNodeLeaseTracker(store, "us-4")
	defer tracker.Close()

	var disconnected int
	identity := onlineIPIdentity{UID: 4}
	tracker.Acquire(identity, "192.0.2.4", 3, func() { disconnected++ })
	tracker.Acquire(identity, "192.0.2.4", 3, func() { disconnected++ })
	tracker.process(time.Now().Add(2 * time.Minute))
	if disconnected != 2 {
		t.Fatalf("disconnected callbacks = %d, want 2", disconnected)
	}
}

func TestActiveNodeReleaseDuringActivationRemovesFormalLease(t *testing.T) {
	store := &fakeActiveNodeStore{
		delay:            time.Minute,
		interval:         time.Minute,
		checkDecision:    activeNodeObserve,
		activateDecision: activeNodeAdmitted,
		renewAllowed:     true,
	}
	tracker := newActiveNodeLeaseTracker(store, "us-5")
	defer tracker.Close()

	var release func()
	release = tracker.Acquire(onlineIPIdentity{UID: 5}, "192.0.2.5", 3, nil)
	store.onActivate = func() { release() }
	tracker.process(time.Now().Add(2 * time.Minute))
	activates, _, releases := store.counts()
	if activates != 1 || releases != 1 {
		t.Fatalf("activation race activated=%d released=%d, want one activation followed by release", activates, releases)
	}
}

func TestActiveNodeAlreadyAdmittedRenewsWithoutObservationDelay(t *testing.T) {
	store := &fakeActiveNodeStore{
		delay:         time.Minute,
		interval:      time.Minute,
		checkDecision: activeNodeAdmitted,
		renewAllowed:  true,
	}
	tracker := newActiveNodeLeaseTracker(store, "us-2")
	defer tracker.Close()

	tracker.Acquire(onlineIPIdentity{UID: 3}, "192.0.2.3", 3, nil)
	tracker.process(time.Now())
	activates, renews, _ := store.counts()
	if activates != 0 || renews != 2 {
		t.Fatalf("admitted connection activated=%d renewed=%d, want activated=0 renewed=2", activates, renews)
	}
}

func TestActiveNodeCloseReleasesAdmittedLeases(t *testing.T) {
	store := &fakeActiveNodeStore{
		delay:         time.Minute,
		interval:      time.Minute,
		checkDecision: activeNodeAdmitted,
		renewAllowed:  true,
	}
	tracker := newActiveNodeLeaseTracker(store, "us-3")
	tracker.Acquire(onlineIPIdentity{UID: 6}, "192.0.2.6", 3, nil)
	tracker.Close()
	_, _, releases := store.counts()
	if releases != 1 {
		t.Fatalf("releases on tracker close = %d, want 1", releases)
	}
}
