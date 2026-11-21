package main

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const testEtcdKey = "/test/vip/lock"

// fakeWatch is one call of fakeWatcher.Watch.
type fakeWatch struct {
	rev int64
	ch  chan clientv3.WatchResponse
}

// fakeWatcher hands each Watch call to the test through watches.
type fakeWatcher struct {
	clientv3.Watcher
	watches chan *fakeWatch
}

func newFakeWatcher() *fakeWatcher {
	return &fakeWatcher{watches: make(chan *fakeWatch, 16)}
}

func (w *fakeWatcher) Watch(ctx context.Context, key string, opts ...clientv3.OpOption) clientv3.WatchChan {
	fw := &fakeWatch{
		rev: clientv3.OpGet(key, opts...).Rev(),
		ch:  make(chan clientv3.WatchResponse, 4),
	}
	w.watches <- fw
	return fw.ch
}

// next returns the next Watch call.
func (w *fakeWatcher) next(t *testing.T) *fakeWatch {
	t.Helper()
	select {
	case fw := <-w.watches:
		return fw
	case <-time.After(5 * time.Second):
		t.Fatal("expected a Watch call")
		return nil
	}
}

func (w *fakeWatcher) expectNoWatch(t *testing.T) {
	t.Helper()
	select {
	case fw := <-w.watches:
		t.Fatalf("unexpected Watch call from revision %d", fw.rev)
	case <-time.After(50 * time.Millisecond):
	}
}

// fakeKV answers Get with a script and records transactions.
type fakeKV struct {
	clientv3.KV
	mu sync.Mutex
	// get returns the answer to the n-th Get call (n starts at 0).
	get  func(n int) (*clientv3.GetResponse, error)
	gets int
	// noDeadline counts requests that carried no deadline.
	noDeadline   int
	txnSucceeded bool
	txnRev       int64
	txnErr       error
	txnTimes     []time.Time
	puts         []string
}

func (kv *fakeKV) Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	kv.mu.Lock()
	n := kv.gets
	kv.gets++
	if _, ok := ctx.Deadline(); !ok {
		kv.noDeadline++
	}
	get := kv.get
	kv.mu.Unlock()
	return get(n)
}

func (kv *fakeKV) Txn(ctx context.Context) clientv3.Txn {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	if _, ok := ctx.Deadline(); !ok {
		kv.noDeadline++
	}
	return &fakeTxn{kv: kv}
}

func (kv *fakeKV) getCount() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return kv.gets
}

func (kv *fakeKV) txnCount() int {
	kv.mu.Lock()
	defer kv.mu.Unlock()
	return len(kv.txnTimes)
}

type fakeTxn struct {
	kv  *fakeKV
	ops []clientv3.Op
}

func (t *fakeTxn) If(...clientv3.Cmp) clientv3.Txn  { return t }
func (t *fakeTxn) Else(...clientv3.Op) clientv3.Txn { return t }
func (t *fakeTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	t.ops = append(t.ops, ops...)
	return t
}

func (t *fakeTxn) Commit() (*clientv3.TxnResponse, error) {
	t.kv.mu.Lock()
	defer t.kv.mu.Unlock()
	t.kv.txnTimes = append(t.kv.txnTimes, time.Now())
	for _, op := range t.ops {
		if op.IsPut() {
			t.kv.puts = append(t.kv.puts, string(op.ValueBytes()))
		}
	}
	if t.kv.txnErr != nil {
		return nil, t.kv.txnErr
	}
	return &clientv3.TxnResponse{Header: &pb.ResponseHeader{Revision: t.kv.txnRev}, Succeeded: t.kv.txnSucceeded}, nil
}

func getResp(rev int64, kvs ...*mvccpb.KeyValue) *clientv3.GetResponse {
	return &clientv3.GetResponse{Header: &pb.ResponseHeader{Revision: rev}, Kvs: kvs}
}

func keyValue(lease clientv3.LeaseID, value string) *mvccpb.KeyValue {
	return &mvccpb.KeyValue{Key: []byte(testEtcdKey), Value: []byte(value), Lease: int64(lease)}
}

