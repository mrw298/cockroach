// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package replica_rac2

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/kvflowcontrol/rac2"
	"github.com/cockroachdb/cockroach/pkg/kv/kvserver/raftlog"
	"github.com/cockroachdb/cockroach/pkg/raft/raftpb"
	"github.com/cockroachdb/cockroach/pkg/roachpb"
	"github.com/cockroachdb/cockroach/pkg/util/admission/admissionpb"
	"github.com/cockroachdb/cockroach/pkg/util/buildutil"
	"github.com/cockroachdb/cockroach/pkg/util/log"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
	"github.com/cockroachdb/errors"
)

// Replica abstracts kvserver.Replica. It exposes internal implementation
// details of Replica, specifically the locking behavior, since it is
// essential to reason about correctness.
type Replica interface {
	// RaftMuAssertHeld asserts that Replica.raftMu is held.
	RaftMuAssertHeld()
	// MuAssertHeld asserts that Replica.mu is held.
	MuAssertHeld()
	// MuLock acquires Replica.mu.
	MuLock()
	// MuUnlock releases Replica.mu.
	MuUnlock()
	// RaftNodeMuLocked returns a reference to the RaftNode. It is only called
	// after Processor knows the Replica is initialized.
	//
	// At least Replica mu is held. The caller does not make any claims about
	// whether it holds raftMu or not.
	RaftNodeMuLocked() RaftNode
	// LeaseholderMuLocked returns the Replica's current knowledge of the
	// leaseholder, which can be stale. It is only called after Processor
	// knows the Replica is initialized.
	//
	// At least Replica mu is held. The caller does not make any claims about
	// whether it holds raftMu or not.
	LeaseholderMuLocked() roachpb.ReplicaID
}

// RaftScheduler abstracts kvserver.raftScheduler.
type RaftScheduler interface {
	// EnqueueRaftReady schedules Ready processing, that will also ensure that
	// Processor.HandleRaftReadyRaftMuLocked is called.
	EnqueueRaftReady(id roachpb.RangeID)
}

// RaftNode abstracts raft.RawNode. All methods must be called while holding
// both Replica mu and raftMu.
//
// It should not be essential for read-only methods to hold Replica mu, since
// except for one case (flushing the proposal buffer), all methods that mutate
// state in raft.RawNode hold both mutexes. Consider the following information
// for a replica maintained by the leader: Match, Next, HighestUnstableIndex.
// (Match, Next) represent in-flight entries, that are not affected by
// flushing the proposal buffer. [Next, HighestUnstableIndex) are pending, and
// HighestUnstableIndex *is* affected by flushing the proposal buffer.
// Additionally, a replica (leader or follower) also has a NextUnstableIndex
// <= HighestUnstableIndex, which is the index of the next entry that will be
// sent to local storage (Match is equivalent to StableIndex at a replica), if
// there are any such entries. That is, NextUnstableIndex represents an
// exclusive upper bound on MsgStorageAppends that have already been retrieved
// from Ready. At the leader, the Next value for a replica is <=
// NextUnstableIndex for the leader. NextUnstableIndex on the leader is not
// affected by flushing the proposal buffer. RACv2 code limits its advancing
// knowledge of state on any replica (leader or follower) to
// NextUnstableIndex, since it is never concerned at any replica with indices
// that have not been seen in a MsgStorageAppend. This suggests read-only
// methods should not be affected by concurrent advancing of
// HighestUnstableIndex.
//
// Despite the above, there are implementation details of Raft, specifically
// maintenance of tracker.Progress, that result in false data races. Due to
// this, reads done by RACv2 ensure both mutexes are held. We mention this
// since RACv2 code may not be able to tolerate a true data race, in that it
// reads Raft state at various points while holding raftMu, and expects those
// various reads to be mutually consistent.
type RaftNode interface {
	// EnablePingForAdmittedLaggingLocked is a one time behavioral change made
	// to enable pinging for the admitted array when it is lagging match. Once
	// changed, this will apply to current and future leadership roles at this
	// replica.
	EnablePingForAdmittedLaggingLocked()

	// Read-only methods.

	// LeaderLocked returns the current known leader. This state can advance
	// past the group membership state, so the leader returned here may not be
	// known as a current group member.
	LeaderLocked() roachpb.ReplicaID
	// StableIndexLocked is the (inclusive) highest index that is known to be
	// successfully persisted in local storage.
	StableIndexLocked() uint64
	// NextUnstableIndexLocked returns the index of the next entry that will
	// be sent to local storage. All entries < this index are either stored,
	// or have been sent to storage.
	//
	// NB: NextUnstableIndex can regress when the node accepts appends or
	// snapshots from a newer leader.
	NextUnstableIndexLocked() uint64
	// GetAdmittedLocked returns the current value of the admitted array.
	GetAdmittedLocked() [raftpb.NumPriorities]uint64
	// MyLeaderTermLocked returns the term, if this replica is the leader, else
	// 0.
	MyLeaderTermLocked() uint64

	// Mutating methods.

	// SetAdmittedLocked sets a new value for the admitted array. It is the
	// caller's responsibility to ensure that it is not regressing admitted,
	// and it is not advancing admitted beyond the stable index.
	SetAdmittedLocked([raftpb.NumPriorities]uint64) raftpb.Message
	// StepMsgAppRespForAdmittedLocked steps a MsgAppResp on the leader, which
	// may advance its knowledge of a follower's admitted state.
	StepMsgAppRespForAdmittedLocked(raftpb.Message) error
}

