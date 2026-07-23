package scheduler

import (
	"context"
	"sync"
	"time"

	"github.com/yzx/rl-router/internal/domain"
	"github.com/yzx/rl-router/internal/scheduler/policy"
	"github.com/yzx/rl-router/internal/scheduler/store"
)

// ---------- event definitions ----------

type eventType int

const (
	evAllocate eventType = iota
	evRelease
	evAllocatePD
	evReleasePD
	evCompensatePD
	evStartStep
	evEndStep
	evCleanupGateway
	evRemoveSession
	evPause
	evContinue
	evGetResourceGroupState
	evGetResourceGroupDrainWait
	evListResourceGroupStates
	evListResourceGroups
	evRegisterInstances
	evUnregisterInstances
	evSyncInstances
	evStop
)

// event is a discriminated union dispatched by the single-threaded event-loop.
// Each eventType uses a specific subset of fields:
//
//	evAllocate:       ctx, route, resultCh
//	evRelease:        instanceID, gatewayAddr, allocationID, costMetrics
//	evAllocatePD:     ctx, route, pdResultCh
//	evReleasePD:      instanceID, gatewayAddr, allocationID, releaseRole, costMetrics, compensateRequestID
//	evCompensatePD:   compensateRequestID plus prefill/decode instance and allocation IDs
//	evStartStep:      stepID, policyName, policyConfig, stepResult
//	evEndStep:        stepID, stepResult
//	evCleanupGateway: gatewayAddr, cleanupDoneCh
//	evRemoveSession:  sessionID, sessionDoneCh
//	evGetResourceGroupDrainWait: resourceGroup, stepWaitResult
//	evRegisterInstances: instances, instanceResult
//	evUnregisterInstances: instanceIDs, instanceResult
//	evSyncInstances:     instances, instanceResult
//	evStop:           (none)
type event struct {
	typ         eventType
	enqueueTime time.Time // set before sending to eventCh; used for queue wait metric

	// Allocate fields
	ctx      context.Context
	route    *domain.RouteContext
	resultCh chan allocResult

	// Waiting queue fields (set by markEventQueued; event-loop only)
	enqueued       bool      // true => ownership transferred to a waiting queue
	queueEntryTime time.Time // wall clock at enqueue, used for timeout check
	// AllocatePD fields (reuse ctx, route, enqueueTime)
	pdResultCh chan pdAllocResult

	// Release fields
	instanceID   string
	gatewayAddr  string // same as RouteContext.GatewayID — the gateway's listen address
	allocationID string // idempotency key for Release dedup
	costMetrics  *domain.CostMetrics

	// ReleasePD fields (reuse instanceID, gatewayAddr, allocationID)
	releaseRole string // "prefill" or "decode"
	// compensateRequestID is set only by AllocatePD cancellation compensation.
	// It lets the event-loop remove a matching AllocatePD dedup entry after
	// releasing an allocation that the caller never observed.
	compensateRequestID       string
	compensatePrefillID       string
	compensateDecodeID        string
	compensatePrefillAllocID  string
	compensateDecodeAllocID   string
	compensateAllocationTrace string

	// Step/resource-group control fields
	resourceGroup            string
	resourceGroupMaxInflight int64
	stepID                   int64
	policyName               string              // optional: override policy for this step
	policyConfig             policy.PolicyConfig // optional: policy parameters for this step
	stepResult               chan stepResult

	// CleanupGateway fields
	cleanupDoneCh chan struct{}

	// RemoveSession fields
	sessionID     string
	sessionDoneCh chan struct{}

	// Pause / Continue fields
	pauseResult chan error

	// Resource-group state query fields
	stepStateResult           chan domain.StepState
	stepWaitResult            chan stepWaitResult
	resourceGroupStatesResult chan []domain.StepState
	resourceGroupsResult      chan []string

	// Instance mutation fields
	instances      []*domain.Instance
	instanceIDs    []string
	instanceResult chan instanceMutationResult
}

type allocResult struct {
	inst          *domain.Instance
	allocationID  string // unique allocation identifier
	newAllocation bool   // true only when this event committed fresh scheduler state
	err           error
}