// always answers every Get the same way.
func always(resp *clientv3.GetResponse, err error) func(int) (*clientv3.GetResponse, error) {
	return func(int) (*clientv3.GetResponse, error) { return resp, err }
}

// sequence answers the n-th Get with the n-th entry and repeats the last.
func sequence(resps ...*clientv3.GetResponse) func(int) (*clientv3.GetResponse, error) {
	return func(n int) (*clientv3.GetResponse, error) {
		if n >= len(resps) {
			n = len(resps) - 1
		}
		return resps[n], nil
	}
}

func putEvent(lease clientv3.LeaseID, value string) clientv3.WatchResponse {
	return clientv3.WatchResponse{Events: []*clientv3.Event{{Type: mvccpb.PUT, Kv: keyValue(lease, value)}}}
}

func deleteEvent() clientv3.WatchResponse {
	return clientv3.WatchResponse{Events: []*clientv3.Event{{Type: mvccpb.DELETE, Kv: &mvccpb.KeyValue{Key: []byte(testEtcdKey)}}}}
}

// fakeLease grants consecutive lease IDs, answers renewals, and records
// grants and revokes in order.
type fakeLease struct {
	clientv3.Lease
	mu         sync.Mutex
	calls      []string
	nextID     clientv3.LeaseID
	grantTTL   int64 // TTL to grant; 0 grants the requested one
	grantErr   error
	revokeErrs []error // answers to Revoke calls in order; nil after that
	// renew answers the n-th KeepAliveOnce call (n starts at 0); nil
	// renews successfully unless the lease is gone.
	renew func(ctx context.Context, n int) (*clientv3.LeaseKeepAliveResponse, error)
	// gone holds leases etcd reports as not found.
	gone          map[clientv3.LeaseID]bool
	renews        int
	renewTimes    []time.Time
	renewDeadline []time.Duration // time left until each call's deadline
}

// markGone makes renewals of the lease report it as not found.
func (l *fakeLease) markGone(id clientv3.LeaseID) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.gone == nil {
		l.gone = map[clientv3.LeaseID]bool{}
	}
	l.gone[id] = true
}

func (l *fakeLease) renewCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.renews
}

func (l *fakeLease) record(format string, args ...any) {
	l.calls = append(l.calls, fmt.Sprintf(format, args...))
}

func (l *fakeLease) Grant(ctx context.Context, ttl int64) (*clientv3.LeaseGrantResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.grantErr != nil {
		l.record("grant failed")
		return nil, l.grantErr
	}
	l.nextID++
	l.record("grant %d", l.nextID)
	granted := ttl
	if l.grantTTL != 0 {
		granted = l.grantTTL
	}
	return &clientv3.LeaseGrantResponse{ID: l.nextID, TTL: granted}, nil
}

func (l *fakeLease) KeepAliveOnce(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseKeepAliveResponse, error) {
	l.mu.Lock()
	n := l.renews
	l.renews++
	l.renewTimes = append(l.renewTimes, time.Now())
	left := time.Duration(-1)
	if deadline, ok := ctx.Deadline(); ok {
		left = time.Until(deadline)
	}
	l.renewDeadline = append(l.renewDeadline, left)
	gone, renew := l.gone[id], l.renew
	l.mu.Unlock()

	switch {
	case gone:
		return nil, rpctypes.ErrLeaseNotFound
	case renew != nil:
		return renew(ctx, n)
	default:
		return &clientv3.LeaseKeepAliveResponse{ID: id, TTL: 10}, nil
	}
}

func (l *fakeLease) Revoke(ctx context.Context, id clientv3.LeaseID) (*clientv3.LeaseRevokeResponse, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	var err error
	if len(l.revokeErrs) > 0 {
		err, l.revokeErrs = l.revokeErrs[0], l.revokeErrs[1:]
	}
	if err != nil {
		l.record("revoke %d failed", id)
		return nil, err
	}
	l.record("revoke %d", id)
	return &clientv3.LeaseRevokeResponse{}, nil
}

func (l *fakeLease) callLog() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.calls)
}

// waitForCall waits until the call log contains the call.
func (l *fakeLease) waitForCall(t *testing.T, call string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !slices.Contains(l.callLog(), call) {
		if time.Now().After(deadline) {
			t.Fatalf("expected call %q, got %v", call, l.callLog())
		}
		time.Sleep(time.Millisecond)
	}
}