// AdmittedPiggybacker is used to enqueue MsgAppResp messages whose purpose is
// to advance Admitted. For efficiency, these need to be piggybacked on other
// messages being sent to the given leader node. The StoreID and RangeID are
// provided so that the leader node can route the incoming message to the
// relevant range.
type AdmittedPiggybacker interface {
	AddMsgAppRespForLeader(roachpb.NodeID, roachpb.StoreID, roachpb.RangeID, raftpb.Message)
}

// EntryForAdmission is the information provided to the admission control (AC)
// system, when requesting admission.
type EntryForAdmission struct {
	// Information needed by the AC system, for deciding when to admit, and
	// for maintaining its accounting of how much work has been
	// requested/admitted.
	TenantID   roachpb.TenantID
	Priority   admissionpb.WorkPriority
	CreateTime int64
	// RequestedCount is the number of admission tokens requested (not to be
	// confused with replication AC flow tokens).
	RequestedCount int64
	// Ingested is true iff this request represents a sstable that will be
	// ingested into Pebble.
	Ingested bool

	// CallbackState is information that is needed by the callback when the
	// entry is admitted.
	CallbackState EntryForAdmissionCallbackState
}

// EntryForAdmissionCallbackState is passed to the callback when the entry is
// admitted.
type EntryForAdmissionCallbackState struct {
	// Routing state to get to the Processor.
	StoreID roachpb.StoreID
	RangeID roachpb.RangeID

	// State needed by the Processor.
	ReplicaID  roachpb.ReplicaID
	LeaderTerm uint64
	Index      uint64
	Priority   raftpb.Priority
}

// ACWorkQueue abstracts the behavior needed from admission.WorkQueue.
type ACWorkQueue interface {
	Admit(ctx context.Context, entry EntryForAdmission)
}

// TODO(sumeer): temporary placeholder, until RangeController is more fully
// fleshed out.
type rangeControllerInitState struct {
	replicaSet    rac2.ReplicaSet
	leaseholder   roachpb.ReplicaID
	nextRaftIndex uint64
}

// RangeControllerFactory abstracts RangeController creation for testing.
type RangeControllerFactory interface {
	// New creates a new RangeController.
	New(state rangeControllerInitState) rac2.RangeController
}

// EnabledWhenLeaderLevel captures the level at which RACv2 is enabled when
// this replica is the leader.
//
// State transitions are NotEnabledWhenLeader => EnabledWhenLeaderV1Encoding
// => EnabledWhenLeaderV2Encoding, i.e., the level will never regress.
type EnabledWhenLeaderLevel uint8

const (
	NotEnabledWhenLeader EnabledWhenLeaderLevel = iota
	EnabledWhenLeaderV1Encoding
	EnabledWhenLeaderV2Encoding
)

// ProcessorOptions are specified when creating a new Processor.
type ProcessorOptions struct {
	// Various constant fields that are duplicated from Replica, since we
	// have abstracted Replica for testing.
	//
	// TODO(sumeer): this is a premature optimization to avoid calling
	// Replica interface methods. Revisit.
	NodeID    roachpb.NodeID
	StoreID   roachpb.StoreID
	RangeID   roachpb.RangeID
	TenantID  roachpb.TenantID
	ReplicaID roachpb.ReplicaID

	Replica                Replica
	RaftScheduler          RaftScheduler
	AdmittedPiggybacker    AdmittedPiggybacker
	ACWorkQueue            ACWorkQueue
	RangeControllerFactory RangeControllerFactory

	EnabledWhenLeaderLevel EnabledWhenLeaderLevel
}

// SideChannelInfoUsingRaftMessageRequest is used to provide a follower
// information about the leader's protocol, and if the leader is using the
// RACv2 protocol, additional information about entries.
type SideChannelInfoUsingRaftMessageRequest struct {
	UsingV2Protocol bool
	LeaderTerm      uint64
	// Following are only used if UsingV2Protocol is true.
	First, Last    uint64
	LowPriOverride bool
}

