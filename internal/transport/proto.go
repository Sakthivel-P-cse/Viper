package transport

import (
	"encoding/binary"
	"hash/crc32"

	apiv1 "viper/api/gen/v1"
	"viper/internal/raft"
)

func entryChecksum(entry raft.Entry) uint32 {
	var header [16]byte
	binary.BigEndian.PutUint64(header[0:8], uint64(entry.Term))
	binary.BigEndian.PutUint64(header[8:16], uint64(entry.Index))
	checksum := crc32.Update(0, checksumTable, header[:])
	return crc32.Update(checksum, checksumTable, entry.Command)
}

func requestVoteToProto(request raft.RequestVoteRequest) *apiv1.RequestVoteRequest {
	return &apiv1.RequestVoteRequest{
		Term:         uint64(request.Term),
		CandidateId:  string(request.CandidateID),
		LastLogIndex: uint64(request.LastLogIndex),
		LastLogTerm:  uint64(request.LastLogTerm),
	}
}

func requestVoteFromProto(message *apiv1.RequestVoteRequest) (raft.RequestVoteRequest, error) {
	if message == nil {
		return raft.RequestVoteRequest{}, invalidMessage("request", "is nil")
	}
	return raft.RequestVoteRequest{
		Term:         raft.Term(message.GetTerm()),
		CandidateID:  raft.NodeID(message.GetCandidateId()),
		LastLogIndex: raft.Index(message.GetLastLogIndex()),
		LastLogTerm:  raft.Term(message.GetLastLogTerm()),
	}, nil
}

func requestVoteResponseToProto(response raft.RequestVoteResponse) *apiv1.RequestVoteResponse {
	return &apiv1.RequestVoteResponse{Term: uint64(response.Term), VoteGranted: response.VoteGranted}
}

func requestVoteResponseFromProto(message *apiv1.RequestVoteResponse) (raft.RequestVoteResponse, error) {
	if message == nil {
		return raft.RequestVoteResponse{}, invalidMessage("response", "is nil")
	}
	return raft.RequestVoteResponse{Term: raft.Term(message.GetTerm()), VoteGranted: message.GetVoteGranted()}, nil
}

func appendEntriesToProto(request raft.AppendEntriesRequest) *apiv1.AppendEntriesRequest {
	entries := make([]*apiv1.RaftLogEntry, len(request.Entries))
	for i, entry := range request.Entries {
		entries[i] = &apiv1.RaftLogEntry{
			Term:     uint64(entry.Term),
			Index:    uint64(entry.Index),
			Command:  append([]byte(nil), entry.Command...),
			Checksum: entryChecksum(entry),
		}
	}
	return &apiv1.AppendEntriesRequest{
		Term:         uint64(request.Term),
		LeaderId:     string(request.LeaderID),
		PrevLogIndex: uint64(request.PrevLogIndex),
		PrevLogTerm:  uint64(request.PrevLogTerm),
		Entries:      entries,
		LeaderCommit: uint64(request.LeaderCommit),
	}
}

func appendEntriesFromProto(message *apiv1.AppendEntriesRequest) (raft.AppendEntriesRequest, error) {
	if message == nil {
		return raft.AppendEntriesRequest{}, invalidMessage("request", "is nil")
	}
	request := raft.AppendEntriesRequest{
		Term:         raft.Term(message.GetTerm()),
		LeaderID:     raft.NodeID(message.GetLeaderId()),
		PrevLogIndex: raft.Index(message.GetPrevLogIndex()),
		PrevLogTerm:  raft.Term(message.GetPrevLogTerm()),
		LeaderCommit: raft.Index(message.GetLeaderCommit()),
		Entries:      make([]raft.Entry, len(message.GetEntries())),
	}
	for i, wireEntry := range message.GetEntries() {
		if wireEntry == nil {
			return raft.AppendEntriesRequest{}, invalidMessage("entries", "contains a nil entry")
		}
		entry := raft.Entry{
			Term:    raft.Term(wireEntry.GetTerm()),
			Index:   raft.Index(wireEntry.GetIndex()),
			Command: append([]byte(nil), wireEntry.GetCommand()...),
		}
		if entryChecksum(entry) != wireEntry.GetChecksum() {
			return raft.AppendEntriesRequest{}, invalidMessage("entries.checksum", "does not match the log entry")
		}
		request.Entries[i] = entry
	}
	return request, nil
}

func appendEntriesResponseToProto(response raft.AppendEntriesResponse) *apiv1.AppendEntriesResponse {
	return &apiv1.AppendEntriesResponse{
		Term:          uint64(response.Term),
		Success:       response.Success,
		MatchIndex:    uint64(response.MatchIndex),
		ConflictIndex: uint64(response.ConflictIndex),
		ConflictTerm:  uint64(response.ConflictTerm),
	}
}

func appendEntriesResponseFromProto(message *apiv1.AppendEntriesResponse) (raft.AppendEntriesResponse, error) {
	if message == nil {
		return raft.AppendEntriesResponse{}, invalidMessage("response", "is nil")
	}
	return raft.AppendEntriesResponse{
		Term:          raft.Term(message.GetTerm()),
		Success:       message.GetSuccess(),
		MatchIndex:    raft.Index(message.GetMatchIndex()),
		ConflictIndex: raft.Index(message.GetConflictIndex()),
		ConflictTerm:  raft.Term(message.GetConflictTerm()),
	}, nil
}

func installSnapshotToProto(request raft.InstallSnapshotRequest) *apiv1.InstallSnapshotRequest {
	return &apiv1.InstallSnapshotRequest{
		Term:              uint64(request.Term),
		LeaderId:          string(request.LeaderID),
		LastIncludedIndex: uint64(request.LastIncludedIndex),
		LastIncludedTerm:  uint64(request.LastIncludedTerm),
		Offset:            request.Offset,
		Data:              append([]byte(nil), request.Data...),
		Done:              request.Done,
		Checksum:          request.Checksum,
	}
}

func installSnapshotFromProto(message *apiv1.InstallSnapshotRequest) (raft.InstallSnapshotRequest, error) {
	if message == nil {
		return raft.InstallSnapshotRequest{}, invalidMessage("request", "is nil")
	}
	return raft.InstallSnapshotRequest{
		Term:              raft.Term(message.GetTerm()),
		LeaderID:          raft.NodeID(message.GetLeaderId()),
		LastIncludedIndex: raft.Index(message.GetLastIncludedIndex()),
		LastIncludedTerm:  raft.Term(message.GetLastIncludedTerm()),
		Offset:            message.GetOffset(),
		Data:              append([]byte(nil), message.GetData()...),
		Done:              message.GetDone(),
		Checksum:          message.GetChecksum(),
	}, nil
}

func installSnapshotResponseToProto(response raft.InstallSnapshotResponse) *apiv1.InstallSnapshotResponse {
	return &apiv1.InstallSnapshotResponse{Term: uint64(response.Term), NextOffset: response.NextOffset}
}

func installSnapshotResponseFromProto(message *apiv1.InstallSnapshotResponse) (raft.InstallSnapshotResponse, error) {
	if message == nil {
		return raft.InstallSnapshotResponse{}, invalidMessage("response", "is nil")
	}
	return raft.InstallSnapshotResponse{Term: raft.Term(message.GetTerm()), NextOffset: message.GetNextOffset()}, nil
}