func cancelableContext(t *testing.T) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx, cancel
}

// result runs f in a goroutine and returns a channel with its error.
func result(f func() error) <-chan error {
	ch := make(chan error, 1)
	go func() { ch <- f() }()
	return ch
}

func expectResult(t *testing.T, ch <-chan error) error {
	t.Helper()
	select {
	case err := <-ch:
		return err
	case <-time.After(5 * time.Second):
		t.Fatal("expected the call to return")
		return nil
	}
}

func expectBlocked(t *testing.T, ch <-chan error) {
	t.Helper()
	select {
	case err := <-ch:
		t.Fatalf("expected the call to keep waiting, it returned %v", err)
	case <-time.After(50 * time.Millisecond):
	}
}

func keyGone(s keyState) bool { return !s.exists }

func TestWaitForKey_ReturnsOnMatchingEvent(t *testing.T) {
	w := newFakeWatcher()
	e := &etcdElector{watcher: w, key: testEtcdKey}
	ctx, _ := cancelableContext(t)

	res := result(func() error { return e.waitForKey(ctx, 10, nil, keyGone) })

	fw := w.next(t)
	if fw.rev != 11 {
		t.Errorf("expected the watch to start after the read revision (11), got %d", fw.rev)
	}
	fw.ch <- putEvent(7, "other")
	expectBlocked(t, res)

	fw.ch <- deleteEvent()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestWaitForKey_RereadsAfterCompaction(t *testing.T) {
	w := newFakeWatcher()
	kv := &fakeKV{get: always(getResp(20, keyValue(7, "other")), nil)}
	e := &etcdElector{watcher: w, kv: kv, key: testEtcdKey}
	ctx, _ := cancelableContext(t)

	res := result(func() error { return e.waitForKey(ctx, 10, nil, keyGone) })

	// etcd cancels a watch whose start revision was compacted.
	w.next(t).ch <- clientv3.WatchResponse{CompactRevision: 15, Canceled: true}

	fw := w.next(t)
	if fw.rev != 21 {
		t.Errorf("expected the new watch to start after the re-read revision (21), got %d", fw.rev)
	}
	if kv.getCount() != 1 {
		t.Errorf("expected one read, got %d", kv.getCount())
	}
	fw.ch <- deleteEvent()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestWaitForKey_ReadCanSatisfyCondition(t *testing.T) {
	w := newFakeWatcher()
	kv := &fakeKV{get: always(getResp(20), nil)}
	e := &etcdElector{watcher: w, kv: kv, key: testEtcdKey}
	ctx, _ := cancelableContext(t)

	res := result(func() error { return e.waitForKey(ctx, 10, nil, keyGone) })

	// The watch ends (e.g. the member lost its leader); the key was deleted
	// meanwhile, so the read already answers.
	close(w.next(t).ch)
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
	w.expectNoWatch(t)
}

func TestWaitForKey_RetriesFailedReads(t *testing.T) {
	old := etcdRereadInterval
	etcdRereadInterval = time.Millisecond
	t.Cleanup(func() { etcdRereadInterval = old })

	w := newFakeWatcher()
	kv := &fakeKV{get: func(n int) (*clientv3.GetResponse, error) {
		if n < 2 {
			return nil, errors.New("etcd unreachable")
		}
		return getResp(30, keyValue(7, "other")), nil
	}}
	e := &etcdElector{watcher: w, kv: kv, key: testEtcdKey}
	ctx, _ := cancelableContext(t)

	res := result(func() error { return e.waitForKey(ctx, 10, nil, keyGone) })

	close(w.next(t).ch)
	fw := w.next(t)
	if fw.rev != 31 {
		t.Errorf("expected the new watch to start at 31, got %d", fw.rev)
	}
	if kv.getCount() != 3 {
		t.Errorf("expected 3 reads, got %d", kv.getCount())
	}
	fw.ch <- deleteEvent()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestWaitForKey_LeaseLost(t *testing.T) {
	w := newFakeWatcher()
	e := &etcdElector{watcher: w, key: testEtcdKey}
	ctx, _ := cancelableContext(t)
	leaseLost := make(chan struct{})

	res := result(func() error { return e.waitForKey(ctx, 10, leaseLost, keyGone) })
	w.next(t)
	close(leaseLost)

	if err := expectResult(t, res); !errors.Is(err, errLeaseLost) {
		t.Errorf("expected errLeaseLost, got %v", err)
	}
}

func TestWaitForKey_LeaseLostWhileRereading(t *testing.T) {
	old := etcdRereadInterval
	etcdRereadInterval = time.Millisecond
	t.Cleanup(func() { etcdRereadInterval = old })

	w := newFakeWatcher()
	kv := &fakeKV{get: always(nil, errors.New("etcd unreachable"))}
	e := &etcdElector{watcher: w, kv: kv, key: testEtcdKey}
	ctx, _ := cancelableContext(t)
	leaseLost := make(chan struct{})

	res := result(func() error { return e.waitForKey(ctx, 10, leaseLost, keyGone) })
	close(w.next(t).ch)
	expectBlocked(t, res)
	close(leaseLost)

	if err := expectResult(t, res); !errors.Is(err, errLeaseLost) {
		t.Errorf("expected errLeaseLost, got %v", err)
	}
}

func TestWaitForKey_ContextCancelled(t *testing.T) {
	w := newFakeWatcher()
	e := &etcdElector{watcher: w, key: testEtcdKey}
	ctx, cancel := cancelableContext(t)

	res := result(func() error { return e.waitForKey(ctx, 10, nil, keyGone) })
	w.next(t)
	cancel()

	if err := expectResult(t, res); !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestHoldLock(t *testing.T) {
	const ours clientv3.LeaseID = 5

	t.Run("key taken over by another lease", func(t *testing.T) {
		w := newFakeWatcher()
		e := &etcdElector{watcher: w, key: testEtcdKey}
		ctx, _ := cancelableContext(t)

		res := result(func() error { return e.holdLock(ctx, ours, 10, nil) })
		fw := w.next(t)
		// A write with our own lease keeps the lock.
		fw.ch <- putEvent(ours, "node1")
		expectBlocked(t, res)
		fw.ch <- putEvent(7, "intruder")

		if err := expectResult(t, res); !errors.Is(err, errLeadershipLost) {
			t.Errorf("expected errLeadershipLost, got %v", err)
		}
	})

	t.Run("key deleted", func(t *testing.T) {
		w := newFakeWatcher()
		e := &etcdElector{watcher: w, key: testEtcdKey}
		ctx, _ := cancelableContext(t)

		res := result(func() error { return e.holdLock(ctx, ours, 10, nil) })
		w.next(t).ch <- deleteEvent()

		if err := expectResult(t, res); !errors.Is(err, errLeadershipLost) {
			t.Errorf("expected errLeadershipLost, got %v", err)
		}
	})

	t.Run("lease lost", func(t *testing.T) {
		w := newFakeWatcher()
		e := &etcdElector{watcher: w, key: testEtcdKey}
		ctx, _ := cancelableContext(t)
		leaseLost := make(chan struct{})

		res := result(func() error { return e.holdLock(ctx, ours, 10, leaseLost) })
		w.next(t)
		close(leaseLost)

		if err := expectResult(t, res); !errors.Is(err, errLeadershipLost) {
			t.Errorf("expected errLeadershipLost, got %v", err)
		}
	})

	t.Run("shutdown", func(t *testing.T) {
		w := newFakeWatcher()
		e := &etcdElector{watcher: w, key: testEtcdKey}
		ctx, cancel := cancelableContext(t)

		res := result(func() error { return e.holdLock(ctx, ours, 10, nil) })
		w.next(t)
		cancel()

		if err := expectResult(t, res); err != nil {
			t.Errorf("expected nil on shutdown, got %v", err)
		}
	})
}

// shortRenewRetry makes failed renewals retry at once.
func shortRenewRetry(t *testing.T) {
	old := leaseRenewRetryInterval
	leaseRenewRetryInterval = time.Millisecond
	t.Cleanup(func() { leaseRenewRetryInterval = old })
}

func TestRenewLease_ReturnsLostWhenLeaseGone(t *testing.T) {
	l := &fakeLease{}
	l.markGone(1)
	ctx, _ := cancelableContext(t)

	res := result(func() error { return renewLease(ctx, l, 1, 30*time.Millisecond) })
	if err := expectResult(t, res); !errors.Is(err, errLeaseLost) {
		t.Errorf("expected errLeaseLost, got %v", err)
	}
}

func TestRenewLease_RetriesWhileEtcdDoesNotAnswer(t *testing.T) {
	// Leadership is not given up while etcd is unreachable, however long:
	// etcd keeps leases through an outage.
	shortRenewRetry(t)
	l := &fakeLease{renew: func(ctx context.Context, n int) (*clientv3.LeaseKeepAliveResponse, error) {
		return nil, errors.New("etcdserver: request timed out")
	}}
	ctx, cancel := cancelableContext(t)

	res := result(func() error { return renewLease(ctx, l, 1, 30*time.Millisecond) })
	deadline := time.Now().Add(5 * time.Second)
	for l.renewCount() < 20 {
		if time.Now().After(deadline) {
			t.Fatalf("expected renewals to be retried, got %d", l.renewCount())
		}
		time.Sleep(time.Millisecond)
	}
	expectBlocked(t, res)
	cancel()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil on shutdown, got %v", err)
	}
}

func TestRenewLease_MovesOnFromHungCall(t *testing.T) {
	// A renewal that reaches a hung member must not block the next one
	// past the TTL: each call has a deadline of half the interval.
	shortRenewRetry(t)
	const ttl = 300 * time.Millisecond
	l := &fakeLease{renew: func(ctx context.Context, n int) (*clientv3.LeaseKeepAliveResponse, error) {
		if n == 0 {
			<-ctx.Done()
			return nil, ctx.Err()
		}
		return &clientv3.LeaseKeepAliveResponse{TTL: 0}, nil
	}}
	ctx, cancel := cancelableContext(t)

	res := result(func() error { return renewLease(ctx, l, 1, ttl) })
	deadline := time.Now().Add(5 * time.Second)
	for l.renewCount() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("expected a second renewal")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil on shutdown, got %v", err)
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	interval := ttl / 3
	if left := l.renewDeadline[0]; left <= 0 || left > interval/2 {
		t.Errorf("expected each renewal to carry a deadline of at most %v, got %v", interval/2, left)
	}
	// First renewal after interval, hung for interval/2, retried at once.
	if second := l.renewTimes[1].Sub(l.renewTimes[0]); second > interval {
		t.Errorf("expected the retry within %v of the hung renewal, it came after %v", interval, second)
	}
}

func TestRenewLease_StopsOnContextCancel(t *testing.T) {
	ctx, cancel := cancelableContext(t)
	res := result(func() error { return renewLease(ctx, &fakeLease{}, 1, 30*time.Millisecond) })
	expectBlocked(t, res)
	cancel()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil, got %v", err)
	}
}

func TestRenewLease_ZeroTTLDoesNotSpin(t *testing.T) {
	l := &fakeLease{}
	ctx, cancel := cancelableContext(t)
	res := result(func() error { return renewLease(ctx, l, 1, 0) })
	expectBlocked(t, res)
	cancel()
	expectResult(t, res)
	if l.renewCount() != 0 {
		t.Errorf("expected no renewals, got %d", l.renewCount())
	}
}

func TestLeaseKeepalive_SignalsLostLease(t *testing.T) {
	l := &fakeLease{}
	l.markGone(1)
	ctx, _ := cancelableContext(t)

	k := startLeaseKeepalive(ctx, l, 1, 30*time.Millisecond)
	defer k.stop()

	select {
	case <-k.lost:
	case <-time.After(5 * time.Second):
		t.Fatal("expected lost to be closed")
	}
}

func TestLeaseKeepalive_StopDoesNotSignalLost(t *testing.T) {
	ctx, _ := cancelableContext(t)
	k := startLeaseKeepalive(ctx, &fakeLease{}, 1, 30*time.Millisecond)
	k.stop()

	select {
	case <-k.lost:
		t.Error("stop must not signal a lost lease")
	default:
	}
}

func TestLeaseKeepalive_ParentCancelDoesNotSignalLost(t *testing.T) {
	ctx, cancel := cancelableContext(t)
	k := startLeaseKeepalive(ctx, &fakeLease{}, 1, 30*time.Millisecond)
	cancel()

	select {
	case <-k.done:
	case <-time.After(5 * time.Second):
		t.Fatal("keepalive goroutine did not exit")
	}
	select {
	case <-k.lost:
		t.Error("shutdown must not signal a lost lease")
	default:
	}
}

func TestCampaign_PlainKey(t *testing.T) {
	kv := &fakeKV{get: always(getResp(5, keyValue(0, "manual")), nil)}
	e := &etcdElector{kv: kv, watcher: newFakeWatcher(), key: testEtcdKey, value: "node1"}
	ctx, _ := cancelableContext(t)

	err := e.campaign(ctx, 1, nil)
	var plain *plainKeyError
	if !errors.As(err, &plain) {
		t.Fatalf("expected *plainKeyError, got %v", err)
	}
	if !strings.Contains(err.Error(), "etcdctl del "+testEtcdKey) {
		t.Errorf("expected the error to say how to resolve it, got %q", err)
	}
	if kv.txnCount() != 0 {
		t.Errorf("expected no write, got %d", kv.txnCount())
	}
}

func TestCampaign_KeyAlreadyHeldByOwnLease(t *testing.T) {
	// Not expected today (a failed write ends the attempt and revokes the
	// lease), but this node must then act as the holder.
	const ours clientv3.LeaseID = 3
	w := newFakeWatcher()
	kv := &fakeKV{get: always(getResp(40, keyValue(ours, "node1")), nil)}
	e := &etcdElector{kv: kv, watcher: w, key: testEtcdKey, value: "node1"}
	ctx, _ := cancelableContext(t)

	res := result(func() error { return e.campaign(ctx, ours, nil) })

	fw := w.next(t)
	if fw.rev != 41 {
		t.Errorf("expected to hold the lock from revision 41, got %d", fw.rev)
	}
	if kv.txnCount() != 0 {
		t.Errorf("expected no write, got %d", kv.txnCount())
	}
	// As the holder, losing the key is a loss of leadership (a standby
	// would keep waiting instead).
	fw.ch <- deleteEvent()
	if err := expectResult(t, res); !errors.Is(err, errLeadershipLost) {
		t.Errorf("expected errLeadershipLost, got %v", err)
	}
}

func TestCampaign_WaitsLockDelayBeforeTakingFreeKey(t *testing.T) {
	const lockDelay = 100 * time.Millisecond
	w := newFakeWatcher()
	kv := &fakeKV{get: always(getResp(5), nil), txnSucceeded: true, txnRev: 6}
	e := &etcdElector{kv: kv, watcher: w, key: testEtcdKey, value: "node1", lockDelay: lockDelay}
	ctx, cancel := cancelableContext(t)

	start := time.Now()
	res := result(func() error { return e.campaign(ctx, 1, nil) })

	// During lock-delay the key is watched from the read revision.
	if fw := w.next(t); fw.rev != 6 {
		t.Errorf("expected the wait to watch from revision 6, got %d", fw.rev)
	}
	if fw := w.next(t); fw.rev != 7 {
		t.Errorf("expected to hold the lock from the write revision 7, got %d", fw.rev)
	}
	if kv.txnCount() != 1 {
		t.Fatalf("expected one write, got %d", kv.txnCount())
	}
	if waited := kv.txnTimes[0].Sub(start); waited < lockDelay {
		t.Errorf("expected the write after lock-delay (%s), it came after %s", lockDelay, waited)
	}
	if !slices.Equal(kv.puts, []string{"node1"}) {
		t.Errorf("expected the lock value to be written, got %q", kv.puts)
	}
	if kv.noDeadline != 0 {
		t.Errorf("expected every request to carry a deadline, %d did not", kv.noDeadline)
	}
	cancel()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil on shutdown, got %v", err)
	}
}

func TestCampaign_LockDelayRestartsWhenKeyChanges(t *testing.T) {
	// Another node takes the key during our lock-delay and lets it go
	// again: our delay must count from its release, not from when we first
	// saw the key free.
	const lockDelay = 100 * time.Millisecond
	w := newFakeWatcher()
	kv := &fakeKV{get: sequence(getResp(5), getResp(8)), txnSucceeded: true, txnRev: 9}
	e := &etcdElector{kv: kv, watcher: w, key: testEtcdKey, value: "node1", lockDelay: lockDelay}
	ctx, cancel := cancelableContext(t)

	res := result(func() error { return e.campaign(ctx, 1, nil) })

	first := w.next(t)
	time.Sleep(lockDelay / 2)
	changedAt := time.Now()
	first.ch <- clientv3.WatchResponse{Events: []*clientv3.Event{
		{Type: mvccpb.PUT, Kv: keyValue(7, "nodeC")},
		{Type: mvccpb.DELETE, Kv: &mvccpb.KeyValue{Key: []byte(testEtcdKey)}},
	}}

	if fw := w.next(t); fw.rev != 9 {
		t.Errorf("expected the new wait to watch from the re-read revision 9, got %d", fw.rev)
	}
	w.next(t) // holding the lock
	if kv.txnCount() != 1 {
		t.Fatalf("expected one write, got %d", kv.txnCount())
	}
	if waited := kv.txnTimes[0].Sub(changedAt); waited < lockDelay {
		t.Errorf("expected the write lock-delay (%s) after the key changed, it came after %s", lockDelay, waited)
	}
	cancel()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil on shutdown, got %v", err)
	}
}