// Processor handles RACv2 processing for a Replica. It combines the
// functionality needed by any replica, and needed only at the leader, since
// there is common membership state needed in both roles. There are some
// methods that will only be called on the leader or a follower, and it must
// gracefully handle the case where those method calls are stale in their
// assumption of the role of this replica.
//
// Processor can be created on an uninitialized Replica, hence group
// membership may not be known. Group membership is learnt (and kept
// up-to-date) via OnDescChangedLocked. Knowledge of the leader can advance
// past the current group membership, and must be tolerated. Knowledge of the
// leaseholder can be stale, and must be tolerated.
//
// Transitions into and out of leadership, or knowledge of the current leader,
// is discovered in HandleRaftReadyRaftMuLocked. It is important that there is
// a low lag between losing leadership, which is discovered on calling
// RawNode.Step, and HandleRaftReadyRaftMuLocked. We rely on the current
// external behavior where Store.processRequestQueue (which calls Step using
// queued messages) will always return true if there were any messages that
// were stepped, even if there are errors. By returning true, the
// raftScheduler will call processReady during the same processing pass for
// the replica. Arguably, we could introduce a TryUpdateLeaderRaftMuLocked to
// be called from Replica.stepRaftGroup, but it does not capture all state
// transitions -- a raft group with a single member causes the replica to
// assume leadership without any messages being stepped. So we choose the
// first option to simplify the Processor interface.
//
// Locking:
//
// We *strongly* prefer methods to be called without holding
// Replica.mu, since then the callee (implementation of Processor) does not
// need to worry about (a) deadlocks, since processorImpl.mu is ordered before
// Replica.mu, (b) the amount of work it is doing under this critical section.
// The only exception is OnDescChangedLocked, where this was hard to achieve.
//
// TODO(sumeer):
// Integration notes reminders:
//
//   - Make Processor a direct member of Replica (not under raftMu), since
//     want to access it both before eval, on the eval wait path, and when the
//     proposal will be encoded. Processor becomes the definitive source of
//     the current EnabledWhenLeaderLevel.
//
//   - Keep a copy of EnabledWhenLeaderLevel under Replica.raftMu. This will
//     be initialized using the cluster version when Replica is created, and
//     the same value will be passed to ProcessorOptions. In
//     handleRaftReadyRaftMuLocked, which is called with raftMy held, cheaply
//     check whether already at the highest level and if not, read the cluster
//     version to see if ratcheting is needed. When ratcheting up from
//     NotEnabledWhenLeader, acquire Replica.mu and close
//     replicaFlowControlIntegrationImpl (RACv1).
type Processor interface {
	// OnDestroyRaftMuLocked is called when the Replica is being destroyed.
	//
	// We need to know when Replica.mu.destroyStatus is updated, so that we
	// can close, and return tokens. We do this call from
	// disconnectReplicationRaftMuLocked. Make sure this is not too late in
	// that these flow tokens may be needed by others.
	//
	// raftMu is held.
	OnDestroyRaftMuLocked(ctx context.Context)

	// SetEnabledWhenLeaderRaftMuLocked is the dynamic change corresponding to
	// ProcessorOptions.EnabledWhenLeaderLevel. The level must only be ratcheted
	// up. We call it in Replica.handleRaftReadyRaftMuLocked, before doing any
	// work (before Ready is called, since it may create a RangeController).
	// This may be a noop if the level has already been reached.
	//
	// raftMu is held.
	SetEnabledWhenLeaderRaftMuLocked(level EnabledWhenLeaderLevel)
	// GetEnabledWhenLeader returns the current level. It may be used in
	// highly concurrent settings at the leaseholder, when waiting for eval,
	// and when encoding a proposal. Note that if the leaseholder is not the
	// leader and the leader has switched to a higher level, there is no harm
	// done, since the leaseholder can continue waiting for v1 tokens and use
	// the v1 entry encoding.
	GetEnabledWhenLeader() EnabledWhenLeaderLevel

	// OnDescChangedLocked provides a possibly updated RangeDescriptor.
	//
	// Both Replica mu and raftMu are held.
	//
	// TODO(sumeer): we are currently delaying the processing caused by this
	// until HandleRaftReadyRaftMuLocked, including telling the
	// RangeController. However, RangeController.WaitForEval needs to have the
	// latest state. We need to either (a) change this
	// OnDescChangedRaftMuLocked, or (b) add a method in RangeController that
	// only updates the voting replicas used in WaitForEval, and call that
	// from OnDescChangedLocked, and do the rest of the updating later.
	OnDescChangedLocked(ctx context.Context, desc *roachpb.RangeDescriptor)

	// HandleRaftReadyRaftMuLocked corresponds to processing that happens when
	// Replica.handleRaftReadyRaftMuLocked is called. It must be called even
	// if there was no Ready, since it can be used to advance Admitted, and do
	// other processing.
	//
	// The entries slice is Ready.Entries, i.e., represent MsgStorageAppend on
	// all replicas. To stay consistent with the structure of
	// Replica.handleRaftReadyRaftMuLocked, this method only does leader
	// specific processing of entries.
	// AdmitRaftEntriesFromMsgStorageAppendRaftMuLocked does the general
	// replica processing for MsgStorageAppend.
	//
	// raftMu is held.
	HandleRaftReadyRaftMuLocked(ctx context.Context, entries []raftpb.Entry)
	// AdmitRaftEntriesFromMsgStorageAppendRaftMuLocked subjects entries to
	// admission control on a replica (leader or follower). Like
	// HandleRaftReadyRaftMuLocked, this is called from
	// Replica.handleRaftReadyRaftMuLocked. It is split off from that function
	// since it is natural to position the admission control processing when we
	// are writing to the store in Replica.handleRaftReadyRaftMuLocked. This is
	// a noop if the leader is not using the RACv2 protocol. Returns false if
	// the leader is using RACv1, in which the caller should follow the RACv1
	// admission pathway.
	//
	// raftMu is held.
	AdmitRaftEntriesFromMsgStorageAppendRaftMuLocked(
		ctx context.Context, leaderTerm uint64, entries []raftpb.Entry) bool

	// EnqueuePiggybackedAdmittedAtLeader is called at the leader when
	// receiving a piggybacked MsgAppResp that can advance a follower's
	// admitted state. The caller is responsible for scheduling on the raft
	// scheduler, such that ProcessPiggybackedAdmittedAtLeaderRaftMuLocked
	// gets called soon.
	EnqueuePiggybackedAdmittedAtLeader(msg raftpb.Message)
	// ProcessPiggybackedAdmittedAtLeaderRaftMuLocked is called to process
	// previous enqueued piggybacked MsgAppResp. Returns true if
	// HandleRaftReadyRaftMuLocked should be called.
	//
	// raftMu is held.
	ProcessPiggybackedAdmittedAtLeaderRaftMuLocked(ctx context.Context) bool

	// SideChannelForPriorityOverrideAtFollowerRaftMuLocked is called on a
	// follower to provide information about whether the leader is using the
	// RACv2 protocol, and if yes, the low-priority override, via a
	// side-channel, since we can't plumb this information directly through
	// Raft.
	//
	// raftMu is held.
	SideChannelForPriorityOverrideAtFollowerRaftMuLocked(
		info SideChannelInfoUsingRaftMessageRequest,
	)

	// AdmittedLogEntry is called when an entry is admitted. It can be called
	// synchronously from within ACWorkQueue.Admit if admission is immediate.
	AdmittedLogEntry(
		ctx context.Context, state EntryForAdmissionCallbackState,
	)
}

