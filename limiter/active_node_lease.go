package limiter

import (
	"strconv"
	"sync"
	"time"
)

var noopActiveNodeLeaseRelease = func() {}

type activeNodeLease struct {
	identity            onlineIPIdentity
	ip                  string
	nodeID              string
	limit               int
	observedAt          time.Time
	admittedAtUnixMicro int64
	admitted            bool
	nextCallbackID      uint64
	callbacks           map[uint64]func()
}

type activeNodeLeaseTracker struct {
	store   activeNodeStore
	nodeID  string
	mu      sync.Mutex
	leases  map[string]*activeNodeLease
	stop    chan struct{}
	done    chan struct{}
	stopped bool
}

func newActiveNodeLeaseTracker(store activeNodeStore, nodeID string) *activeNodeLeaseTracker {
	if store == nil || store.RenewInterval() <= 0 || nodeID == "" {
		return nil
	}
	tracker := &activeNodeLeaseTracker{
		store:  store,
		nodeID: nodeID,
		leases: make(map[string]*activeNodeLease),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go tracker.run(tracker.tickInterval())
	return tracker
}

func (t *activeNodeLeaseTracker) Acquire(identity onlineIPIdentity, ip string, limit int, disconnect func()) func() {
	if t == nil || limit <= 0 {
		return noopActiveNodeLeaseRelease
	}
	if disconnect == nil {
		disconnect = noopActiveNodeLeaseRelease
	}

	ip = t.store.NormalizeIP(ip)
	key := activeNodeLeaseMapKey(identity, ip, t.nodeID)
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return noopActiveNodeLeaseRelease
	}
	if lease, ok := t.leases[key]; ok {
		lease.limit = limit
		callbackID := lease.nextCallbackID
		lease.nextCallbackID++
		lease.callbacks[callbackID] = disconnect
		t.mu.Unlock()
		return activeNodeReleaseFunc(t, key, callbackID)
	}
	t.mu.Unlock()

	decision := t.store.Check(identity, ip, t.nodeID, limit)
	if decision == activeNodeRejected {
		disconnect()
		return noopActiveNodeLeaseRelease
	}
	admittedAtUnixMicro := time.Now().UnixMicro()
	if decision == activeNodeAdmitted && !t.store.Renew(identity, ip, t.nodeID, limit, admittedAtUnixMicro) {
		disconnect()
		return noopActiveNodeLeaseRelease
	}

	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		if decision == activeNodeAdmitted {
			t.store.Release(identity, ip, t.nodeID)
		}
		return noopActiveNodeLeaseRelease
	}
	lease, ok := t.leases[key]
	if !ok {
		now := time.Now()
		lease = &activeNodeLease{
			identity:            identity,
			ip:                  ip,
			nodeID:              t.nodeID,
			limit:               limit,
			observedAt:          now,
			admittedAtUnixMicro: admittedAtUnixMicro,
			admitted:            decision == activeNodeAdmitted,
			callbacks:           make(map[uint64]func()),
		}
		t.leases[key] = lease
	} else {
		lease.limit = limit
		if decision == activeNodeAdmitted {
			lease.admitted = true
		}
	}
	callbackID := lease.nextCallbackID
	lease.nextCallbackID++
	lease.callbacks[callbackID] = disconnect
	t.mu.Unlock()
	return activeNodeReleaseFunc(t, key, callbackID)
}

func activeNodeReleaseFunc(t *activeNodeLeaseTracker, key string, callbackID uint64) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			t.release(key, callbackID)
		})
	}
}

func (t *activeNodeLeaseTracker) release(key string, callbackID uint64) {
	t.mu.Lock()
	lease, ok := t.leases[key]
	if !ok {
		t.mu.Unlock()
		return
	}
	delete(lease.callbacks, callbackID)
	if len(lease.callbacks) != 0 {
		t.mu.Unlock()
		return
	}
	delete(t.leases, key)
	t.mu.Unlock()
	if lease.admitted {
		t.store.Release(lease.identity, lease.ip, lease.nodeID)
	}
}