func TestCampaign_LostRaceWaitsForHolder(t *testing.T) {
	w := newFakeWatcher()
	kv := &fakeKV{
		get:          sequence(getResp(5), getResp(8, keyValue(7, "node2"))),
		txnSucceeded: false,
		txnRev:       8,
	}
	e := &etcdElector{kv: kv, watcher: w, key: testEtcdKey, value: "node1"}
	ctx, cancel := cancelableContext(t)

	res := result(func() error { return e.campaign(ctx, 1, nil) })

	// The write failed because node2 was faster; wait for node2's key.
	if fw := w.next(t); fw.rev != 9 {
		t.Errorf("expected to wait from revision 9, got %d", fw.rev)
	}
	cancel()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil on shutdown, got %v", err)
	}
}

func TestCampaign_PlainWriteWhileWaiting(t *testing.T) {
	w := newFakeWatcher()
	kv := &fakeKV{get: sequence(getResp(8, keyValue(7, "node2")), getResp(10, keyValue(0, "manual")))}
	e := &etcdElector{kv: kv, watcher: w, key: testEtcdKey, value: "node1"}
	ctx, _ := cancelableContext(t)

	res := result(func() error { return e.campaign(ctx, 1, nil) })

	// A plain write over the held key would never expire.
	w.next(t).ch <- putEvent(0, "manual")

	var plain *plainKeyError
	if err := expectResult(t, res); !errors.As(err, &plain) {
		t.Errorf("expected *plainKeyError, got %v", err)
	}
}

