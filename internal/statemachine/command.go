package statemachine

import (
	"encoding/binary"
	"errors"
	"fmt"

	"viper/pkg/contracts"
)

const (
	commandHeaderBytes = 24
	commandVersion     = 1
	expectedPresentBit = 1 << 0
)

var commandMagic = [4]byte{'V', 'K', 'C', '1'}

// CommandKind identifies one deterministic replicated mutation.
type CommandKind uint8

const (
	CommandUnspecified CommandKind = iota
	CommandPut
	CommandCompareAndSwap
)

// Command is the version-independent representation of a replicated mutation.
// ClientID and RequestID together provide retry-safe idempotency.
type Command struct {
	Kind            CommandKind
	ClientID        contracts.ClientID
	RequestID       contracts.RequestID
	Key             []byte
	Value           []byte
	ExpectedValue   []byte
	ExpectedPresent bool
}

// EncodeCommand validates and encodes a command in the stable Viper KV format.
func EncodeCommand(command Command) ([]byte, error) {
	if err := validateCommand(command); err != nil {
		return nil, err
	}
	client := []byte(command.ClientID)
	request := []byte(command.RequestID)
	total := commandHeaderBytes + len(client) + len(request) + len(command.Key) + len(command.Value) + len(command.ExpectedValue)
	if total > contracts.MaxCommandBytes {
		return nil, fmt.Errorf("%w: encoded command exceeds the size limit", ErrInvalidCommand)
	}
	encoded := make([]byte, total)
	copy(encoded[0:4], commandMagic[:])
	binary.BigEndian.PutUint16(encoded[4:6], commandVersion)
	encoded[6] = byte(command.Kind)
	if command.ExpectedPresent {
		encoded[7] = expectedPresentBit
	}
	binary.BigEndian.PutUint16(encoded[8:10], uint16(len(client)))
	binary.BigEndian.PutUint16(encoded[10:12], uint16(len(request)))
	binary.BigEndian.PutUint32(encoded[12:16], uint32(len(command.Key)))
	binary.BigEndian.PutUint32(encoded[16:20], uint32(len(command.Value)))
	binary.BigEndian.PutUint32(encoded[20:24], uint32(len(command.ExpectedValue)))
	offset := commandHeaderBytes
	for _, part := range [][]byte{client, request, command.Key, command.Value, command.ExpectedValue} {
		copy(encoded[offset:], part)
		offset += len(part)
	}
	return encoded, nil
}

// DecodeCommand strictly decodes one stable Viper KV command.
func DecodeCommand(encoded []byte) (Command, error) {
	if len(encoded) < commandHeaderBytes || len(encoded) > contracts.MaxCommandBytes {
		return Command{}, fmt.Errorf("%w: invalid encoded length", ErrInvalidCommand)
	}
	if string(encoded[0:4]) != string(commandMagic[:]) {
		return Command{}, fmt.Errorf("%w: unknown command magic", ErrInvalidCommand)
	}
	if binary.BigEndian.Uint16(encoded[4:6]) != commandVersion {
		return Command{}, fmt.Errorf("%w: unsupported command version", ErrInvalidCommand)
	}
	if encoded[7]&^byte(expectedPresentBit) != 0 {
		return Command{}, fmt.Errorf("%w: unsupported command flags", ErrInvalidCommand)
	}
	lengths := []uint64{
		uint64(binary.BigEndian.Uint16(encoded[8:10])),
		uint64(binary.BigEndian.Uint16(encoded[10:12])),
		uint64(binary.BigEndian.Uint32(encoded[12:16])),
		uint64(binary.BigEndian.Uint32(encoded[16:20])),
		uint64(binary.BigEndian.Uint32(encoded[20:24])),
	}
	total := uint64(commandHeaderBytes)
	for _, length := range lengths {
		total += length
	}
	if total != uint64(len(encoded)) {
		return Command{}, fmt.Errorf("%w: inconsistent field lengths", ErrInvalidCommand)
	}
	offset := commandHeaderBytes
	next := func(length uint64) []byte {
		value := append([]byte(nil), encoded[offset:offset+int(length)]...)
		offset += int(length)
		return value
	}
	client := next(lengths[0])
	request := next(lengths[1])
	command := Command{
		Kind:            CommandKind(encoded[6]),
		ClientID:        contracts.ClientID(client),
		RequestID:       contracts.RequestID(request),
		Key:             next(lengths[2]),
		Value:           next(lengths[3]),
		ExpectedValue:   next(lengths[4]),
		ExpectedPresent: encoded[7]&expectedPresentBit != 0,
	}
	if err := validateCommand(command); err != nil {
		return Command{}, err
	}
	return command, nil
}

func validateCommand(command Command) error {
	if command.Kind != CommandPut && command.Kind != CommandCompareAndSwap {
		return fmt.Errorf("%w: unsupported kind", ErrInvalidCommand)
	}
	if err := command.ClientID.Validate(); err != nil {
		return fmt.Errorf("%w: invalid client ID", ErrInvalidCommand)
	}
	if err := command.RequestID.Validate(); err != nil {
		return fmt.Errorf("%w: invalid request ID", ErrInvalidCommand)
	}
	if err := contracts.ValidateKey(command.Key); err != nil {
		return errors.Join(ErrInvalidCommand, err)
	}
	if err := contracts.ValidateValue(command.Value); err != nil {
		return errors.Join(ErrInvalidCommand, err)
	}
	if err := contracts.ValidateValue(command.ExpectedValue); err != nil {
		return errors.Join(ErrInvalidCommand, err)
	}
	if command.Kind == CommandPut && (command.ExpectedPresent || len(command.ExpectedValue) != 0) {
		return fmt.Errorf("%w: put cannot contain an expected value", ErrInvalidCommand)
	}
	if command.Kind == CommandCompareAndSwap && !command.ExpectedPresent && len(command.ExpectedValue) != 0 {
		return fmt.Errorf("%w: absent comparison cannot contain an expected value", ErrInvalidCommand)
	}
	return nil
}