func (t *activeNodeLeaseTracker) UpdateLimit(identity onlineIPIdentity, limit int) {
	if t == nil {
		return
	}
	t.mu.Lock()
	var removed []activeNodeLease
	for key, lease := range t.leases {
		if lease.identity != identity {
			continue
		}
		if limit > 0 {
			lease.limit = limit
			continue
		}
		delete(t.leases, key)
		if lease.admitted {
			removed = append(removed, *lease)
		}
	}
	t.mu.Unlock()
	for _, lease := range removed {
		t.store.Release(lease.identity, lease.ip, lease.nodeID)
	}
}

func (t *activeNodeLeaseTracker) tickInterval() time.Duration {
	interval := t.store.RenewInterval()
	delay := t.store.ActivationDelay()
	if delay > 0 && delay < interval {
		return delay
	}
	return interval
}

func (t *activeNodeLeaseTracker) run(interval time.Duration) {
	defer close(t.done)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case now := <-ticker.C:
			t.process(now)
		case <-t.stop:
			return
		}
	}
}

func (t *activeNodeLeaseTracker) process(now time.Time) {
	leases := t.snapshot()
	for _, lease := range leases {
		if lease.admitted {
			if !t.store.Renew(lease.identity, lease.ip, lease.nodeID, lease.limit, lease.admittedAtUnixMicro) {
				t.disconnect(activeNodeLeaseMapKey(lease.identity, lease.ip, lease.nodeID))
			}
			continue
		}
		if now.Sub(lease.observedAt) < t.store.ActivationDelay() {
			continue
		}
		switch t.store.Activate(lease.identity, lease.ip, lease.nodeID, lease.limit, lease.admittedAtUnixMicro) {
		case activeNodeAdmitted:
			t.markAdmitted(activeNodeLeaseMapKey(lease.identity, lease.ip, lease.nodeID), lease.identity, lease.ip, lease.nodeID)
		case activeNodeRejected:
			t.disconnect(activeNodeLeaseMapKey(lease.identity, lease.ip, lease.nodeID))
		}
	}
}

func (t *activeNodeLeaseTracker) snapshot() []activeNodeLease {
	t.mu.Lock()
	defer t.mu.Unlock()
	leases := make([]activeNodeLease, 0, len(t.leases))
	for _, lease := range t.leases {
		leases = append(leases, activeNodeLease{
			identity:            lease.identity,
			ip:                  lease.ip,
			nodeID:              lease.nodeID,
			limit:               lease.limit,
			observedAt:          lease.observedAt,
			admittedAtUnixMicro: lease.admittedAtUnixMicro,
			admitted:            lease.admitted,
		})
	}
	return leases
}

func (t *activeNodeLeaseTracker) markAdmitted(key string, identity onlineIPIdentity, ip string, nodeID string) {
	t.mu.Lock()
	lease, ok := t.leases[key]
	if ok && len(lease.callbacks) > 0 {
		lease.admitted = true
		t.mu.Unlock()
		return
	}
	t.mu.Unlock()
	t.store.Release(identity, ip, nodeID)
}

func (t *activeNodeLeaseTracker) disconnect(key string) {
	t.mu.Lock()
	lease, ok := t.leases[key]
	if ok {
		delete(t.leases, key)
	}
	t.mu.Unlock()
	if !ok {
		return
	}
	for _, disconnect := range lease.callbacks {
		disconnect()
	}
}

func (t *activeNodeLeaseTracker) Close() {
	if t == nil {
		return
	}
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return
	}
	t.stopped = true
	close(t.stop)
	var admitted []activeNodeLease
	for _, lease := range t.leases {
		if lease.admitted {
			admitted = append(admitted, *lease)
		}
	}
	t.leases = make(map[string]*activeNodeLease)
	t.mu.Unlock()
	<-t.done
	for _, lease := range admitted {
		t.store.Release(lease.identity, lease.ip, lease.nodeID)
	}
}

func activeNodeLeaseMapKey(identity onlineIPIdentity, ip string, nodeID string) string {
	if identity.UID != 0 {
		return strconv.Itoa(identity.UID) + "|" + ip + "|" + nodeID
	}
	return identity.UUID + "|" + ip + "|" + nodeID
}
