// Copyright 2024 The Cockroach Authors.
//
// Use of this software is governed by the Business Source License
// included in the file licenses/BSL.txt.
//
// As of the Change Date specified in that file, in accordance with
// the Business Source License, use of this software will be governed
// by the Apache License, Version 2.0, included in the file
// licenses/APL.txt.

package storeliveness

import (
	"sync/atomic"

	slpb "github.com/cockroachdb/cockroach/pkg/kv/kvserver/storeliveness/storelivenesspb"
	"github.com/cockroachdb/cockroach/pkg/util/hlc"
	"github.com/cockroachdb/cockroach/pkg/util/syncutil"
)

// supporterState stores the core data structures for providing support.
type supporterState struct {
	// meta stores the SupporterMeta, including the max timestamp at which this
	// store has withdrawn support.
	meta slpb.SupporterMeta
	// supportFor stores the SupportState for each remote store for which this
	// store has provided support.
	supportFor map[slpb.StoreIdent]slpb.SupportState
}

// supporterStateHandler is the main interface for handling support for other
// stores. The typical interactions with supporterStateHandler are:
//   - getSupportFor(id slpb.StoreIdent)
//   - ssfu := checkOutUpdate()
//     ssfu.handleHeartbeat(msg slpb.Message)
//     checkInUpdate(ssfu)
//   - ssfu := checkOutUpdate()
//     ssfu.withdrawSupport(now hlc.ClockTimestamp)
//     checkInUpdate(ssfu)
//
// Only one update can be in progress to ensure that multiple mutation methods
// are not run concurrently.
//
// Adding a store to support is done automatically when a heartbeat from that
// store is first received. Currently, a store is never removed.
type supporterStateHandler struct {
	// supporterState is the source of truth for provided support.
	supporterState supporterState
	// mu controls access to supporterState. The access pattern to supporterState
	// is single writer, multi reader. Concurrent reads come from API calls to
	// SupportFor; these require RLocking mu. Updates to supporterState are done
	// from a single goroutine; these require Locking mu when writing the updates.
	// These updates also read from supporterState but there is no need to RLock
	// mu during these reads (since there are no concurrent writes).
	mu syncutil.RWMutex
	// update is a reference to an in-progress change in supporterStateForUpdate.
	// A non-nil update implies there is no ongoing update; i.e. the referenced
	// requesterStateForUpdate is available to be checked out.
	update atomic.Pointer[supporterStateForUpdate]
}

func newSupporterStateHandler() *supporterStateHandler {
	ssh := &supporterStateHandler{
		supporterState: supporterState{
			meta:       slpb.SupporterMeta{},
			supportFor: make(map[slpb.StoreIdent]slpb.SupportState),
		},
	}
	ssh.update.Store(
		&supporterStateForUpdate{
			checkedIn: &ssh.supporterState,
			inProgress: supporterState{
				meta:       slpb.SupporterMeta{},
				supportFor: make(map[slpb.StoreIdent]slpb.SupportState),
			},
		},
	)
	return ssh
}

// supporterStateForUpdate is a helper struct that facilitates updates to
// supporterState. It is necessary only for batch updates where the individual
// updates need to see each other's changes, while concurrent calls to
// SupportFor see the persisted-to-disk view until the in-progress batch is
// successfully persisted.
type supporterStateForUpdate struct {
	// checkedIn is a reference to the original supporterState struct stored in
	// supporterStateHandler. It is used to respond to calls to SupportFor (while
	// an update is in progress) to provide a response consistent with the state
	// persisted on disk.
	checkedIn *supporterState
	// inProgress holds all the updates to supporterState that are in progress and
	// have not yet been reflected in the checkedIn view. The inProgress view
	// ensures that ongoing updates from the same batch see each other's changes.
	inProgress supporterState
}

// getSupportFor returns the SupportState corresponding to the given store in
// supporterState.supportFor.
func (ssh *supporterStateHandler) getSupportFor(id slpb.StoreIdent) slpb.SupportState {
	ssh.mu.RLock()
	defer ssh.mu.RUnlock()
	return ssh.supporterState.supportFor[id]
}

// Functions for handling supporterState updates.

// getMeta returns the SupporterMeta from the inProgress view; if not present,
// it falls back to the SupporterMeta from the checkedIn view.
func (ssfu *supporterStateForUpdate) getMeta() slpb.SupporterMeta {
	if ssfu.inProgress.meta != (slpb.SupporterMeta{}) {
		return ssfu.inProgress.meta
	}
	return ssfu.checkedIn.meta
}

// getSupportFor returns the SupportState from the inProgress view; if not
// present, it falls back to the SupportState from the checkedIn view.
// The returned boolean indicates whether the store is present in the supportFor
// map; it does NOT indicate whether support is provided.
func (ssfu *supporterStateForUpdate) getSupportFor(
	storeID slpb.StoreIdent,
) (slpb.SupportState, bool) {
	ss, ok := ssfu.inProgress.supportFor[storeID]
	if !ok {
		ss, ok = ssfu.checkedIn.supportFor[storeID]
	}
	return ss, ok
}