func TestCampaign_LeaseLostWhileWaiting(t *testing.T) {
	w := newFakeWatcher()
	kv := &fakeKV{get: always(getResp(8, keyValue(7, "node2")), nil)}
	e := &etcdElector{kv: kv, watcher: w, key: testEtcdKey, value: "node1"}
	ctx, _ := cancelableContext(t)
	leaseLost := make(chan struct{})

	res := result(func() error { return e.campaign(ctx, 1, leaseLost) })
	w.next(t)
	close(leaseLost)

	if err := expectResult(t, res); !errors.Is(err, errLeaseLost) {
		t.Errorf("expected errLeaseLost, got %v", err)
	}
}

func TestRevokeLease(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "revoked", want: true},
		{name: "lease already gone counts as revoked", err: rpctypes.ErrLeaseNotFound, want: true},
		{name: "etcd unreachable", err: errors.New("context deadline exceeded"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := &etcdElector{lease: &fakeLease{revokeErrs: []error{tt.err}}}
			if got := e.revokeLease(1); got != tt.want {
				t.Errorf("expected %v, got %v", tt.want, got)
			}
		})
	}
}

func newRunElector(kv *fakeKV, l *fakeLease, w *fakeWatcher) *etcdElector {
	return &etcdElector{kv: kv, lease: l, watcher: w, key: testEtcdKey, value: "node1", ttl: 10}
}

