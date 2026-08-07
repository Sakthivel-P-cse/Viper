// Command viper-contractcheck verifies cross-schema size budgets from a
// compiled protobuf descriptor set.
package main

import (
	"fmt"
	"io"
	"os"
	"strings"

	validate "buf.build/gen/go/bufbuild/protovalidate/protocolbuffers/go/buf/validate"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"

	"viper/pkg/contracts"
)

const serializationHeadroom = 4 * 1024
const maxDescriptorBytes = 16 * 1024 * 1024
const grpcFrameBytes = 5

type limitKind uint8

const (
	stringLength limitKind = iota
	stringBytes
	bytesLength
	repeatedItems
)

type limitCheck struct {
	message protoreflect.FullName
	field   protoreflect.Name
	kind    limitKind
	want    uint64
}

func main() {
	if err := check(); err != nil {
		fmt.Fprintln(os.Stderr, "contract check:", err)
		os.Exit(1)
	}
}

func check() error {
	contents, readErr := io.ReadAll(io.LimitReader(os.Stdin, maxDescriptorBytes+1))
	if readErr != nil {
		return fmt.Errorf("read descriptor set from stdin: %w", readErr)
	}
	if len(contents) > maxDescriptorBytes {
		return fmt.Errorf("descriptor set exceeds %d bytes", maxDescriptorBytes)
	}
	set := &descriptorpb.FileDescriptorSet{}
	if decodeErr := proto.Unmarshal(contents, set); decodeErr != nil {
		return fmt.Errorf("decode descriptor set: %w", decodeErr)
	}
	files, descriptorErr := protodesc.NewFiles(set)
	if descriptorErr != nil {
		return fmt.Errorf("load descriptors: %w", descriptorErr)
	}

	if limitsErr := checkDeclaredLimits(files); limitsErr != nil {
		return limitsErr
	}

	cas, casErr := message(files, "viper.api.v1.CompareAndSwapRequest")
	if casErr != nil {
		return casErr
	}
	encodedSize, encodingErr := maximumCASSize(cas)
	if encodingErr != nil {
		return encodingErr
	}
	raftEntry, raftErr := message(files, "viper.api.v1.RaftLogEntry")
	if raftErr != nil {
		return raftErr
	}
	commandLimit := uint64(contracts.MaxCommandBytes)
	if encodedSize+serializationHeadroom > contracts.MaxCommandBytes {
		return fmt.Errorf(
			"maximum CAS encoding and headroom (%d) exceed Raft command limit (%d)",
			encodedSize+serializationHeadroom,
			commandLimit,
		)
	}
	if commandLimit >= uint64(contracts.MaxRPCMessageBytes) {
		return fmt.Errorf("raft command limit %d leaves no room below RPC limit %d", commandLimit, contracts.MaxRPCMessageBytes)
	}
	appendSize, appendErr := maximumAppendEntriesSize(files, raftEntry)
	if appendErr != nil {
		return appendErr
	}
	if appendSize+grpcFrameBytes > contracts.MaxRPCMessageBytes {
		return fmt.Errorf(
			"maximum one-entry AppendEntries frame (%d) exceeds RPC limit (%d)",
			appendSize+grpcFrameBytes,
			contracts.MaxRPCMessageBytes,
		)
	}
	return nil
}

