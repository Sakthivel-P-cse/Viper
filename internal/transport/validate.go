package transport

import (
	"bytes"
	"hash/crc32"
	"math"

	"viper/internal/raft"
	"viper/pkg/contracts"
)

const appendEntryWireOverhead = 64

var checksumTable = crc32.MakeTable(crc32.Castagnoli)

func validateTarget(cluster *StaticCluster, localID, target raft.NodeID) error {
	if cluster == nil {
		return invalidCluster("cluster is nil")
	}
	if localID != cluster.LocalID() {
		return invalidCluster("transport local node does not match cluster view")
	}
	if target == localID {
		return invalidMessage("target", "must be a remote peer")
	}
	if !cluster.Contains(target) {
		return ErrUnknownPeer
	}
	return nil
}

func validateMember(cluster *StaticCluster, id raft.NodeID, field string) error {
	if err := contracts.NodeID(id).Validate(); err != nil {
		return invalidMessage(field, "is invalid")
	}
	if cluster == nil || !cluster.Contains(id) {
		return ErrUnknownPeer
	}
	return nil
}

func validateRequestVote(cluster *StaticCluster, request raft.RequestVoteRequest) error {
	if request.Term == 0 {
		return invalidMessage("term", "must be positive")
	}
	if err := validateMember(cluster, request.CandidateID, "candidate_id"); err != nil {
		return err
	}
	if err := validateLogPosition(request.LastLogIndex, request.LastLogTerm, "last_log"); err != nil {
		return err
	}
	if request.LastLogTerm > request.Term {
		return invalidMessage("last_log_term", "cannot exceed the election term")
	}
	return nil
}

func validateRequestVoteResponse(request raft.RequestVoteRequest, response raft.RequestVoteResponse) error {
	if response.Term < request.Term {
		return invalidMessage("response.term", "is older than the request term")
	}
	return nil
}

func validateAppendEntries(cluster *StaticCluster, request raft.AppendEntriesRequest) error {
	if request.Term == 0 {
		return invalidMessage("term", "must be positive")
	}
	if err := validateMember(cluster, request.LeaderID, "leader_id"); err != nil {
		return err
	}
	if err := validateLogPosition(request.PrevLogIndex, request.PrevLogTerm, "prev_log"); err != nil {
		return err
	}
	if len(request.Entries) > contracts.MaxLogEntriesPerAppend {
		return invalidMessage("entries", "exceeds the replication batch limit")
	}
	wireBytes := 128
	expected := request.PrevLogIndex
	for _, entry := range request.Entries {
		if expected == raft.Index(math.MaxUint64) {
			return invalidMessage("entries", "overflows the log index")
		}
		expected++
		if entry.Index != expected {
			return invalidMessage("entries", "must be contiguous after prev_log_index")
		}
		if entry.Term == 0 || entry.Term > request.Term {
			return invalidMessage("entries", "contains an invalid term")
		}
		if len(entry.Command) > contracts.MaxCommandBytes {
			return invalidMessage("entries", "contains an oversized command")
		}
		if wireBytes+appendEntryWireOverhead > contracts.MaxRPCMessageBytes ||
			len(entry.Command) > contracts.MaxRPCMessageBytes-wireBytes-appendEntryWireOverhead {
			return invalidMessage("entries", "exceeds the RPC message limit")
		}
		wireBytes += len(entry.Command) + appendEntryWireOverhead
	}
	return nil
}

func validateAppendEntriesResponse(request raft.AppendEntriesRequest, response raft.AppendEntriesResponse) error {
	if response.Term < request.Term {
		return invalidMessage("response.term", "is older than the request term")
	}
	expectedMatch := request.PrevLogIndex + raft.Index(len(request.Entries))
	if response.Success && response.MatchIndex != expectedMatch {
		return invalidMessage("response.match_index", "does not match the replicated suffix")
	}
	if !response.Success && response.MatchIndex != 0 {
		return invalidMessage("response.match_index", "must be empty on failure")
	}
	if response.Success && (response.ConflictIndex != 0 || response.ConflictTerm != 0) {
		return invalidMessage("response.conflict", "must be empty on success")
	}
	if response.ConflictTerm != 0 && response.ConflictIndex == 0 {
		return invalidMessage("response.conflict_index", "must accompany a conflict term")
	}
	return nil
}

func validateInstallSnapshot(cluster *StaticCluster, request raft.InstallSnapshotRequest) error {
	if request.Term == 0 {
		return invalidMessage("term", "must be positive")
	}
	if err := validateMember(cluster, request.LeaderID, "leader_id"); err != nil {
		return err
	}
	if request.LastIncludedIndex == 0 || request.LastIncludedTerm == 0 {
		return invalidMessage("last_included", "index and term must be positive")
	}
	if request.LastIncludedTerm > request.Term {
		return invalidMessage("last_included_term", "cannot exceed the leader term")
	}
	if len(request.Data) > contracts.MaxSnapshotChunkBytes {
		return invalidMessage("data", "exceeds the snapshot chunk limit")
	}
	if request.Offset > math.MaxUint64-uint64(len(request.Data)) {
		return invalidMessage("offset", "overflows the snapshot position")
	}
	if crc32.Checksum(request.Data, checksumTable) != request.Checksum {
		return invalidMessage("checksum", "does not match the snapshot chunk")
	}
	return nil
}

func validateInstallSnapshotResponse(request raft.InstallSnapshotRequest, response raft.InstallSnapshotResponse) error {
	if response.Term < request.Term {
		return invalidMessage("response.term", "is older than the request term")
	}
	end := request.Offset + uint64(len(request.Data))
	if response.NextOffset > end {
		return invalidMessage("response.next_offset", "advances beyond the transmitted chunk")
	}
	return nil
}

func validateLogPosition(index raft.Index, term raft.Term, field string) error {
	if (index == 0) != (term == 0) {
		return invalidMessage(field, "index and term must both be zero or both be positive")
	}
	return nil
}

func cloneRequestVote(request raft.RequestVoteRequest) raft.RequestVoteRequest {
	return request
}

func cloneAppendEntries(request raft.AppendEntriesRequest) raft.AppendEntriesRequest {
	clone := request
	clone.Entries = make([]raft.Entry, len(request.Entries))
	for i, entry := range request.Entries {
		clone.Entries[i] = entry
		clone.Entries[i].Command = bytes.Clone(entry.Command)
	}
	return clone
}

func cloneInstallSnapshot(request raft.InstallSnapshotRequest) raft.InstallSnapshotRequest {
	clone := request
	clone.Data = bytes.Clone(request.Data)
	return clone
}