func TestEtcdRun_RevokesLeaseOnShutdown(t *testing.T) {
	w := newFakeWatcher()
	kv := &fakeKV{get: always(getResp(5), nil), txnSucceeded: true, txnRev: 6}
	l := &fakeLease{}
	ctx, cancel := cancelableContext(t)

	res := result(func() error { return newRunElector(kv, l, w).run(ctx) })
	w.next(t) // holding the lock
	cancel()

	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil on shutdown, got %v", err)
	}
	// Revoking the lease is what deletes the key.
	want := []string{"grant 1", "revoke 1"}
	if got := l.callLog(); !slices.Equal(got, want) {
		t.Errorf("expected calls %v, got %v", want, got)
	}
}

func TestEtcdRun_RetriesStaleLeaseBeforeNextAttempt(t *testing.T) {
	w := newFakeWatcher()
	kv := &fakeKV{get: always(getResp(5), nil), txnSucceeded: true, txnRev: 6}
	l := &fakeLease{revokeErrs: []error{errors.New("etcd unreachable")}}
	ctx, cancel := cancelableContext(t)
	e := newRunElector(kv, l, w)
	e.ttl = 1 // renewals every 333ms

	res := result(func() error { return e.run(ctx) })
	w.next(t) // holding the lock with lease 1

	// etcd reports the lease gone, and the revoke fails; as far as this
	// node knows, the lease may still hold the key.
	l.markGone(1)
	l.waitForCall(t, "grant 2")
	cancel()
	if err := expectResult(t, res); err != nil {
		t.Errorf("expected nil on shutdown, got %v", err)
	}

	got := l.callLog()
	want := []string{"grant 1", "revoke 1 failed", "revoke 1", "grant 2"}
	if len(got) < len(want) || !slices.Equal(got[:len(want)], want) {
		t.Errorf("expected calls to start with %v, got %v", want, got)
	}
}