func checkDeclaredLimits(files *protoregistry.Files) error {
	checks := []limitCheck{
		{message: "viper.api.v1.PutRequest", field: "client_id", kind: stringLength, want: contracts.MaxClientIDBytes},
		{message: "viper.api.v1.PutRequest", field: "request_id", kind: stringLength, want: contracts.MaxRequestIDBytes},
		{message: "viper.api.v1.PutRequest", field: "key", kind: bytesLength, want: contracts.MaxKeyBytes},
		{message: "viper.api.v1.PutRequest", field: "value", kind: bytesLength, want: contracts.MaxValueBytes},
		{message: "viper.api.v1.GetRequest", field: "key", kind: bytesLength, want: contracts.MaxKeyBytes},
		{message: "viper.api.v1.GetResponse", field: "value", kind: bytesLength, want: contracts.MaxValueBytes},
		{message: "viper.api.v1.CompareAndSwapRequest", field: "client_id", kind: stringLength, want: contracts.MaxClientIDBytes},
		{message: "viper.api.v1.CompareAndSwapRequest", field: "request_id", kind: stringLength, want: contracts.MaxRequestIDBytes},
		{message: "viper.api.v1.CompareAndSwapRequest", field: "key", kind: bytesLength, want: contracts.MaxKeyBytes},
		{message: "viper.api.v1.CompareAndSwapRequest", field: "expected_value", kind: bytesLength, want: contracts.MaxValueBytes},
		{message: "viper.api.v1.CompareAndSwapRequest", field: "new_value", kind: bytesLength, want: contracts.MaxValueBytes},
		{message: "viper.api.v1.CompareAndSwapResponse", field: "previous_value", kind: bytesLength, want: contracts.MaxValueBytes},
		{message: "viper.api.v1.ErrorDetail", field: "leader_hint", kind: stringLength, want: contracts.MaxNodeIDBytes},
		{message: "viper.api.v1.RequestVoteRequest", field: "candidate_id", kind: stringLength, want: contracts.MaxNodeIDBytes},
		{message: "viper.api.v1.RaftLogEntry", field: "command", kind: bytesLength, want: contracts.MaxCommandBytes},
		{message: "viper.api.v1.AppendEntriesRequest", field: "leader_id", kind: stringLength, want: contracts.MaxNodeIDBytes},
		{message: "viper.api.v1.AppendEntriesRequest", field: "entries", kind: repeatedItems, want: contracts.MaxLogEntriesPerAppend},
		{message: "viper.api.v1.InstallSnapshotRequest", field: "leader_id", kind: stringLength, want: contracts.MaxNodeIDBytes},
		{message: "viper.api.v1.InstallSnapshotRequest", field: "data", kind: bytesLength, want: contracts.MaxSnapshotChunkBytes},
		{message: "viper.api.v1.GetNodeStatusResponse", field: "node_id", kind: stringLength, want: contracts.MaxNodeIDBytes},
		{message: "viper.api.v1.GetNodeStatusResponse", field: "reason", kind: stringBytes, want: contracts.MaxStatusReasonBytes},
	}

	for _, check := range checks {
		messageDescriptor, messageErr := message(files, check.message)
		if messageErr != nil {
			return messageErr
		}
		actual, limitErr := declaredLimit(messageDescriptor, check.field, check.kind)
		if limitErr != nil {
			return limitErr
		}
		if actual != check.want {
			return fmt.Errorf(
				"%s.%s limit differs: schema=%d contract=%d",
				check.message,
				check.field,
				actual,
				check.want,
			)
		}
	}
	return nil
}

func declaredLimit(messageDescriptor protoreflect.MessageDescriptor, name protoreflect.Name, kind limitKind) (uint64, error) {
	rules, rulesErr := fieldRules(messageDescriptor, name)
	if rulesErr != nil {
		return 0, rulesErr
	}
	switch kind {
	case stringLength:
		if rules.GetString() != nil && rules.GetString().MaxLen != nil {
			return rules.GetString().GetMaxLen(), nil
		}
	case stringBytes:
		if rules.GetString() != nil && rules.GetString().MaxBytes != nil {
			return rules.GetString().GetMaxBytes(), nil
		}
	case bytesLength:
		if rules.GetBytes() != nil && rules.GetBytes().MaxLen != nil {
			return rules.GetBytes().GetMaxLen(), nil
		}
	case repeatedItems:
		if rules.GetRepeated() != nil && rules.GetRepeated().MaxItems != nil {
			return rules.GetRepeated().GetMaxItems(), nil
		}
	}
	return 0, fmt.Errorf("field %s.%s has no required limit kind %d", messageDescriptor.FullName(), name, kind)
}

func message(files *protoregistry.Files, name protoreflect.FullName) (protoreflect.MessageDescriptor, error) {
	descriptor, err := files.FindDescriptorByName(name)
	if err != nil {
		return nil, fmt.Errorf("find message %s: %w", name, err)
	}
	messageDescriptor, ok := descriptor.(protoreflect.MessageDescriptor)
	if !ok {
		return nil, fmt.Errorf("descriptor %s is not a message", name)
	}
	return messageDescriptor, nil
}

