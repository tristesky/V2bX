package limiter

import (
	"strconv"
	"sync"
	"time"
)

var noopOnlineIPLeaseRelease = func() {}

type onlineIPLease struct {
	identity            onlineIPIdentity
	ip                  string
	limit               int
	admittedAtUnixMicro int64
	nextCallbackID      uint64
	callbacks           map[uint64]func()
}

type onlineIPLeaseTracker struct {
	store   onlineIPStore
	mu      sync.Mutex
	leases  map[string]*onlineIPLease
	stop    chan struct{}
	done    chan struct{}
	stopped bool
}

func newOnlineIPLeaseTracker(store onlineIPStore) *onlineIPLeaseTracker {
	if store == nil || store.RenewInterval() <= 0 {
		return nil
	}

	t := &onlineIPLeaseTracker{
		store:  store,
		leases: make(map[string]*onlineIPLease),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	go t.run(store.RenewInterval())
	return t
}

func (t *onlineIPLeaseTracker) Acquire(identity onlineIPIdentity, ip string, limit int, disconnect func()) func() {
	if t == nil {
		return noopOnlineIPLeaseRelease
	}

	key := onlineIPLeaseMapKey(identity, ip)
	t.mu.Lock()
	if t.stopped {
		t.mu.Unlock()
		return noopOnlineIPLeaseRelease
	}
	lease, ok := t.leases[key]
	if !ok {
		lease = &onlineIPLease{
			identity:            identity,
			ip:                  ip,
			limit:               limit,
			admittedAtUnixMicro: time.Now().UnixMicro(),
			callbacks:           make(map[uint64]func()),
		}
		t.leases[key] = lease
	}
	if limit > 0 && (lease.limit == 0 || limit < lease.limit) {
		lease.limit = limit
	}
	callbackID := lease.nextCallbackID
	lease.nextCallbackID++
	if disconnect == nil {
		disconnect = noopOnlineIPLeaseRelease
	}
	lease.callbacks[callbackID] = disconnect
	t.mu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			t.release(key, callbackID)
		})
	}
}

func (t *onlineIPLeaseTracker) release(key string, callbackID uint64) {
	t.mu.Lock()
	defer t.mu.Unlock()

	lease, ok := t.leases[key]
	if !ok {
		return
	}
	delete(lease.callbacks, callbackID)
	if len(lease.callbacks) == 0 {
		delete(t.leases, key)
	}
}

func (t *onlineIPLeaseTracker) run(interval time.Duration) {
	defer close(t.done)

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			t.renewAll()
		case <-t.stop:
			return
		}
	}
}

func (t *onlineIPLeaseTracker) renewAll() {
	leases := t.snapshot()
	for _, lease := range leases {
		if !t.store.Renew(lease.identity, lease.ip, lease.limit, lease.admittedAtUnixMicro) {
			t.disconnect(onlineIPLeaseMapKey(lease.identity, lease.ip))
		}
	}
}

func (t *onlineIPLeaseTracker) snapshot() []onlineIPLease {
	t.mu.Lock()
	defer t.mu.Unlock()

	leases := make([]onlineIPLease, 0, len(t.leases))
	for _, lease := range t.leases {
		leases = append(leases, onlineIPLease{
			identity:            lease.identity,
			ip:                  lease.ip,
			limit:               lease.limit,
			admittedAtUnixMicro: lease.admittedAtUnixMicro,
		})
	}
	return leases
}

func (t *onlineIPLeaseTracker) disconnect(key string) {
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

func (t *onlineIPLeaseTracker) Close() {
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
	t.mu.Unlock()

	<-t.done
}

func onlineIPLeaseMapKey(identity onlineIPIdentity, ip string) string {
	if identity.UID != 0 {
		return strconv.Itoa(identity.UID) + "|" + ip
	}
	return identity.UUID + "|" + ip
}