// pdAllocResult is the result of a PD allocation event.
type pdAllocResult struct {
	prefill        *domain.Instance
	decode         *domain.Instance
	prefillAllocID string
	decodeAllocID  string
	newAllocation  bool // true only when this event committed fresh scheduler state
	err            error
}

type stepResult struct {
	pendingRequests int64
	err             error
}

type stepWaitResult struct {
	state  domain.StepState
	doneCh <-chan struct{}
}

type instanceMutationResult struct {
	syncResult store.SyncResult
	removed    int
	err        error
}

// ---------- constants ----------

const (
	defaultEventBufSize = 131072
	maxDrainBatch       = 8192
	submitRetryDelay    = 100 * time.Microsecond

	// defaultDedupCapacity is the initial capacity for allocDedup / releaseDedup maps.
	// Sized to hold 10w requests without rehashing (10w < 131072), avoiding
	// 4-5 map doublings that leave ~3.5MB of abandoned bucket arrays for GC.
	defaultDedupCapacity = 131072

	// heartbeatInterval controls how often the event-loop logs a periodic
	// status summary to router.log. This "proof of life" distinguishes a
	// genuinely idle scheduler from a deadlocked event-loop.
	heartbeatInterval = 30 * time.Second

	// pdAllocationCompensationWait bounds the background wait used after an
	// AllocatePD caller cancels. Fresh allocations normally resolve immediately;
	// queued canceled events may not drain until capacity changes, so they are
	// intentionally not waited on forever.
	pdAllocationCompensationWait = 30 * time.Second
	pdAllocationCanceledCode     = "allocate_canceled"
)

// ---------- object pools ----------

var allocResultPool = sync.Pool{
	New: func() any { return make(chan allocResult, 1) },
}

var pdAllocResultPool = sync.Pool{
	New: func() any { return make(chan pdAllocResult, 1) },
}

var eventPool = sync.Pool{
	New: func() any { return new(event) },
}

// getEvent returns an event from the pool, reset to zero value.
func getEvent() *event {
	ev := eventPool.Get().(*event)
	*ev = event{} // zero out all fields
	return ev
}

// markEventQueued records that a route event is now owned by a waiting queue.
func markEventQueued(ev *event) {
	ev.enqueued = true
	ev.queueEntryTime = time.Now()
}

// finishRouteEventIfNotQueued releases a route event unless a waiting queue
// took ownership. Queue drain/reject paths are responsible for queued events.
func finishRouteEventIfNotQueued(ev *event) {
	if ev.enqueued {
		return
	}
	domain.ReleaseRouteContext(ev.route)
	putEvent(ev)
}

// putEvent returns an event to the pool for reuse.
func putEvent(ev *event) {
	eventPool.Put(ev)
}

// sendError dispatches err to whichever result channel this event was created
// with (allocate vs allocate-PD). Success paths still build the full result
// struct manually; this helper only covers error/timeout/cancel/reject branches
// where only the error field matters.
func (ev *event) sendError(err error) {
	switch {
	case ev.resultCh != nil:
		ev.resultCh <- allocResult{err: err}
	case ev.pdResultCh != nil:
		ev.pdResultCh <- pdAllocResult{err: err}
	}
}

// finishQueued reports err on the event's result channel, releases its route
// context and returns the event to the pool. Returns wait_ms since queue entry
// for metric observation. Use only on events whose ownership has been
// transferred to a waiting queue (markEventQueued); non-queued events go
// through finishRouteEventIfNotQueued.
func (ev *event) finishQueued(now time.Time, err error) float64 {
	waitMs := float64(now.Sub(ev.queueEntryTime).Microseconds()) / 1000.0
	ev.sendError(err)
	domain.ReleaseRouteContext(ev.route)
	putEvent(ev)
	return waitMs
}

// allocCacheEntry caches a successful Allocate result for request_id dedup.
// Stores the Instance pointer directly to avoid re-allocating on dedup hits.
type allocCacheEntry struct {
	inst         *domain.Instance
	allocationID string
}

// pdAllocCacheEntry caches a successful AllocatePD result for request_id dedup.
type pdAllocCacheEntry struct {
	prefill        *domain.Instance
	decode         *domain.Instance
	prefillAllocID string
	decodeAllocID  string
	claimed        bool // true after the event-loop served this allocation to a dedup retry
}