func TestEtcdRun_PlainKeyIsFatal(t *testing.T) {
	kv := &fakeKV{get: always(getResp(5, keyValue(0, "manual")), nil)}
	l := &fakeLease{}
	ctx, _ := cancelableContext(t)

	err := expectResult(t, result(func() error { return newRunElector(kv, l, newFakeWatcher()).run(ctx) }))
	if err == nil || !strings.Contains(err.Error(), "no lease") {
		t.Fatalf("expected a fatal 'no lease' error, got %v", err)
	}
	if !slices.Contains(l.callLog(), "revoke 1") {
		t.Errorf("expected the lease to be revoked, got %v", l.callLog())
	}
}

func TestVerifyEtcdKeyState(t *testing.T) {
	tests := []struct {
		name    string
		resp    *clientv3.GetResponse
		err     error
		wantErr string
	}{
		{name: "no key", resp: getResp(1)},
		{name: "key held by a lease", resp: getResp(1, keyValue(7, "node2"))},
		{name: "plain key", resp: getResp(1, keyValue(0, "manual")), wantErr: "has no lease"},
		{name: "etcd unreachable", err: errors.New("context deadline exceeded"), wantErr: "failed to query key state"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kv := &fakeKV{get: always(tt.resp, tt.err)}
			err := verifyEtcdKeyState(context.Background(), kv, testEtcdKey)
			if tt.wantErr == "" {
				if err != nil {
					t.Errorf("expected no error, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("expected error containing %q, got %v", tt.wantErr, err)
			}
			if kv.noDeadline != 0 {
				t.Error("expected the read to carry a deadline")
			}
		})
	}
}

func TestEtcdConnectHint(t *testing.T) {
	if hint := etcdConnectHint(fmt.Errorf("failed to query key state: %w", context.DeadlineExceeded)); !strings.Contains(hint, "ETCD_CLIENT_DEBUG=warn") {
		t.Errorf("expected a hint for a timeout, got %q", hint)
	}
	if hint := etcdConnectHint(rpctypes.ErrPermissionDenied); hint != "" {
		t.Errorf("expected no hint for other errors, got %q", hint)
	}
}