func fieldRules(messageDescriptor protoreflect.MessageDescriptor, name protoreflect.Name) (*validate.FieldRules, error) {
	field := messageDescriptor.Fields().ByName(name)
	if field == nil {
		return nil, fmt.Errorf("message %s has no field %s", messageDescriptor.FullName(), name)
	}
	options, ok := field.Options().(*descriptorpb.FieldOptions)
	if !ok || !proto.HasExtension(options, validate.E_Field) {
		return nil, fmt.Errorf("field %s.%s has no validation rules", messageDescriptor.FullName(), name)
	}
	rules, ok := proto.GetExtension(options, validate.E_Field).(*validate.FieldRules)
	if !ok {
		return nil, fmt.Errorf("field %s.%s has invalid validation rules", messageDescriptor.FullName(), name)
	}
	return rules, nil
}

func maximumCASSize(descriptor protoreflect.MessageDescriptor) (int, error) {
	message := dynamicpb.NewMessage(descriptor)
	message.Set(descriptor.Fields().ByName("client_id"), protoreflect.ValueOfString(strings.Repeat("a", contracts.MaxClientIDBytes)))
	message.Set(descriptor.Fields().ByName("request_id"), protoreflect.ValueOfString(strings.Repeat("a", contracts.MaxRequestIDBytes)))
	message.Set(descriptor.Fields().ByName("key"), protoreflect.ValueOfBytes(make([]byte, contracts.MaxKeyBytes)))
	message.Set(descriptor.Fields().ByName("expected_value"), protoreflect.ValueOfBytes(make([]byte, contracts.MaxValueBytes)))
	message.Set(descriptor.Fields().ByName("expected_present"), protoreflect.ValueOfBool(true))
	message.Set(descriptor.Fields().ByName("new_value"), protoreflect.ValueOfBytes(make([]byte, contracts.MaxValueBytes)))
	encoded, err := proto.MarshalOptions{Deterministic: true}.Marshal(message)
	if err != nil {
		return 0, fmt.Errorf("marshal maximum CAS: %w", err)
	}
	return len(encoded), nil
}

func maximumAppendEntriesSize(
	files *protoregistry.Files,
	entryDescriptor protoreflect.MessageDescriptor,
) (int, error) {
	requestDescriptor, requestErr := message(files, "viper.api.v1.AppendEntriesRequest")
	if requestErr != nil {
		return 0, requestErr
	}
	entry := dynamicpb.NewMessage(entryDescriptor)
	entry.Set(entryDescriptor.Fields().ByName("term"), protoreflect.ValueOfUint64(^uint64(0)))
	entry.Set(entryDescriptor.Fields().ByName("index"), protoreflect.ValueOfUint64(^uint64(0)))
	entry.Set(entryDescriptor.Fields().ByName("command"), protoreflect.ValueOfBytes(make([]byte, contracts.MaxCommandBytes)))
	entry.Set(entryDescriptor.Fields().ByName("checksum"), protoreflect.ValueOfUint32(^uint32(0)))

	request := dynamicpb.NewMessage(requestDescriptor)
	request.Set(requestDescriptor.Fields().ByName("term"), protoreflect.ValueOfUint64(^uint64(0)))
	request.Set(
		requestDescriptor.Fields().ByName("leader_id"),
		protoreflect.ValueOfString(strings.Repeat("a", contracts.MaxNodeIDBytes)),
	)
	request.Set(requestDescriptor.Fields().ByName("prev_log_index"), protoreflect.ValueOfUint64(^uint64(0)))
	request.Set(requestDescriptor.Fields().ByName("prev_log_term"), protoreflect.ValueOfUint64(^uint64(0)))
	request.Mutable(requestDescriptor.Fields().ByName("entries")).List().Append(protoreflect.ValueOfMessage(entry))
	request.Set(requestDescriptor.Fields().ByName("leader_commit"), protoreflect.ValueOfUint64(^uint64(0)))
	encoded, marshalErr := proto.MarshalOptions{Deterministic: true}.Marshal(request)
	if marshalErr != nil {
		return 0, fmt.Errorf("marshal maximum AppendEntries: %w", marshalErr)
	}
	return len(encoded), nil
}