type processorImpl struct {
	opts ProcessorOptions

	// The fields below are accessed while holding the mutex. Lock ordering:
	// Replica.raftMu < this.mu < Replica.mu.
	mu struct {
		syncutil.Mutex

		// Transitions once from false => true when the Replica is destroyed.
		destroyed bool

		leaderID roachpb.ReplicaID
		// leaderNodeID, leaderStoreID are a function of leaderID and
		// raftMu.replicas. They are set when leaderID is non-zero and replicas
		// contains leaderID, else are 0.
		leaderNodeID  roachpb.NodeID
		leaderStoreID roachpb.StoreID
		leaseholderID roachpb.ReplicaID
		// State for advancing admitted.
		lastObservedStableIndex     uint64
		scheduledAdmittedProcessing bool
		waitingForAdmissionState    waitingForAdmissionState
		// State at a follower.
		follower struct {
			isLeaderUsingV2Protocol bool
			lowPriOverrideState     lowPriOverrideState
		}
		// State when leader, i.e., when leaderID == opts.ReplicaID, and v2
		// protocol is enabled.
		leader struct {
			enqueuedPiggybackedResponses map[roachpb.ReplicaID]raftpb.Message
			rc                           rac2.RangeController
			// Term is used to notice transitions out of leadership and back,
			// to recreate rc. It is set when rc is created, and is not
			// up-to-date if there is no rc (which can happen when using the
			// v1 protocol).
			term uint64
		}
		// Is the RACv2 protocol enabled when this replica is the leader.
		enabledWhenLeader EnabledWhenLeaderLevel
	}
	// Fields below are accessed while holding Replica.raftMu. This
	// peculiarity is only to handle the fact that OnDescChanged is called
	// with Replica.mu held.
	raftMu struct {
		raftNode RaftNode
		// replicasChanged is set to true when replicas has been updated. This
		// is used to lazily update all the state under mu that needs to use
		// the state in replicas.
		replicas        rac2.ReplicaSet
		replicasChanged bool
	}
	// Atomic value, for serving GetEnabledWhenLeader. Mirrors
	// mu.enabledWhenLeader.
	enabledWhenLeader atomic.Uint32

	v1EncodingPriorityMismatch log.EveryN
}

