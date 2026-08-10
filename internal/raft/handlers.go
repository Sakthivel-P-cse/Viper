package raft

import (
	"bytes"
	"context"
	"fmt"
	"hash/crc32"
	"math"
)

var raftChecksumTable = crc32.MakeTable(crc32.Castagnoli)

// HandleRequestVote implements the Raft election restriction and one-vote-per
// term rule. Durable term/vote changes complete before a granted response.
func (n *Node) HandleRequestVote(_ context.Context, request RequestVoteRequest) (RequestVoteResponse, error) {
	if n == nil {
		return RequestVoteResponse{}, ErrUnavailable
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.readyLocked(); err != nil {
		return RequestVoteResponse{Term: n.term}, err
	}
	if err := n.validateRequestVoteLocked(request); err != nil {
		return RequestVoteResponse{Term: n.term}, err
	}
	if request.Term < n.term {
		return RequestVoteResponse{Term: n.term}, nil
	}
	if request.Term > n.term {
		if err := n.becomeFollowerTermLocked(request.Term); err != nil {
			return RequestVoteResponse{Term: n.term}, err
		}
	}
	upToDate := request.LastLogTerm > n.lastLogTermLocked() ||
		(request.LastLogTerm == n.lastLogTermLocked() && request.LastLogIndex >= n.lastLogIndexLocked())
	if !upToDate || n.votedFor != "" && n.votedFor != request.CandidateID {
		return RequestVoteResponse{Term: n.term}, nil
	}
	if n.votedFor != request.CandidateID {
		hardState := n.hardStateLocked(n.commitIndex)
		hardState.VotedFor = request.CandidateID
		if err := n.persistLocked(PersistBatch{HardState: &hardState}); err != nil {
			return RequestVoteResponse{Term: n.term}, err
		}
		n.votedFor = request.CandidateID
	}
	n.role = RoleFollower
	n.leaderID = ""
	n.resetElectionLocked()
	return RequestVoteResponse{Term: n.term, VoteGranted: true}, nil
}

// HandleAppendEntries implements log matching, conflict hints, commitment,
// and ordered application for a leader's contiguous suffix.
func (n *Node) HandleAppendEntries(_ context.Context, request AppendEntriesRequest) (AppendEntriesResponse, error) {
	if n == nil {
		return AppendEntriesResponse{}, ErrUnavailable
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.readyLocked(); err != nil {
		return AppendEntriesResponse{Term: n.term}, err
	}
	if err := n.validateAppendEntriesLocked(request); err != nil {
		return AppendEntriesResponse{Term: n.term}, err
	}
	if request.Term < n.term {
		return AppendEntriesResponse{Term: n.term}, nil
	}
	if request.Term > n.term {
		if err := n.becomeFollowerTermLocked(request.Term); err != nil {
			return AppendEntriesResponse{Term: n.term}, err
		}
	} else if n.role != RoleFollower {
		n.role = RoleFollower
		n.votes = nil
	}
	n.leaderID = request.LeaderID
	n.resetElectionLocked()

	if !n.previousMatchesLocked(request.PrevLogIndex, request.PrevLogTerm) {
		conflictIndex, conflictTerm := n.conflictHintLocked(request.PrevLogIndex, request.PrevLogTerm)
		return AppendEntriesResponse{Term: n.term, ConflictIndex: conflictIndex, ConflictTerm: conflictTerm}, nil
	}

	truncateFrom, suffix := n.logChangesLocked(request.Entries)
	if truncateFrom != nil && *truncateFrom <= n.commitIndex {
		conflictIndex := n.commitIndex
		if conflictIndex < Index(math.MaxUint64) {
			conflictIndex++
		}
		return AppendEntriesResponse{Term: n.term, ConflictIndex: conflictIndex}, nil
	}
	newLast := n.lastLogIndexLocked()
	if len(suffix) > 0 {
		newLast = suffix[len(suffix)-1].Index
	} else if truncateFrom != nil {
		newLast = truncateFromValue(*truncateFrom, n)
	}
	commitTarget := request.LeaderCommit
	if commitTarget > newLast {
		commitTarget = newLast
	}
	if commitTarget < n.commitIndex {
		commitTarget = n.commitIndex
	}
	changedLog := truncateFrom != nil || len(suffix) > 0
	if changedLog || commitTarget != n.commitIndex {
		hardState := n.hardStateLocked(commitTarget)
		batch := PersistBatch{HardState: &hardState, Entries: suffix}
		if truncateFrom != nil {
			value := *truncateFrom
			batch.TruncateFrom = &value
		}
		if err := n.persistLocked(batch); err != nil {
			return AppendEntriesResponse{Term: n.term}, err
		}
		if changedLog {
			n.applyLogChangesLocked(truncateFrom, suffix)
		}
		n.commitIndex = commitTarget
	}
	if err := n.applyCommittedLocked(); err != nil {
		return AppendEntriesResponse{Term: n.term}, err
	}
	matchIndex := request.PrevLogIndex + Index(len(request.Entries))
	return AppendEntriesResponse{Term: n.term, Success: true, MatchIndex: matchIndex}, nil
}

// HandleInstallSnapshot validates snapshot chunk integrity. Full snapshot
// activation is deliberately delegated to the snapshot integration layer,
// because the StableStore contract requires a committed matching log prefix.
func (n *Node) HandleInstallSnapshot(_ context.Context, request InstallSnapshotRequest) (InstallSnapshotResponse, error) {
	if n == nil {
		return InstallSnapshotResponse{}, ErrUnavailable
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	if err := n.readyLocked(); err != nil {
		return InstallSnapshotResponse{Term: n.term}, err
	}
	if err := n.validateInstallSnapshotLocked(request); err != nil {
		return InstallSnapshotResponse{Term: n.term}, err
	}
	if request.Term < n.term {
		return InstallSnapshotResponse{Term: n.term, NextOffset: request.Offset}, nil
	}
	if request.Term > n.term {
		if err := n.becomeFollowerTermLocked(request.Term); err != nil {
			return InstallSnapshotResponse{Term: n.term}, err
		}
	}
	n.role = RoleFollower
	n.leaderID = request.LeaderID
	n.resetElectionLocked()
	if n.snapshot != nil && request.LastIncludedIndex <= n.snapshot.Index {
		return InstallSnapshotResponse{Term: n.term, NextOffset: request.Offset + uint64(len(request.Data))}, nil
	}
	if request.LastIncludedIndex < n.commitIndex {
		return InstallSnapshotResponse{Term: n.term, NextOffset: request.Offset}, fmt.Errorf("%w: snapshot regresses committed progress", ErrInvalidMessage)
	}
	if n.incomingSnapshot == nil || request.Offset == 0 ||
		n.incomingSnapshot.index != request.LastIncludedIndex ||
		n.incomingSnapshot.term != request.LastIncludedTerm ||
		n.incomingSnapshot.leader != request.LeaderID {
		if request.Offset != 0 {
			nextOffset := uint64(0)
			if n.incomingSnapshot != nil {
				nextOffset = uint64(len(n.incomingSnapshot.data))
			}
			return InstallSnapshotResponse{Term: n.term, NextOffset: nextOffset}, nil
		}
		n.incomingSnapshot = &snapshotAssembly{index: request.LastIncludedIndex, term: request.LastIncludedTerm, leader: request.LeaderID}
	}
	expectedOffset := uint64(len(n.incomingSnapshot.data))
	if request.Offset != expectedOffset {
		return InstallSnapshotResponse{Term: n.term, NextOffset: expectedOffset}, nil
	}
	if uint64(len(n.incomingSnapshot.data))+uint64(len(request.Data)) > maxSnapshotBytes {
		return InstallSnapshotResponse{Term: n.term, NextOffset: expectedOffset}, fmt.Errorf("%w: snapshot exceeds maximum size", ErrInvalidMessage)
	}
	n.incomingSnapshot.data = append(n.incomingSnapshot.data, request.Data...)
	nextOffset := uint64(len(n.incomingSnapshot.data))
	if !request.Done {
		return InstallSnapshotResponse{Term: n.term, NextOffset: nextOffset}, nil
	}
	snapshot := Snapshot{Index: n.incomingSnapshot.index, Term: n.incomingSnapshot.term, Data: append([]byte(nil), n.incomingSnapshot.data...)}
	commitIndex := n.commitIndex
	if commitIndex < snapshot.Index {
		commitIndex = snapshot.Index
	}
	if n.apply != nil && n.restore == nil {
		return InstallSnapshotResponse{Term: n.term, NextOffset: nextOffset}, fmt.Errorf("%w: state-machine restore is unavailable", ErrUnavailable)
	}
	hardState := n.hardStateLocked(commitIndex)
	if n.store != nil {
		store, ok := n.store.(SnapshotStore)
		if !ok {
			return InstallSnapshotResponse{Term: n.term, NextOffset: nextOffset}, fmt.Errorf("%w: durable snapshot installation is unavailable", ErrUnavailable)
		}
		if err := store.InstallSnapshot(snapshot, hardState); err != nil {
			return InstallSnapshotResponse{Term: n.term, NextOffset: nextOffset}, n.failLocked(err)
		}
	}
	if n.restore != nil {
		if err := n.restore(*cloneSnapshot(&snapshot)); err != nil {
			return InstallSnapshotResponse{Term: n.term, NextOffset: nextOffset}, n.failLocked(err)
		}
	}
	localTerm, localTermFound := n.termAtLocked(snapshot.Index)
	if localTermFound && localTerm == snapshot.Term {
		n.log = retainedEntriesAfterLocked(n.log, n.snapshot, snapshot.Index)
	} else {
		n.log = nil
	}
	n.snapshot = cloneSnapshot(&snapshot)
	n.commitIndex = commitIndex
	n.lastApplied = snapshot.Index
	n.incomingSnapshot = nil
	return InstallSnapshotResponse{Term: n.term, NextOffset: nextOffset}, nil
}

func (n *Node) validateRequestVoteLocked(request RequestVoteRequest) error {
	if request.Term == 0 || !n.isPeerLocked(request.CandidateID) {
		return fmt.Errorf("%w: invalid vote request identity or term", ErrInvalidMessage)
	}
	if !validLogPosition(request.LastLogIndex, request.LastLogTerm) {
		return fmt.Errorf("%w: invalid last log position", ErrInvalidMessage)
	}
	return nil
}

func (n *Node) validateAppendEntriesLocked(request AppendEntriesRequest) error {
	if request.Term == 0 || !n.isPeerLocked(request.LeaderID) || !validLogPosition(request.PrevLogIndex, request.PrevLogTerm) {
		return fmt.Errorf("%w: invalid append identity, term, or previous position", ErrInvalidMessage)
	}
	if len(request.Entries) > maxAppendEntries {
		return fmt.Errorf("%w: append batch is too large", ErrInvalidMessage)
	}
	expected := request.PrevLogIndex
	for _, entry := range request.Entries {
		if expected == Index(math.MaxUint64) || entry.Index != expected+1 || entry.Term == 0 || entry.Term > request.Term || len(entry.Command) > maxCommandBytes {
			return fmt.Errorf("%w: append entries are invalid or non-contiguous", ErrInvalidMessage)
		}
		expected++
	}
	return nil
}

func (n *Node) validateInstallSnapshotLocked(request InstallSnapshotRequest) error {
	if request.Term == 0 || !n.isPeerLocked(request.LeaderID) || request.LastIncludedIndex == 0 || request.LastIncludedTerm == 0 {
		return fmt.Errorf("%w: invalid snapshot metadata", ErrInvalidMessage)
	}
	if request.LastIncludedTerm > request.Term {
		return fmt.Errorf("%w: snapshot term exceeds leader term", ErrInvalidMessage)
	}
	if len(request.Data) > maxSnapshotChunk || request.Offset > math.MaxUint64-uint64(len(request.Data)) {
		return fmt.Errorf("%w: snapshot chunk is too large", ErrInvalidMessage)
	}
	if crc32.Checksum(request.Data, raftChecksumTable) != request.Checksum {
		return fmt.Errorf("%w: snapshot checksum mismatch", ErrInvalidMessage)
	}
	return nil
}

func retainedEntriesAfterLocked(entries []Entry, snapshot *Snapshot, index Index) []Entry {
	base := Index(0)
	if snapshot != nil {
		base = snapshot.Index
	}
	position := int(index - base)
	if position < 0 {
		return nil
	}
	if position >= len(entries) {
		return nil
	}
	return cloneEntries(entries[position:])
}

func validLogPosition(index Index, term Term) bool { return (index == 0) == (term == 0) }

func (n *Node) isPeerLocked(id NodeID) bool {
	_, ok := n.peerSet[id]
	return ok
}

func (n *Node) previousMatchesLocked(index Index, term Term) bool {
	actual, found := n.termAtLocked(index)
	return found && actual == term
}

func (n *Node) conflictHintLocked(index Index, term Term) (Index, Term) {
	last := n.lastLogIndexLocked()
	if index > last {
		if last == Index(math.MaxUint64) {
			return last, n.lastLogTermLocked()
		}
		return last + 1, 0
	}
	actual, found := n.termAtLocked(index)
	if !found {
		return last + 1, 0
	}
	first := index
	for first > 1 {
		previous, ok := n.termAtLocked(first - 1)
		if !ok || previous != actual {
			break
		}
		first--
	}
	_ = term
	return first, actual
}

func (n *Node) logChangesLocked(entries []Entry) (*Index, []Entry) {
	for position, incoming := range entries {
		existing, found := n.entryAtLocked(incoming.Index)
		if !found {
			suffix := cloneEntries(entries[position:])
			return nil, suffix
		}
		if existing.Term != incoming.Term || !bytes.Equal(existing.Command, incoming.Command) {
			from := incoming.Index
			suffix := cloneEntries(entries[position:])
			return &from, suffix
		}
	}
	return nil, nil
}

func cloneEntries(entries []Entry) []Entry {
	if len(entries) == 0 {
		return nil
	}
	cloned := make([]Entry, len(entries))
	for i, entry := range entries {
		cloned[i] = cloneEntry(entry)
	}
	return cloned
}

func (n *Node) applyLogChangesLocked(truncateFrom *Index, suffix []Entry) {
	if truncateFrom != nil {
		base := Index(0)
		if n.snapshot != nil {
			base = n.snapshot.Index
		}
		position := int(*truncateFrom - base - 1)
		if position < 0 {
			position = 0
		}
		if position < len(n.log) {
			clear(n.log[position:])
			n.log = n.log[:position]
		}
	}
	n.log = append(n.log, cloneEntries(suffix)...)
}

func truncateFromValue(from Index, n *Node) Index {
	base := Index(0)
	if n.snapshot != nil {
		base = n.snapshot.Index
	}
	position := int(from - base - 1)
	if position <= 0 {
		return base
	}
	if position > len(n.log) {
		return n.lastLogIndexLocked()
	}
	return n.log[position-1].Index
}
