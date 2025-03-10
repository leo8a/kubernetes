/*
Copyright 2019 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package queueset

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"time"

	"k8s.io/apiserver/pkg/util/flowcontrol/debug"
	fq "k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing"
	"k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/eventclock"
	"k8s.io/apiserver/pkg/util/flowcontrol/fairqueuing/promise"
	"k8s.io/apiserver/pkg/util/flowcontrol/metrics"
	fqrequest "k8s.io/apiserver/pkg/util/flowcontrol/request"
	"k8s.io/apiserver/pkg/util/shufflesharding"
	"k8s.io/klog/v2"

	// The following hack is needed to work around a tooling deficiency.
	// Packages imported only for test code are not included in vendor.
	// See https://kubernetes.slack.com/archives/C0EG7JC6T/p1626985671458800?thread_ts=1626983387.450800&cid=C0EG7JC6T
	_ "k8s.io/utils/clock/testing"
)

const nsTimeFmt = "2006-01-02 15:04:05.000000000"

// queueSetFactory implements the QueueSetFactory interface
// queueSetFactory makes QueueSet objects.
type queueSetFactory struct {
	clock                 eventclock.Interface
	promiseFactoryFactory promiseFactoryFactory
}

// promiseFactory returns a WriteOnce
// - whose Set method is invoked with the queueSet locked, and
// - whose Get method is invoked with the queueSet not locked.
// The parameters are the same as for `promise.NewWriteOnce`.
type promiseFactory func(initial interface{}, doneCh <-chan struct{}, doneVal interface{}) promise.WriteOnce

// promiseFactoryFactory returns the promiseFactory to use for the given queueSet
type promiseFactoryFactory func(*queueSet) promiseFactory

// `*queueSetCompleter` implements QueueSetCompleter.  Exactly one of
// the fields `factory` and `theSet` is non-nil.
type queueSetCompleter struct {
	factory *queueSetFactory
	obsPair metrics.TimedObserverPair
	theSet  *queueSet
	qCfg    fq.QueuingConfig
	dealer  *shufflesharding.Dealer
}

// queueSet implements the Fair Queuing for Server Requests technique
// described in this package's doc, and a pointer to one implements
// the QueueSet interface.  The fields listed before the lock
// should not be changed; the fields listed after the
// lock must be accessed only while holding the lock.  The methods of
// this type follow the naming convention that the suffix "Locked"
// means the caller must hold the lock; for a method whose name does
// not end in "Locked" either acquires the lock or does not care about
// locking.
type queueSet struct {
	clock                    eventclock.Interface
	estimatedServiceDuration time.Duration
	obsPair                  metrics.TimedObserverPair

	promiseFactory promiseFactory

	lock sync.Mutex

	// qCfg holds the current queuing configuration.  Its
	// DesiredNumQueues may be less than the current number of queues.
	// If its DesiredNumQueues is zero then its other queuing
	// parameters retain the settings they had when DesiredNumQueues
	// was last non-zero (if ever).
	qCfg fq.QueuingConfig

	// the current dispatching configuration.
	dCfg fq.DispatchingConfig

	// If `qCfg.DesiredNumQueues` is non-zero then dealer is not nil
	// and is good for `qCfg`.
	dealer *shufflesharding.Dealer

	// queues may be longer than the desired number, while the excess
	// queues are still draining.
	queues []*queue

	// currentR is the amount of seat-seconds allocated per queue since process startup.
	// This is our generalization of the progress meter named R in the original fair queuing work.
	currentR SeatSeconds

	// lastRealTime is what `clock.Now()` yielded when `virtualTime` was last updated
	lastRealTime time.Time

	// robinIndex is the index of the last queue dispatched
	robinIndex int

	// totRequestsWaiting is the sum, over all the queues, of the
	// number of requests waiting in that queue
	totRequestsWaiting int

	// totRequestsExecuting is the total number of requests of this
	// queueSet that are currently executing.  That is the same as the
	// sum, over all the queues, of the number of requests executing
	// from that queue.
	totRequestsExecuting int

	// totSeatsInUse is the number of total "seats" in use by all the
	// request(s) that are currently executing in this queueset.
	totSeatsInUse int

	// enqueues is the number of requests that have ever been enqueued
	enqueues int
}

// NewQueueSetFactory creates a new QueueSetFactory object
func NewQueueSetFactory(c eventclock.Interface) fq.QueueSetFactory {
	return newTestableQueueSetFactory(c, ordinaryPromiseFactoryFactory)
}

// newTestableQueueSetFactory creates a new QueueSetFactory object with the given promiseFactoryFactory
func newTestableQueueSetFactory(c eventclock.Interface, promiseFactoryFactory promiseFactoryFactory) fq.QueueSetFactory {
	return &queueSetFactory{
		clock:                 c,
		promiseFactoryFactory: promiseFactoryFactory,
	}
}

func (qsf *queueSetFactory) BeginConstruction(qCfg fq.QueuingConfig, obsPair metrics.TimedObserverPair) (fq.QueueSetCompleter, error) {
	dealer, err := checkConfig(qCfg)
	if err != nil {
		return nil, err
	}
	return &queueSetCompleter{
		factory: qsf,
		obsPair: obsPair,
		qCfg:    qCfg,
		dealer:  dealer}, nil
}

// checkConfig returns a non-nil Dealer if the config is valid and
// calls for one, and returns a non-nil error if the given config is
// invalid.
func checkConfig(qCfg fq.QueuingConfig) (*shufflesharding.Dealer, error) {
	if qCfg.DesiredNumQueues == 0 {
		return nil, nil
	}
	dealer, err := shufflesharding.NewDealer(qCfg.DesiredNumQueues, qCfg.HandSize)
	if err != nil {
		err = fmt.Errorf("the QueueSetConfig implies an invalid shuffle sharding config (DesiredNumQueues is deckSize): %w", err)
	}
	return dealer, err
}

func (qsc *queueSetCompleter) Complete(dCfg fq.DispatchingConfig) fq.QueueSet {
	qs := qsc.theSet
	if qs == nil {
		qs = &queueSet{
			clock:                    qsc.factory.clock,
			estimatedServiceDuration: 3 * time.Millisecond,
			obsPair:                  qsc.obsPair,
			qCfg:                     qsc.qCfg,
			currentR:                 0,
			lastRealTime:             qsc.factory.clock.Now(),
		}
		qs.promiseFactory = qsc.factory.promiseFactoryFactory(qs)
	}
	qs.setConfiguration(context.Background(), qsc.qCfg, qsc.dealer, dCfg)
	return qs
}

// createQueues is a helper method for initializing an array of n queues
func createQueues(n, baseIndex int) []*queue {
	fqqueues := make([]*queue, n)
	for i := 0; i < n; i++ {
		fqqueues[i] = &queue{index: baseIndex + i, requests: newRequestFIFO()}
	}
	return fqqueues
}

func (qs *queueSet) BeginConfigChange(qCfg fq.QueuingConfig) (fq.QueueSetCompleter, error) {
	dealer, err := checkConfig(qCfg)
	if err != nil {
		return nil, err
	}
	return &queueSetCompleter{
		theSet: qs,
		qCfg:   qCfg,
		dealer: dealer}, nil
}

// setConfiguration is used to set the configuration for a queueSet.
// Update handling for when fields are updated is handled here as well -
// eg: if DesiredNum is increased, setConfiguration reconciles by
// adding more queues.
func (qs *queueSet) setConfiguration(ctx context.Context, qCfg fq.QueuingConfig, dealer *shufflesharding.Dealer, dCfg fq.DispatchingConfig) {
	qs.lockAndSyncTime(ctx)
	defer qs.lock.Unlock()

	if qCfg.DesiredNumQueues > 0 {
		// Adding queues is the only thing that requires immediate action
		// Removing queues is handled by attrition, removing a queue when
		// it goes empty and there are too many.
		numQueues := len(qs.queues)
		if qCfg.DesiredNumQueues > numQueues {
			qs.queues = append(qs.queues,
				createQueues(qCfg.DesiredNumQueues-numQueues, len(qs.queues))...)
		}
	} else {
		qCfg.QueueLengthLimit = qs.qCfg.QueueLengthLimit
		qCfg.HandSize = qs.qCfg.HandSize
		qCfg.RequestWaitLimit = qs.qCfg.RequestWaitLimit
	}

	qs.qCfg = qCfg
	qs.dCfg = dCfg
	qs.dealer = dealer
	qll := qCfg.QueueLengthLimit
	if qll < 1 {
		qll = 1
	}
	qs.obsPair.RequestsWaiting.SetX1(float64(qll))
	qs.obsPair.RequestsExecuting.SetX1(float64(dCfg.ConcurrencyLimit))

	qs.dispatchAsMuchAsPossibleLocked()
}

// A decision about a request
type requestDecision int

// Values passed through a request's decision
const (
	// Serve this one
	decisionExecute requestDecision = iota

	// Reject this one due to APF queuing considerations
	decisionReject

	// This one's context timed out / was canceled
	decisionCancel
)

// StartRequest begins the process of handling a request.  We take the
// approach of updating the metrics about total requests queued and
// executing at each point where there is a change in that quantity,
// because the metrics --- and only the metrics --- track that
// quantity per FlowSchema.
// The queueSet's promiseFactory is invoked once if the returned Request is non-nil,
// not invoked if the Request is nil.
func (qs *queueSet) StartRequest(ctx context.Context, workEstimate *fqrequest.WorkEstimate, hashValue uint64, flowDistinguisher, fsName string, descr1, descr2 interface{}, queueNoteFn fq.QueueNoteFn) (fq.Request, bool) {
	qs.lockAndSyncTime(ctx)
	defer qs.lock.Unlock()
	var req *request

	// ========================================================================
	// Step 0:
	// Apply only concurrency limit, if zero queues desired
	if qs.qCfg.DesiredNumQueues < 1 {
		if !qs.canAccommodateSeatsLocked(workEstimate.MaxSeats()) {
			klog.V(5).Infof("QS(%s): rejecting request %q %#+v %#+v because %d seats are asked for, %d seats are in use (%d are executing) and the limit is %d",
				qs.qCfg.Name, fsName, descr1, descr2, workEstimate, qs.totSeatsInUse, qs.totRequestsExecuting, qs.dCfg.ConcurrencyLimit)
			metrics.AddReject(ctx, qs.qCfg.Name, fsName, "concurrency-limit")
			return nil, qs.isIdleLocked()
		}
		req = qs.dispatchSansQueueLocked(ctx, workEstimate, flowDistinguisher, fsName, descr1, descr2)
		return req, false
	}

	// ========================================================================
	// Step 1:
	// 1) Start with shuffle sharding, to pick a queue.
	// 2) Reject old requests that have been waiting too long
	// 3) Reject current request if there is not enough concurrency shares and
	// we are at max queue length
	// 4) If not rejected, create a request and enqueue
	req = qs.timeoutOldRequestsAndRejectOrEnqueueLocked(ctx, workEstimate, hashValue, flowDistinguisher, fsName, descr1, descr2, queueNoteFn)
	// req == nil means that the request was rejected - no remaining
	// concurrency shares and at max queue length already
	if req == nil {
		klog.V(5).Infof("QS(%s): rejecting request %q %#+v %#+v due to queue full", qs.qCfg.Name, fsName, descr1, descr2)
		metrics.AddReject(ctx, qs.qCfg.Name, fsName, "queue-full")
		return nil, qs.isIdleLocked()
	}

	// ========================================================================
	// Step 2:
	// The next step is to invoke the method that dequeues as much
	// as possible.
	// This method runs a loop, as long as there are non-empty
	// queues and the number currently executing is less than the
	// assured concurrency value.  The body of the loop uses the
	// fair queuing technique to pick a queue and dispatch a
	// request from that queue.
	qs.dispatchAsMuchAsPossibleLocked()

	return req, false
}

// ordinaryPromiseFactoryFactory is the promiseFactoryFactory that
// a queueSetFactory would ordinarily use.
// Test code might use something different.
func ordinaryPromiseFactoryFactory(qs *queueSet) promiseFactory {
	return promise.NewWriteOnce
}

// MaxSeats returns the maximum number of seats this request requires, it is
// the maxumum of the two - WorkEstimate.InitialSeats, WorkEstimate.FinalSeats.
func (req *request) MaxSeats() int {
	return req.workEstimate.MaxSeats()
}

func (req *request) InitialSeats() int {
	return int(req.workEstimate.InitialSeats)
}

func (req *request) NoteQueued(inQueue bool) {
	if req.queueNoteFn != nil {
		req.queueNoteFn(inQueue)
	}
}

func (req *request) Finish(execFn func()) bool {
	exec, idle := req.wait()
	if !exec {
		return idle
	}
	func() {
		defer func() {
			idle = req.qs.finishRequestAndDispatchAsMuchAsPossible(req)
		}()

		execFn()
	}()

	return idle
}

func (req *request) wait() (bool, bool) {
	qs := req.qs

	// ========================================================================
	// Step 3:
	// The final step is to wait on a decision from
	// somewhere and then act on it.
	decisionAny := req.decision.Get()
	qs.lockAndSyncTime(req.ctx)
	defer qs.lock.Unlock()
	if req.waitStarted {
		// This can not happen, because the client is forbidden to
		// call Wait twice on the same request
		klog.Errorf("Duplicate call to the Wait method!  Immediately returning execute=false.  QueueSet=%s, startTime=%s, descr1=%#+v, descr2=%#+v", req.qs.qCfg.Name, req.startTime, req.descr1, req.descr2)
		return false, qs.isIdleLocked()
	}
	req.waitStarted = true
	switch decisionAny {
	case decisionReject:
		klog.V(5).Infof("QS(%s): request %#+v %#+v timed out after being enqueued\n", qs.qCfg.Name, req.descr1, req.descr2)
		metrics.AddReject(req.ctx, qs.qCfg.Name, req.fsName, "time-out")
		return false, qs.isIdleLocked()
	case decisionCancel:
	case decisionExecute:
		klog.V(5).Infof("QS(%s): Dispatching request %#+v %#+v from its queue", qs.qCfg.Name, req.descr1, req.descr2)
		return true, false
	default:
		// This can not happen, all possible values are handled above
		klog.Errorf("QS(%s): Impossible decision (type %T, value %#+v) for request %#+v %#+v!  Treating as cancel", qs.qCfg.Name, decisionAny, decisionAny, req.descr1, req.descr2)
	}
	// TODO(aaron-prindle) add metrics for this case
	klog.V(5).Infof("QS(%s): Ejecting request %#+v %#+v from its queue", qs.qCfg.Name, req.descr1, req.descr2)
	// remove the request from the queue as it has timed out
	req.removeFromQueueLocked()
	qs.totRequestsWaiting--
	metrics.AddRequestsInQueues(req.ctx, qs.qCfg.Name, req.fsName, -1)
	req.NoteQueued(false)
	qs.obsPair.RequestsWaiting.Add(-1)
	return false, qs.isIdleLocked()
}

func (qs *queueSet) IsIdle() bool {
	qs.lock.Lock()
	defer qs.lock.Unlock()
	return qs.isIdleLocked()
}

func (qs *queueSet) isIdleLocked() bool {
	return qs.totRequestsWaiting == 0 && qs.totRequestsExecuting == 0
}

// lockAndSyncTime acquires the lock and updates the virtual time.
// Doing them together avoids the mistake of modifying some queue state
// before calling syncTimeLocked.
func (qs *queueSet) lockAndSyncTime(ctx context.Context) {
	qs.lock.Lock()
	qs.syncTimeLocked(ctx)
}

// syncTimeLocked updates the virtual time based on the assumption
// that the current state of the queues has been in effect since
// `qs.lastRealTime`.  Thus, it should be invoked after acquiring the
// lock and before modifying the state of any queue.
func (qs *queueSet) syncTimeLocked(ctx context.Context) {
	realNow := qs.clock.Now()
	timeSinceLast := realNow.Sub(qs.lastRealTime)
	qs.lastRealTime = realNow
	prevR := qs.currentR
	incrR := SeatsTimesDuration(qs.getVirtualTimeRatioLocked(), timeSinceLast)
	qs.currentR = prevR + incrR
	switch {
	case prevR > qs.currentR:
		klog.ErrorS(errors.New("queueset::currentR overflow"), "Overflow", "QS", qs.qCfg.Name, "when", realNow.Format(nsTimeFmt), "prevR", prevR, "incrR", incrR, "currentR", qs.currentR)
	case qs.currentR >= highR:
		qs.advanceEpoch(ctx, realNow, incrR)
	}
	metrics.SetCurrentR(qs.qCfg.Name, qs.currentR.ToFloat())
}

// rDecrement is the amount by which the progress meter R is wound backwards
// when needed to avoid overflow.
const rDecrement = MaxSeatSeconds / 2

// highR is the threshold that triggers advance of the epoch.
// That is, decrementing the global progress meter R by rDecrement.
const highR = rDecrement + rDecrement/2

// advanceEpoch subtracts rDecrement from the global progress meter R
// and all the readings that have been taked from that meter.
// The now and incrR parameters are only used to add info to the log messages.
func (qs *queueSet) advanceEpoch(ctx context.Context, now time.Time, incrR SeatSeconds) {
	oldR := qs.currentR
	qs.currentR -= rDecrement
	klog.InfoS("Advancing epoch", "QS", qs.qCfg.Name, "when", now.Format(nsTimeFmt), "oldR", oldR, "newR", qs.currentR, "incrR", incrR)
	success := true
	for qIdx, queue := range qs.queues {
		if queue.requests.Length() == 0 && queue.requestsExecuting == 0 {
			// Do not just decrement, the value could be quite outdated.
			// It is safe to reset to zero in this case, because the next request
			// will overwrite the zero with `qs.currentR`.
			queue.nextDispatchR = 0
			continue
		}
		oldNextDispatchR := queue.nextDispatchR
		queue.nextDispatchR -= rDecrement
		if queue.nextDispatchR > oldNextDispatchR {
			klog.ErrorS(errors.New("queue::nextDispatchR underflow"), "Underflow", "QS", qs.qCfg.Name, "queue", qIdx, "oldNextDispatchR", oldNextDispatchR, "newNextDispatchR", queue.nextDispatchR, "incrR", incrR)
			success = false
		}
		queue.requests.Walk(func(req *request) bool {
			oldArrivalR := req.arrivalR
			req.arrivalR -= rDecrement
			if req.arrivalR > oldArrivalR {
				klog.ErrorS(errors.New("request::arrivalR underflow"), "Underflow", "QS", qs.qCfg.Name, "queue", qIdx, "request", *req, "oldArrivalR", oldArrivalR, "incrR", incrR)
				success = false
			}
			return true
		})
	}
	metrics.AddEpochAdvance(ctx, qs.qCfg.Name, success)
}

// getVirtualTimeRatio calculates the rate at which virtual time has
// been advancing, according to the logic in `doc.go`.
func (qs *queueSet) getVirtualTimeRatioLocked() float64 {
	activeQueues := 0
	seatsRequested := 0
	for _, queue := range qs.queues {
		// here we want the sum of the maximum width of the requests in this queue since our
		// goal is to find the maximum rate at which the queue could work.
		seatsRequested += (queue.seatsInUse + queue.requests.QueueSum().MaxSeatsSum)
		if queue.requests.Length() > 0 || queue.requestsExecuting > 0 {
			activeQueues++
		}
	}
	if activeQueues == 0 {
		return 0
	}
	return math.Min(float64(seatsRequested), float64(qs.dCfg.ConcurrencyLimit)) / float64(activeQueues)
}

// timeoutOldRequestsAndRejectOrEnqueueLocked encapsulates the logic required
// to validate and enqueue a request for the queueSet/QueueSet:
// 1) Start with shuffle sharding, to pick a queue.
// 2) Reject old requests that have been waiting too long
// 3) Reject current request if there is not enough concurrency shares and
// we are at max queue length
// 4) If not rejected, create a request and enqueue
// returns the enqueud request on a successful enqueue
// returns nil in the case that there is no available concurrency or
// the queuelengthlimit has been reached
func (qs *queueSet) timeoutOldRequestsAndRejectOrEnqueueLocked(ctx context.Context, workEstimate *fqrequest.WorkEstimate, hashValue uint64, flowDistinguisher, fsName string, descr1, descr2 interface{}, queueNoteFn fq.QueueNoteFn) *request {
	// Start with the shuffle sharding, to pick a queue.
	queueIdx := qs.shuffleShardLocked(hashValue, descr1, descr2)
	queue := qs.queues[queueIdx]
	// The next step is the logic to reject requests that have been waiting too long
	qs.removeTimedOutRequestsFromQueueLocked(queue, fsName)
	// NOTE: currently timeout is only checked for each new request.  This means that there can be
	// requests that are in the queue longer than the timeout if there are no new requests
	// We prefer the simplicity over the promptness, at least for now.

	defer qs.boundNextDispatchLocked(queue)

	// Create a request and enqueue
	req := &request{
		qs:                qs,
		fsName:            fsName,
		flowDistinguisher: flowDistinguisher,
		ctx:               ctx,
		decision:          qs.promiseFactory(nil, ctx.Done(), decisionCancel),
		arrivalTime:       qs.clock.Now(),
		arrivalR:          qs.currentR,
		queue:             queue,
		descr1:            descr1,
		descr2:            descr2,
		queueNoteFn:       queueNoteFn,
		workEstimate:      qs.completeWorkEstimate(workEstimate),
	}
	if ok := qs.rejectOrEnqueueLocked(req); !ok {
		return nil
	}
	metrics.ObserveQueueLength(ctx, qs.qCfg.Name, fsName, queue.requests.Length())
	return req
}

// shuffleShardLocked uses shuffle sharding to select a queue index
// using the given hashValue and the shuffle sharding parameters of the queueSet.
func (qs *queueSet) shuffleShardLocked(hashValue uint64, descr1, descr2 interface{}) int {
	var backHand [8]int
	// Deal into a data structure, so that the order of visit below is not necessarily the order of the deal.
	// This removes bias in the case of flows with overlapping hands.
	hand := qs.dealer.DealIntoHand(hashValue, backHand[:])
	handSize := len(hand)
	offset := qs.enqueues % handSize
	qs.enqueues++
	bestQueueIdx := -1
	minQueueSeatSeconds := MaxSeatSeconds
	for i := 0; i < handSize; i++ {
		queueIdx := hand[(offset+i)%handSize]
		queue := qs.queues[queueIdx]
		queueSum := queue.requests.QueueSum()

		// this is the total amount of work in seat-seconds for requests
		// waiting in this queue, we will select the queue with the minimum.
		thisQueueSeatSeconds := queueSum.TotalWorkSum
		klog.V(7).Infof("QS(%s): For request %#+v %#+v considering queue %d with sum: %#v and %d seats in use, nextDispatchR=%v", qs.qCfg.Name, descr1, descr2, queueIdx, queueSum, queue.seatsInUse, queue.nextDispatchR)
		if thisQueueSeatSeconds < minQueueSeatSeconds {
			minQueueSeatSeconds = thisQueueSeatSeconds
			bestQueueIdx = queueIdx
		}
	}
	if klog.V(6).Enabled() {
		chosenQueue := qs.queues[bestQueueIdx]
		klog.V(6).Infof("QS(%s) at t=%s R=%v: For request %#+v %#+v chose queue %d, with sum: %#v & %d seats in use & nextDispatchR=%v", qs.qCfg.Name, qs.clock.Now().Format(nsTimeFmt), qs.currentR, descr1, descr2, bestQueueIdx, chosenQueue.requests.QueueSum(), chosenQueue.seatsInUse, chosenQueue.nextDispatchR)
	}
	return bestQueueIdx
}

// removeTimedOutRequestsFromQueueLocked rejects old requests that have been enqueued
// past the requestWaitLimit
func (qs *queueSet) removeTimedOutRequestsFromQueueLocked(queue *queue, fsName string) {
	timeoutCount := 0
	now := qs.clock.Now()
	reqs := queue.requests
	// reqs are sorted oldest -> newest
	// can short circuit loop (break) if oldest requests are not timing out
	// as newer requests also will not have timed out

	// now - requestWaitLimit = waitLimit
	waitLimit := now.Add(-qs.qCfg.RequestWaitLimit)
	reqs.Walk(func(req *request) bool {
		if waitLimit.After(req.arrivalTime) {
			req.decision.Set(decisionReject)
			timeoutCount++
			metrics.AddRequestsInQueues(req.ctx, qs.qCfg.Name, req.fsName, -1)
			req.NoteQueued(false)

			// we need to check if the next request has timed out.
			return true
		}

		// since reqs are sorted oldest -> newest, we are done here.
		return false
	})

	// remove timed out requests from queue
	if timeoutCount > 0 {
		// The number of requests we have timed out is timeoutCount,
		// so, let's dequeue the exact number of requests for this queue.
		for i := 0; i < timeoutCount; i++ {
			queue.requests.Dequeue()
		}
		// decrement the # of requestsEnqueued
		qs.totRequestsWaiting -= timeoutCount
		qs.obsPair.RequestsWaiting.Add(float64(-timeoutCount))
	}
}

// rejectOrEnqueueLocked rejects or enqueues the newly arrived
// request, which has been assigned to a queue.  If up against the
// queue length limit and the concurrency limit then returns false.
// Otherwise enqueues and returns true.
func (qs *queueSet) rejectOrEnqueueLocked(request *request) bool {
	queue := request.queue
	curQueueLength := queue.requests.Length()
	// rejects the newly arrived request if resource criteria not met
	if qs.totSeatsInUse >= qs.dCfg.ConcurrencyLimit &&
		curQueueLength >= qs.qCfg.QueueLengthLimit {
		return false
	}

	qs.enqueueLocked(request)
	return true
}

// enqueues a request into its queue.
func (qs *queueSet) enqueueLocked(request *request) {
	queue := request.queue
	now := qs.clock.Now()
	if queue.requests.Length() == 0 && queue.requestsExecuting == 0 {
		// the queue’s start R is set to the virtual time.
		queue.nextDispatchR = qs.currentR
		if klog.V(6).Enabled() {
			klog.Infof("QS(%s) at t=%s R=%v: initialized queue %d start R due to request %#+v %#+v", qs.qCfg.Name, now.Format(nsTimeFmt), queue.nextDispatchR, queue.index, request.descr1, request.descr2)
		}
	}
	request.removeFromQueueLocked = queue.requests.Enqueue(request)
	qs.totRequestsWaiting++
	metrics.AddRequestsInQueues(request.ctx, qs.qCfg.Name, request.fsName, 1)
	request.NoteQueued(true)
	qs.obsPair.RequestsWaiting.Add(1)
}

// dispatchAsMuchAsPossibleLocked runs a loop, as long as there
// are non-empty queues and the number currently executing is less than the
// assured concurrency value.  The body of the loop uses the fair queuing
// technique to pick a queue, dequeue the request at the head of that
// queue, increment the count of the number executing, and send true
// to the request's channel.
func (qs *queueSet) dispatchAsMuchAsPossibleLocked() {
	for qs.totRequestsWaiting != 0 && qs.totSeatsInUse < qs.dCfg.ConcurrencyLimit {
		ok := qs.dispatchLocked()
		if !ok {
			break
		}
	}
}

func (qs *queueSet) dispatchSansQueueLocked(ctx context.Context, workEstimate *fqrequest.WorkEstimate, flowDistinguisher, fsName string, descr1, descr2 interface{}) *request {
	// does not call metrics.SetDispatchMetrics because there is no queuing and thus no interesting virtual world
	now := qs.clock.Now()
	req := &request{
		qs:                qs,
		fsName:            fsName,
		flowDistinguisher: flowDistinguisher,
		ctx:               ctx,
		startTime:         now,
		decision:          qs.promiseFactory(decisionExecute, ctx.Done(), decisionCancel),
		arrivalTime:       now,
		arrivalR:          qs.currentR,
		descr1:            descr1,
		descr2:            descr2,
		workEstimate:      qs.completeWorkEstimate(workEstimate),
	}
	qs.totRequestsExecuting++
	qs.totSeatsInUse += req.MaxSeats()
	metrics.AddRequestsExecuting(ctx, qs.qCfg.Name, fsName, 1)
	metrics.AddRequestConcurrencyInUse(qs.qCfg.Name, fsName, req.MaxSeats())
	qs.obsPair.RequestsExecuting.Add(1)
	if klog.V(5).Enabled() {
		klog.Infof("QS(%s) at t=%s R=%v: immediate dispatch of request %q %#+v %#+v, qs will have %d executing", qs.qCfg.Name, now.Format(nsTimeFmt), qs.currentR, fsName, descr1, descr2, qs.totRequestsExecuting)
	}
	return req
}

// dispatchLocked uses the Fair Queuing for Server Requests method to
// select a queue and dispatch the oldest request in that queue.  The
// return value indicates whether a request was dispatched; this will
// be false when there are no requests waiting in any queue.
func (qs *queueSet) dispatchLocked() bool {
	queue, request := qs.findDispatchQueueLocked()
	if queue == nil {
		return false
	}
	if request == nil { // This should never happen.  But if it does...
		return false
	}
	request.startTime = qs.clock.Now()
	// At this moment the request leaves its queue and starts
	// executing.  We do not recognize any interim state between
	// "queued" and "executing".  While that means "executing"
	// includes a little overhead from this package, this is not a
	// problem because other overhead is also included.
	qs.totRequestsWaiting--
	qs.totRequestsExecuting++
	qs.totSeatsInUse += request.MaxSeats()
	queue.requestsExecuting++
	queue.seatsInUse += request.MaxSeats()
	metrics.AddRequestsInQueues(request.ctx, qs.qCfg.Name, request.fsName, -1)
	request.NoteQueued(false)
	metrics.AddRequestsExecuting(request.ctx, qs.qCfg.Name, request.fsName, 1)
	metrics.AddRequestConcurrencyInUse(qs.qCfg.Name, request.fsName, request.MaxSeats())
	qs.obsPair.RequestsWaiting.Add(-1)
	qs.obsPair.RequestsExecuting.Add(1)
	if klog.V(6).Enabled() {
		klog.Infof("QS(%s) at t=%s R=%v: dispatching request %#+v %#+v work %v from queue %d with start R %v, queue will have %d waiting & %d requests occupying %d seats, set will have %d seats occupied",
			qs.qCfg.Name, request.startTime.Format(nsTimeFmt), qs.currentR, request.descr1, request.descr2,
			request.workEstimate, queue.index, queue.nextDispatchR, queue.requests.Length(), queue.requestsExecuting, queue.seatsInUse, qs.totSeatsInUse)
	}
	// When a request is dequeued for service -> qs.virtualStart += G * width
	queue.nextDispatchR += request.totalWork()
	qs.boundNextDispatchLocked(queue)
	request.decision.Set(decisionExecute)
	return true
}

// canAccommodateSeatsLocked returns true if this queueSet has enough
// seats available to accommodate a request with the given number of seats,
// otherwise it returns false.
func (qs *queueSet) canAccommodateSeatsLocked(seats int) bool {
	switch {
	case seats > qs.dCfg.ConcurrencyLimit:
		// we have picked the queue with the minimum virtual finish time, but
		// the number of seats this request asks for exceeds the concurrency limit.
		// TODO: this is a quick fix for now, once we have borrowing in place we will not need it
		if qs.totRequestsExecuting == 0 {
			// TODO: apply additional lateny associated with this request, as described in the KEP
			return true
		}
		// wait for all "currently" executing requests in this queueSet
		// to finish before we can execute this request.
		return false
	case qs.totSeatsInUse+seats > qs.dCfg.ConcurrencyLimit:
		return false
	}

	return true
}

// findDispatchQueueLocked examines the queues in round robin order and
// returns the first one of those for which the virtual finish time of
// the oldest waiting request is minimal, and also returns that request.
// Returns nils if the head of the selected queue can not be dispatched now.
func (qs *queueSet) findDispatchQueueLocked() (*queue, *request) {
	minVirtualFinish := MaxSeatSeconds
	sMin := MaxSeatSeconds
	dsMin := MaxSeatSeconds
	sMax := MinSeatSeconds
	dsMax := MinSeatSeconds
	var minQueue *queue
	var minIndex int
	nq := len(qs.queues)
	for range qs.queues {
		qs.robinIndex = (qs.robinIndex + 1) % nq
		queue := qs.queues[qs.robinIndex]
		oldestWaiting, _ := queue.requests.Peek()
		if oldestWaiting != nil {
			sMin = ssMin(sMin, queue.nextDispatchR)
			sMax = ssMax(sMax, queue.nextDispatchR)
			estimatedWorkInProgress := SeatsTimesDuration(float64(queue.seatsInUse), qs.estimatedServiceDuration)
			dsMin = ssMin(dsMin, queue.nextDispatchR-estimatedWorkInProgress)
			dsMax = ssMax(dsMax, queue.nextDispatchR-estimatedWorkInProgress)
			currentVirtualFinish := queue.nextDispatchR + oldestWaiting.totalWork()
			klog.V(11).InfoS("Considering queue to dispatch", "queueSet", qs.qCfg.Name, "queue", qs.robinIndex, "finishR", currentVirtualFinish)
			if currentVirtualFinish < minVirtualFinish {
				minVirtualFinish = currentVirtualFinish
				minQueue = queue
				minIndex = qs.robinIndex
			}
		}
	}

	oldestReqFromMinQueue, _ := minQueue.requests.Peek()
	if oldestReqFromMinQueue == nil {
		// This cannot happen
		klog.ErrorS(errors.New("selected queue is empty"), "Impossible", "queueSet", qs.qCfg.Name)
		return nil, nil
	}
	if !qs.canAccommodateSeatsLocked(oldestReqFromMinQueue.MaxSeats()) {
		// since we have not picked the queue with the minimum virtual finish
		// time, we are not going to advance the round robin index here.
		if klog.V(4).Enabled() {
			klog.Infof("QS(%s): request %v %v seats %d cannot be dispatched from queue %d, waiting for currently executing requests to complete, %d requests are occupying %d seats and the limit is %d",
				qs.qCfg.Name, oldestReqFromMinQueue.descr1, oldestReqFromMinQueue.descr2, oldestReqFromMinQueue.MaxSeats(), minQueue.index, qs.totRequestsExecuting, qs.totSeatsInUse, qs.dCfg.ConcurrencyLimit)
		}
		return nil, nil
	}
	oldestReqFromMinQueue.removeFromQueueLocked()

	// If the requested final seats exceed capacity of that queue,
	// we reduce them to current capacity and adjust additional latency
	// to preserve the total amount of work.
	if oldestReqFromMinQueue.workEstimate.FinalSeats > uint(qs.dCfg.ConcurrencyLimit) {
		finalSeats := uint(qs.dCfg.ConcurrencyLimit)
		additionalLatency := oldestReqFromMinQueue.workEstimate.finalWork.DurationPerSeat(float64(finalSeats))
		oldestReqFromMinQueue.workEstimate.FinalSeats = finalSeats
		oldestReqFromMinQueue.workEstimate.AdditionalLatency = additionalLatency
	}

	// we set the round robin indexing to start at the chose queue
	// for the next round.  This way the non-selected queues
	// win in the case that the virtual finish times are the same
	qs.robinIndex = minIndex

	if minQueue.nextDispatchR < oldestReqFromMinQueue.arrivalR {
		klog.ErrorS(errors.New("dispatch before arrival"), "Inconceivable!", "QS", qs.qCfg.Name, "queue", minQueue.index, "dispatchR", minQueue.nextDispatchR, "request", oldestReqFromMinQueue)
	}
	metrics.SetDispatchMetrics(qs.qCfg.Name, qs.currentR.ToFloat(), minQueue.nextDispatchR.ToFloat(), sMin.ToFloat(), sMax.ToFloat(), dsMin.ToFloat(), dsMax.ToFloat())
	return minQueue, oldestReqFromMinQueue
}

func ssMin(a, b SeatSeconds) SeatSeconds {
	if a > b {
		return b
	}
	return a
}

func ssMax(a, b SeatSeconds) SeatSeconds {
	if a < b {
		return b
	}
	return a
}

// finishRequestAndDispatchAsMuchAsPossible is a convenience method
// which calls finishRequest for a given request and then dispatches
// as many requests as possible.  This is all of what needs to be done
// once a request finishes execution or is canceled.  This returns a bool
// indicating whether the QueueSet is now idle.
func (qs *queueSet) finishRequestAndDispatchAsMuchAsPossible(req *request) bool {
	qs.lockAndSyncTime(req.ctx)
	defer qs.lock.Unlock()

	qs.finishRequestLocked(req)
	qs.dispatchAsMuchAsPossibleLocked()
	return qs.isIdleLocked()
}

// finishRequestLocked is a callback that should be used when a
// previously dispatched request has completed it's service.  This
// callback updates important state in the queueSet
func (qs *queueSet) finishRequestLocked(r *request) {
	now := qs.clock.Now()
	qs.totRequestsExecuting--
	metrics.AddRequestsExecuting(r.ctx, qs.qCfg.Name, r.fsName, -1)
	qs.obsPair.RequestsExecuting.Add(-1)

	actualServiceDuration := now.Sub(r.startTime)

	// TODO: for now we keep the logic localized so it is easier to see
	//  how the counters are tracked for queueset and queue, in future we
	//  can refactor to move this function.
	releaseSeatsLocked := func() {
		defer qs.removeQueueIfEmptyLocked(r)

		qs.totSeatsInUse -= r.MaxSeats()
		metrics.AddRequestConcurrencyInUse(qs.qCfg.Name, r.fsName, -r.MaxSeats())
		if r.queue != nil {
			r.queue.seatsInUse -= r.MaxSeats()
		}
	}

	defer func() {
		if r.workEstimate.AdditionalLatency <= 0 {
			// release the seats allocated to this request immediately
			releaseSeatsLocked()
			if !klog.V(6).Enabled() {
			} else if r.queue != nil {
				klog.Infof("QS(%s) at t=%s R=%v: request %#+v %#+v finished all use of %d seats, adjusted queue %d start R to %v due to service time %.9fs, queue will have %d requests with %#v waiting & %d requests occupying %d seats",
					qs.qCfg.Name, now.Format(nsTimeFmt), qs.currentR, r.descr1, r.descr2, r.workEstimate.MaxSeats(), r.queue.index,
					r.queue.nextDispatchR, actualServiceDuration.Seconds(), r.queue.requests.Length(), r.queue.requests.QueueSum(), r.queue.requestsExecuting, r.queue.seatsInUse)
			} else {
				klog.Infof("QS(%s) at t=%s R=%v: request %#+v %#+v finished all use of %d seats, qs will have %d requests occupying %d seats", qs.qCfg.Name, now.Format(nsTimeFmt), qs.currentR, r.descr1, r.descr2, r.workEstimate.InitialSeats, qs.totRequestsExecuting, qs.totSeatsInUse)
			}
			return
		}

		additionalLatency := r.workEstimate.AdditionalLatency
		if !klog.V(6).Enabled() {
		} else if r.queue != nil {
			klog.Infof("QS(%s) at t=%s R=%v: request %#+v %#+v finished main use of %d seats but lingering on %d seats for %v seconds, adjusted queue %d start R to %v due to service time %.9fs, queue will have %d requests with %#v waiting & %d requests occupying %d seats",
				qs.qCfg.Name, now.Format(nsTimeFmt), qs.currentR, r.descr1, r.descr2, r.workEstimate.InitialSeats, r.workEstimate.FinalSeats, additionalLatency.Seconds(), r.queue.index,
				r.queue.nextDispatchR, actualServiceDuration.Seconds(), r.queue.requests.Length(), r.queue.requests.QueueSum(), r.queue.requestsExecuting, r.queue.seatsInUse)
		} else {
			klog.Infof("QS(%s) at t=%s R=%v: request %#+v %#+v finished main use of %d seats but lingering on %d seats for %v seconds, qs will have %d requests occupying %d seats", qs.qCfg.Name, now.Format(nsTimeFmt), qs.currentR, r.descr1, r.descr2, r.workEstimate.InitialSeats, r.workEstimate.FinalSeats, additionalLatency.Seconds(), qs.totRequestsExecuting, qs.totSeatsInUse)
		}
		// EventAfterDuration will execute the event func in a new goroutine,
		// so the seats allocated to this request will be released after
		// AdditionalLatency elapses, this ensures that the additional
		// latency has no impact on the user experience.
		qs.clock.EventAfterDuration(func(_ time.Time) {
			qs.lockAndSyncTime(r.ctx)
			defer qs.lock.Unlock()
			now := qs.clock.Now()
			releaseSeatsLocked()
			if !klog.V(6).Enabled() {
			} else if r.queue != nil {
				klog.Infof("QS(%s) at t=%s R=%v: request %#+v %#+v finished lingering on %d seats, queue %d will have %d requests with %#v waiting & %d requests occupying %d seats",
					qs.qCfg.Name, now.Format(nsTimeFmt), qs.currentR, r.descr1, r.descr2, r.workEstimate.FinalSeats, r.queue.index,
					r.queue.requests.Length(), r.queue.requests.QueueSum(), r.queue.requestsExecuting, r.queue.seatsInUse)
			} else {
				klog.Infof("QS(%s) at t=%s R=%v: request %#+v %#+v finished lingering on %d seats, qs will have %d requests occupying %d seats", qs.qCfg.Name, now.Format(nsTimeFmt), qs.currentR, r.descr1, r.descr2, r.workEstimate.FinalSeats, qs.totRequestsExecuting, qs.totSeatsInUse)
			}
			qs.dispatchAsMuchAsPossibleLocked()
		}, additionalLatency)
	}()

	if r.queue != nil {
		// request has finished, remove from requests executing
		r.queue.requestsExecuting--

		// When a request finishes being served, and the actual service time was S,
		// the queue’s start R is decremented by (G - S)*width.
		r.queue.nextDispatchR -= SeatsTimesDuration(float64(r.InitialSeats()), qs.estimatedServiceDuration-actualServiceDuration)
		qs.boundNextDispatchLocked(r.queue)
	}
}

// boundNextDispatchLocked applies the anti-windup hack.
// We need a hack because all non-empty queues are allocated the same
// number of seats.  A queue that can not use all those seats and does
// not go empty accumulates a progresively earlier `virtualStart` compared
// to queues that are using more than they are allocated.
// The following hack addresses the first side of that inequity,
// by insisting that dispatch in the virtual world not precede arrival.
func (qs *queueSet) boundNextDispatchLocked(queue *queue) {
	oldestReqFromMinQueue, _ := queue.requests.Peek()
	if oldestReqFromMinQueue == nil {
		return
	}
	var virtualStartBound = oldestReqFromMinQueue.arrivalR
	if queue.nextDispatchR < virtualStartBound {
		if klog.V(4).Enabled() {
			klog.InfoS("AntiWindup tweaked queue", "QS", qs.qCfg.Name, "queue", queue.index, "time", qs.clock.Now().Format(nsTimeFmt), "requestDescr1", oldestReqFromMinQueue.descr1, "requestDescr2", oldestReqFromMinQueue.descr2, "newVirtualStart", virtualStartBound, "deltaVirtualStart", (virtualStartBound - queue.nextDispatchR))
		}
		queue.nextDispatchR = virtualStartBound
	}
}

func (qs *queueSet) removeQueueIfEmptyLocked(r *request) {
	if r.queue == nil {
		return
	}

	// If there are more queues than desired and this one has no
	// requests then remove it
	if len(qs.queues) > qs.qCfg.DesiredNumQueues &&
		r.queue.requests.Length() == 0 &&
		r.queue.requestsExecuting == 0 {
		qs.queues = removeQueueAndUpdateIndexes(qs.queues, r.queue.index)

		// decrement here to maintain the invariant that (qs.robinIndex+1) % numQueues
		// is the index of the next queue after the one last dispatched from
		if qs.robinIndex >= r.queue.index {
			qs.robinIndex--
		}
	}
}

// removeQueueAndUpdateIndexes uses reslicing to remove an index from a slice
// and then updates the 'index' field of the queues to be correct
func removeQueueAndUpdateIndexes(queues []*queue, index int) []*queue {
	keptQueues := append(queues[:index], queues[index+1:]...)
	for i := index; i < len(keptQueues); i++ {
		keptQueues[i].index--
	}
	return keptQueues
}

func (qs *queueSet) UpdateObservations() {
	qs.obsPair.RequestsWaiting.Add(0)
	qs.obsPair.RequestsExecuting.Add(0)
}

func (qs *queueSet) Dump(includeRequestDetails bool) debug.QueueSetDump {
	qs.lock.Lock()
	defer qs.lock.Unlock()
	d := debug.QueueSetDump{
		Queues:     make([]debug.QueueDump, len(qs.queues)),
		Waiting:    qs.totRequestsWaiting,
		Executing:  qs.totRequestsExecuting,
		SeatsInUse: qs.totSeatsInUse,
	}
	for i, q := range qs.queues {
		d.Queues[i] = q.dumpLocked(includeRequestDetails)
	}
	return d
}