var _ Processor = &processorImpl{}

func NewProcessor(opts ProcessorOptions) Processor {
	p := &processorImpl{opts: opts}
	p.mu.enabledWhenLeader = opts.EnabledWhenLeaderLevel
	p.enabledWhenLeader.Store(uint32(opts.EnabledWhenLeaderLevel))
	p.v1EncodingPriorityMismatch = log.Every(time.Minute)
	return p
}

// OnDestroyRaftMuLocked implements Processor.
func (p *processorImpl) OnDestroyRaftMuLocked(ctx context.Context) {
	p.opts.Replica.RaftMuAssertHeld()
	p.mu.Lock()
	defer p.mu.Unlock()

	p.mu.destroyed = true
	p.closeLeaderStateRaftMuLockedProcLocked(ctx)

	// Release some memory.
	p.mu.waitingForAdmissionState = waitingForAdmissionState{}
	p.mu.follower.lowPriOverrideState = lowPriOverrideState{}
}

// SetEnabledWhenLeaderRaftMuLocked implements Processor.
func (p *processorImpl) SetEnabledWhenLeaderRaftMuLocked(level EnabledWhenLeaderLevel) {
	p.opts.Replica.RaftMuAssertHeld()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mu.destroyed || p.mu.enabledWhenLeader >= level {
		return
	}
	p.mu.enabledWhenLeader = level
	p.enabledWhenLeader.Store(uint32(level))
	if level != EnabledWhenLeaderV1Encoding || p.raftMu.replicas == nil {
		return
	}
	// May need to create RangeController.
	var leaderID roachpb.ReplicaID
	var myLeaderTerm uint64
	var nextUnstableIndex uint64
	func() {
		p.opts.Replica.MuLock()
		defer p.opts.Replica.MuUnlock()
		leaderID = p.raftMu.raftNode.LeaderLocked()
		if leaderID == p.opts.ReplicaID {
			myLeaderTerm = p.raftMu.raftNode.MyLeaderTermLocked()
			nextUnstableIndex = p.raftMu.raftNode.NextUnstableIndexLocked()
		}
	}()
	if leaderID == p.opts.ReplicaID {
		p.createLeaderStateRaftMuLockedProcLocked(myLeaderTerm, nextUnstableIndex)
	}
}

// GetEnabledWhenLeader implements Processor.
func (p *processorImpl) GetEnabledWhenLeader() EnabledWhenLeaderLevel {
	return EnabledWhenLeaderLevel(p.enabledWhenLeader.Load())
}

func descToReplicaSet(desc *roachpb.RangeDescriptor) rac2.ReplicaSet {
	rs := rac2.ReplicaSet{}
	for _, r := range desc.InternalReplicas {
		rs[r.ReplicaID] = r
	}
	return rs
}

// OnDescChangedLocked implements Processor.
func (p *processorImpl) OnDescChangedLocked(ctx context.Context, desc *roachpb.RangeDescriptor) {
	p.opts.Replica.RaftMuAssertHeld()
	p.opts.Replica.MuAssertHeld()
	if p.raftMu.replicas == nil {
		// Replica is initialized, in that we have a descriptor. Get the
		// RaftNode.
		p.raftMu.raftNode = p.opts.Replica.RaftNodeMuLocked()
	}
	p.raftMu.replicas = descToReplicaSet(desc)
	p.raftMu.replicasChanged = true
}