// reset clears the inProgress view of supporterStateForUpdate.
func (ssfu *supporterStateForUpdate) reset() {
	ssfu.inProgress.meta = slpb.SupporterMeta{}
	clear(ssfu.inProgress.supportFor)
}

// checkOutUpdate returns the supporterStateForUpdate referenced in
// supporterStateHandler.update and replaces it with a nil pointer to ensure it
// cannot be checked out concurrently as part of another mutation.
func (ssh *supporterStateHandler) checkOutUpdate() *supporterStateForUpdate {
	ssfu := ssh.update.Swap(nil)
	if ssfu == nil {
		panic("unsupported concurrent update")
	}
	return ssfu
}

// checkInUpdate updates the checkedIn view of supporterStateForUpdate with any
// updates from the inProgress view. It clears the inProgress view, and swaps it
// back in supporterStateHandler.update to be checked out by future updates.
func (ssh *supporterStateHandler) checkInUpdate(ssfu *supporterStateForUpdate) {
	defer func() {
		ssfu.reset()
		ssh.update.Swap(ssfu)
	}()
	if ssfu.inProgress.meta == (slpb.SupporterMeta{}) && len(ssfu.inProgress.supportFor) == 0 {
		return
	}
	ssh.mu.Lock()
	defer ssh.mu.Unlock()
	if ssfu.inProgress.meta != (slpb.SupporterMeta{}) {
		if !ssfu.inProgress.meta.MaxWithdrawn.IsEmpty() {
			ssfu.checkedIn.meta.MaxWithdrawn = ssfu.inProgress.meta.MaxWithdrawn
		}
	}
	for storeID, ss := range ssfu.inProgress.supportFor {
		ssfu.checkedIn.supportFor[storeID] = ss
	}
}

// Functions for handling heartbeats.

// handleHeartbeat handles a single heartbeat message. It updates the inProgress
// view of supporterStateForUpdate only if there are any changes, and returns
// a heartbeat response message.
func (ssfu *supporterStateForUpdate) handleHeartbeat(msg slpb.Message) slpb.Message {
	from := msg.From
	ss, ok := ssfu.getSupportFor(from)
	if !ok {
		ss = slpb.SupportState{Target: from}
	}
	ssNew := handleHeartbeat(ss, msg)
	if ss != ssNew {
		ssfu.inProgress.supportFor[from] = ssNew
	}
	return slpb.Message{
		Type:       slpb.MsgHeartbeatResp,
		From:       msg.To,
		To:         msg.From,
		Epoch:      ssNew.Epoch,
		Expiration: ssNew.Expiration,
	}
}

// handleHeartbeat contains the core logic for updating the epoch and expiration
// of a support requester upon receiving a heartbeat.
func handleHeartbeat(ss slpb.SupportState, msg slpb.Message) slpb.SupportState {
	if ss.Epoch == msg.Epoch {
		ss.Expiration.Forward(msg.Expiration)
	} else if ss.Epoch < msg.Epoch {
		assert(
			ss.Expiration.Less(msg.Expiration), "support expiration regression across epochs",
		)
		ss.Epoch = msg.Epoch
		ss.Expiration = msg.Expiration
	}
	return ss
}

// Functions for withdrawing support.

// withdrawSupport handles a single support withdrawal. It updates the
// inProgress view of supporterStateForUpdate only if there are any changes.
func (ssfu *supporterStateForUpdate) withdrawSupport(now hlc.ClockTimestamp) {
	// Assert that there are no updates in ssfu.inProgress.supportFor to make
	// sure we can iterate over ssfu.checkedIn.supportFor in the loop below.
	assert(
		len(ssfu.inProgress.supportFor) == 0, "reading from supporterStateForUpdate."+
			"checkedIn.supportFor while supporterStateForUpdate.inProgress.supportFor is not empty",
	)
	for id, ss := range ssfu.checkedIn.supportFor {
		ssNew := maybeWithdrawSupport(ss, now)
		if ss != ssNew {
			ssfu.inProgress.supportFor[id] = ssNew
			if ssfu.getMeta().MaxWithdrawn.Less(now) {
				ssfu.inProgress.meta.MaxWithdrawn.Forward(now)
			}
		}
	}
}

// maybeWithdrawSupport contains the core logic for updating the epoch and
// expiration of a support requester when withdrawing support.
func maybeWithdrawSupport(ss slpb.SupportState, now hlc.ClockTimestamp) slpb.SupportState {
	if !ss.Expiration.IsEmpty() && ss.Expiration.LessEq(now.ToTimestamp()) {
		ss.Epoch++
		ss.Expiration = hlc.Timestamp{}
	}
	return ss
}