// makeStateConsistentRaftMuLockedProcLocked, uses the union of the latest
// state retrieved from RaftNode, and the set of replica (in raftMu.replicas),
// to initialize or update the internal state of processorImpl.
//
// nextUnstableIndex is used to initialize the state of the send-queues if
// this replica is becoming the leader. This index must immediately precede
// the entries provided to RangeController.
func (p *processorImpl) makeStateConsistentRaftMuLockedProcLocked(
	ctx context.Context,
	nextUnstableIndex uint64,
	leaderID roachpb.ReplicaID,
	leaseholderID roachpb.ReplicaID,
	myLeaderTerm uint64,
) {
	replicasChanged := p.raftMu.replicasChanged
	if replicasChanged {
		p.raftMu.replicasChanged = false
	}
	if !replicasChanged && leaderID == p.mu.leaderID && leaseholderID == p.mu.leaseholderID &&
		(p.mu.leader.rc == nil || p.mu.leader.term == myLeaderTerm) {
		// Common case.
		return
	}
	// The leader or leaseholder or replicas or myLeaderTerm changed. We set
	// everything.
	p.mu.leaderID = leaderID
	p.mu.leaseholderID = leaseholderID
	// Set leaderNodeID, leaderStoreID.
	if p.mu.leaderID == 0 {
		p.mu.leaderNodeID = 0
		p.mu.leaderStoreID = 0
	} else {
		rd, ok := p.raftMu.replicas[leaderID]
		if !ok {
			if leaderID == p.opts.ReplicaID {
				// Is leader, but not in the set of replicas. We expect this
				// should not be happening anymore, due to
				// raft.Config.StepDownOnRemoval being set to true. But we
				// tolerate it.
				log.Errorf(ctx,
					"leader=%d is not in the set of replicas=%v",
					leaderID, p.raftMu.replicas)
				p.mu.leaderNodeID = p.opts.NodeID
				p.mu.leaderStoreID = p.opts.StoreID
			} else {
				// A follower, which can learn about a leader before it learns
				// about a config change that includes the leader in the set
				// of replicas, so ignore.
				p.mu.leaderNodeID = 0
				p.mu.leaderStoreID = 0
			}
		} else {
			p.mu.leaderNodeID = rd.NodeID
			p.mu.leaderStoreID = rd.StoreID
		}
	}
	if p.mu.leaderID != p.opts.ReplicaID {
		if p.mu.leader.rc != nil {
			// Transition from leader to follower.
			p.closeLeaderStateRaftMuLockedProcLocked(ctx)
		}
		return
	}
	// Is the leader.
	if p.mu.enabledWhenLeader == NotEnabledWhenLeader {
		return
	}
	if p.mu.leader.rc != nil && myLeaderTerm > p.mu.leader.term {
		// Need to recreate the RangeController.
		p.closeLeaderStateRaftMuLockedProcLocked(ctx)
	}
	if p.mu.leader.rc == nil {
		p.createLeaderStateRaftMuLockedProcLocked(myLeaderTerm, nextUnstableIndex)
		return
	}
	// Existing RangeController.
	if replicasChanged {
		if err := p.mu.leader.rc.SetReplicasRaftMuLocked(ctx, p.raftMu.replicas); err != nil {
			log.Errorf(ctx, "error setting replicas: %v", err)
		}
	}
	p.mu.leader.rc.SetLeaseholderRaftMuLocked(ctx, leaseholderID)
}

func (p *processorImpl) closeLeaderStateRaftMuLockedProcLocked(ctx context.Context) {
	if p.mu.leader.rc == nil {
		return
	}
	p.mu.leader.rc.CloseRaftMuLocked(ctx)
	p.mu.leader.rc = nil
	p.mu.leader.enqueuedPiggybackedResponses = nil
	p.mu.leader.term = 0
}

func (p *processorImpl) createLeaderStateRaftMuLockedProcLocked(
	term uint64, nextUnstableIndex uint64,
) {
	if p.mu.leader.rc != nil {
		panic("RangeController already exists")
	}
	p.mu.leader.rc = p.opts.RangeControllerFactory.New(rangeControllerInitState{
		replicaSet:    p.raftMu.replicas,
		leaseholder:   p.mu.leaseholderID,
		nextRaftIndex: nextUnstableIndex,
	})
	p.mu.leader.term = term
	p.mu.leader.enqueuedPiggybackedResponses = map[roachpb.ReplicaID]raftpb.Message{}
}

// HandleRaftReadyRaftMuLocked implements Processor.
func (p *processorImpl) HandleRaftReadyRaftMuLocked(ctx context.Context, entries []raftpb.Entry) {
	p.opts.Replica.RaftMuAssertHeld()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mu.destroyed {
		return
	}
	if p.raftMu.raftNode == nil {
		if buildutil.CrdbTestBuild {
			if len(entries) > 0 {
				panic(errors.AssertionFailedf("entries provided without raft node"))
			}
		}
		return
	}
	// NB: we need to call makeStateConsistentRaftMuLockedProcLocked even if
	// NotEnabledWhenLeader, since this replica could be a follower and the
	// leader may switch to v2.

	// Grab the state we need in one shot after acquiring Replica mu.
	var nextUnstableIndex, stableIndex uint64
	var leaderID, leaseholderID roachpb.ReplicaID
	var admitted [raftpb.NumPriorities]uint64
	var myLeaderTerm uint64
	func() {
		p.opts.Replica.MuLock()
		defer p.opts.Replica.MuUnlock()
		nextUnstableIndex = p.raftMu.raftNode.NextUnstableIndexLocked()
		stableIndex = p.raftMu.raftNode.StableIndexLocked()
		leaderID = p.raftMu.raftNode.LeaderLocked()
		leaseholderID = p.opts.Replica.LeaseholderMuLocked()
		admitted = p.raftMu.raftNode.GetAdmittedLocked()
		if leaderID == p.opts.ReplicaID {
			myLeaderTerm = p.raftMu.raftNode.MyLeaderTermLocked()
		}
	}()
	if len(entries) > 0 {
		nextUnstableIndex = entries[0].Index
	}
	p.mu.lastObservedStableIndex = stableIndex
	p.mu.scheduledAdmittedProcessing = false
	p.makeStateConsistentRaftMuLockedProcLocked(
		ctx, nextUnstableIndex, leaderID, leaseholderID, myLeaderTerm)

	isLeaderUsingV2 := p.mu.leader.rc != nil || p.mu.follower.isLeaderUsingV2Protocol
	if !isLeaderUsingV2 {
		return
	}
	// If there was a recent MsgStoreAppendResp that triggered this Ready
	// processing, it has already been stepped, so the stable index would have
	// advanced. So this is an opportune place to do Admitted processing.
	nextAdmitted := p.mu.waitingForAdmissionState.computeAdmitted(stableIndex)
	if admittedIncreased(admitted, nextAdmitted) {
		p.opts.Replica.MuLock()
		msgResp := p.raftMu.raftNode.SetAdmittedLocked(nextAdmitted)
		p.opts.Replica.MuUnlock()
		if p.mu.leader.rc == nil && p.mu.leaderNodeID != 0 {
			// Follower, and know leaderNodeID, leaderStoreID.
			p.opts.AdmittedPiggybacker.AddMsgAppRespForLeader(
				p.mu.leaderNodeID, p.mu.leaderStoreID, p.opts.RangeID, msgResp)
		}
		// Else if the local replica is the leader, we have already told it
		// about the update by calling SetAdmittedLocked. If the leader is not
		// known, we simply drop the message.
	}
	if p.mu.leader.rc != nil {
		if err := p.mu.leader.rc.HandleRaftEventRaftMuLocked(ctx, rac2.RaftEvent{
			Entries: entries,
		}); err != nil {
			log.Errorf(ctx, "error handling raft event: %v", err)
		}
	}
}

// AdmitRaftEntriesFromMsgStorageAppendRaftMuLocked implements Processor.
func (p *processorImpl) AdmitRaftEntriesFromMsgStorageAppendRaftMuLocked(
	ctx context.Context, leaderTerm uint64, entries []raftpb.Entry,
) bool {
	// NB: the state being read here is only modified under raftMu, so it will
	// not become stale during this method.
	var isLeaderUsingV2Protocol bool
	func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		isLeaderUsingV2Protocol = !p.mu.destroyed &&
			(p.mu.leader.rc != nil || p.mu.follower.isLeaderUsingV2Protocol)
	}()
	if !isLeaderUsingV2Protocol {
		return false
	}
	for _, entry := range entries {
		typ, priBits, err := raftlog.EncodingOf(entry)
		if err != nil {
			panic(errors.Wrap(err, "unable to determine raft command encoding"))
		}
		if !typ.UsesAdmissionControl() {
			continue // nothing to do
		}
		isV2Encoding := typ == raftlog.EntryEncodingStandardWithACAndPriority ||
			typ == raftlog.EntryEncodingSideloadedWithACAndPriority
		meta, err := raftlog.DecodeRaftAdmissionMeta(entry.Data)
		if err != nil {
			panic(errors.Wrap(err, "unable to decode raft command admission data: %v"))
		}
		var raftPri raftpb.Priority
		if isV2Encoding {
			raftPri = raftpb.Priority(meta.AdmissionPriority)
			if raftPri != priBits {
				panic(errors.AssertionFailedf("inconsistent priorities %s, %s", raftPri, priBits))
			}
			func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				raftPri = p.mu.follower.lowPriOverrideState.getEffectivePriority(entry.Index, raftPri)
				p.mu.waitingForAdmissionState.add(leaderTerm, entry.Index, raftPri)
			}()
		} else {
			raftPri = raftpb.LowPri
			if admissionpb.WorkClassFromPri(admissionpb.WorkPriority(meta.AdmissionPriority)) ==
				admissionpb.RegularWorkClass && p.v1EncodingPriorityMismatch.ShouldLog() {
				log.Errorf(ctx,
					"do not use RACv1 for pri %s, which is regular work",
					admissionpb.WorkPriority(meta.AdmissionPriority))
			}
			func() {
				p.mu.Lock()
				defer p.mu.Unlock()
				p.mu.waitingForAdmissionState.add(leaderTerm, entry.Index, raftPri)
			}()
		}
		admissionPri := rac2.RaftToAdmissionPriority(raftPri)
		// NB: cannot hold mu when calling Admit since the callback may
		// execute from inside Admit, when the entry is immediately admitted.
		p.opts.ACWorkQueue.Admit(ctx, EntryForAdmission{
			TenantID:       p.opts.TenantID,
			Priority:       admissionPri,
			CreateTime:     meta.AdmissionCreateTime,
			RequestedCount: int64(len(entry.Data)),
			Ingested:       typ.IsSideloaded(),
			CallbackState: EntryForAdmissionCallbackState{
				StoreID:    p.opts.StoreID,
				RangeID:    p.opts.RangeID,
				ReplicaID:  p.opts.ReplicaID,
				LeaderTerm: leaderTerm,
				Index:      entry.Index,
				Priority:   raftPri,
			},
		})
	}
	return true
}

// EnqueuePiggybackedAdmittedAtLeader implements Processor.
func (p *processorImpl) EnqueuePiggybackedAdmittedAtLeader(msg raftpb.Message) {
	if roachpb.ReplicaID(msg.To) != p.opts.ReplicaID {
		// Ignore message to a stale ReplicaID.
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mu.leader.rc == nil {
		return
	}
	// Only need to keep the latest message from a replica.
	p.mu.leader.enqueuedPiggybackedResponses[roachpb.ReplicaID(msg.From)] = msg
}

// ProcessPiggybackedAdmittedAtLeaderRaftMuLocked implements Processor.
func (p *processorImpl) ProcessPiggybackedAdmittedAtLeaderRaftMuLocked(ctx context.Context) bool {
	p.opts.Replica.RaftMuAssertHeld()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mu.destroyed || len(p.mu.leader.enqueuedPiggybackedResponses) == 0 || p.raftMu.raftNode == nil {
		return false
	}
	p.opts.Replica.MuLock()
	defer p.opts.Replica.MuUnlock()
	for k, m := range p.mu.leader.enqueuedPiggybackedResponses {
		err := p.raftMu.raftNode.StepMsgAppRespForAdmittedLocked(m)
		if err != nil {
			log.Errorf(ctx, "%s", err)
		}
		delete(p.mu.leader.enqueuedPiggybackedResponses, k)
	}
	return true
}

// SideChannelForPriorityOverrideAtFollowerRaftMuLocked implements Processor.
func (p *processorImpl) SideChannelForPriorityOverrideAtFollowerRaftMuLocked(
	info SideChannelInfoUsingRaftMessageRequest,
) {
	p.opts.Replica.RaftMuAssertHeld()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mu.destroyed {
		return
	}
	if info.UsingV2Protocol {
		if p.mu.follower.lowPriOverrideState.sideChannelForLowPriOverride(
			info.LeaderTerm, info.First, info.Last, info.LowPriOverride) &&
			!p.mu.follower.isLeaderUsingV2Protocol {
			// Either term advanced, or stayed the same. In the latter case we know
			// that a leader does a one-way switch from v1 => v2. In the former case
			// we of course use v2 if the leader is claiming to use v2.
			p.mu.follower.isLeaderUsingV2Protocol = true
		}
	} else {
		if p.mu.follower.lowPriOverrideState.sideChannelForV1Leader(info.LeaderTerm) &&
			p.mu.follower.isLeaderUsingV2Protocol {
			// Leader term advanced, so this is switching back to v1.
			p.mu.follower.isLeaderUsingV2Protocol = false
		}
	}
}

// AdmittedLogEntry implements Processor.
func (p *processorImpl) AdmittedLogEntry(
	ctx context.Context, state EntryForAdmissionCallbackState,
) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.mu.destroyed || state.ReplicaID != p.opts.ReplicaID {
		return
	}
	admittedMayAdvance :=
		p.mu.waitingForAdmissionState.remove(state.LeaderTerm, state.Index, state.Priority)
	if !admittedMayAdvance || state.Index > p.mu.lastObservedStableIndex ||
		(p.mu.leader.rc == nil && !p.mu.follower.isLeaderUsingV2Protocol) {
		return
	}
	// The lastObservedStableIndex has moved at or ahead of state.Index. This
	// will happen when admission is not immediate. In this case we need to
	// schedule processing.
	if !p.mu.scheduledAdmittedProcessing {
		p.mu.scheduledAdmittedProcessing = true
		p.opts.RaftScheduler.EnqueueRaftReady(p.opts.RangeID)
	}
}

func admittedIncreased(prev, next [raftpb.NumPriorities]uint64) bool {
	for i := range prev {
		if prev[i] < next[i] {
			return true
		}
	}
	return false
}
