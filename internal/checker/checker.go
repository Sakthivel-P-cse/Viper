package checker

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"sort"
)

var (
	ErrInvalidHistory  = errors.New("invalid operation history")
	ErrHistoryTooLarge = errors.New("operation history exceeds checker bounds")
)

// Result reports a bounded linearizability search outcome.
type Result struct {
	Linearizable   bool
	ExploredStates uint64
	Counterexample []string
	Reason         string
}

type valueState struct {
	value    []byte
	found    bool
	revision uint64
}

type mutationResult struct {
	signature [sha256.Size]byte
	result    Operation
}

type model struct {
	values   map[string]valueState
	requests map[string]mutationResult
	revision uint64
}

// Check verifies a bounded history against the sequential KV reference model.
// Real-time precedence is enforced; overlapping operations may be explored in
// any legal linearization order until the deterministic search bound is hit.
func Check(history History) Result {
	if err := Validate(history); err != nil {
		return Result{Reason: err.Error()}
	}
	predecessors := buildPredecessors(history.Operations)
	search := checkerSearch{operations: history.Operations, predecessors: predecessors, limit: 1_000_000}
	search.model = model{values: make(map[string]valueState), requests: make(map[string]mutationResult)}
	search.history = []model{cloneModel(search.model)}
	search.historyMasks = []uint64{0}
	placed := make([]bool, len(history.Operations))
	if search.walk(placed, 0) {
		return Result{Linearizable: true, ExploredStates: search.explored}
	}
	result := Result{Linearizable: false, ExploredStates: search.explored, Reason: "no legal linearization found"}
	result.Counterexample = append([]string(nil), search.bestPath...)
	if search.bounded {
		result.Reason = "checker search bound exceeded"
	}
	return result
}

// CheckJSON decodes and checks one bounded JSON history.
func CheckJSON(data []byte) Result {
	history, err := Decode(data)
	if err != nil {
		return Result{Reason: err.Error()}
	}
	return Check(history)
}

type checkerSearch struct {
	operations   []Operation
	predecessors []uint64
	model        model
	path         []string
	pathIndices  []int
	bestPath     []string
	history      []model
	historyMasks []uint64
	explored     uint64
	limit        uint64
	bounded      bool
}

func (s *checkerSearch) walk(placed []bool, depth int) bool {
	if depth == len(s.operations) {
		return true
	}
	if len(s.path) > len(s.bestPath) {
		s.bestPath = append([]string(nil), s.path...)
	}
	if s.explored >= s.limit {
		s.bounded = true
		return false
	}
	for index, operation := range s.operations {
		if placed[index] || !predecessorsPlaced(s.predecessors[index], placed) {
			continue
		}
		s.explored++
		previousModel := cloneModel(s.model)
		previousPathLength := len(s.path)
		previousHistoryLength := len(s.history)
		if s.apply(operation) {
			placed[index] = true
			s.path = append(s.path, operation.ID)
			s.history = append(s.history, cloneModel(s.model))
			s.historyMasks = append(s.historyMasks, placedMask(placed))
			if s.walk(placed, depth+1) {
				return true
			}
			placed[index] = false
		}
		s.model = previousModel
		s.path = s.path[:previousPathLength]
		s.history = s.history[:previousHistoryLength]
		s.historyMasks = s.historyMasks[:previousHistoryLength]
	}
	return false
}

func (s *checkerSearch) apply(operation Operation) bool {
	if !operation.Succeeded {
		return true
	}
	switch operation.Kind {
	case OperationGet:
		return s.applyGet(operation)
	case OperationPut, OperationCAS:
		return s.applyMutation(operation)
	default:
		return false
	}
}

func (s *checkerSearch) applyGet(operation Operation) bool {
	if operation.Consistency == ConsistencyStale {
		// A stale read may observe any prior state that already includes all
		// operations required to precede it in real time. It is not limited to
		// the latest state in the current linearization prefix.
		for index, candidate := range s.history {
			if s.predecessorsFor(operation)&^s.historyMasks[index] != 0 {
				continue
			}
			if equalValue(candidate.values[string(operation.Key)], operation.ResultValue, operation.ResultFound, operation.ResultRevision) {
				return true
			}
		}
		return false
	}
	return equalValue(s.model.values[string(operation.Key)], operation.ResultValue, operation.ResultFound, operation.ResultRevision)
}

func (s *checkerSearch) predecessorsFor(operation Operation) uint64 {
	for index, candidate := range s.operations {
		if candidate.ID == operation.ID {
			return s.predecessors[index]
		}
	}
	return 0
}

func (s *checkerSearch) applyMutation(operation Operation) bool {
	requestKey := operation.ClientID + "\x00" + operation.RequestID
	signature := mutationSignature(operation)
	if previous, exists := s.model.requests[requestKey]; exists {
		return previous.signature == signature && mutationOutputEqual(previous.result, operation)
	}
	current := s.model.values[string(operation.Key)]
	result := operation
	result.ResultPreviousValue = append([]byte(nil), current.value...)
	result.ResultPreviousPresent = current.found
	switch operation.Kind {
	case OperationPut:
		s.model.revision++
		s.model.values[string(operation.Key)] = valueState{value: append([]byte(nil), operation.NewValue...), found: true, revision: s.model.revision}
		result.ResultRevision = s.model.revision
	case OperationCAS:
		matches := current.found == operation.ExpectedPresent
		if matches && current.found {
			matches = bytes.Equal(current.value, operation.ExpectedValue)
		}
		if matches {
			s.model.revision++
			s.model.values[string(operation.Key)] = valueState{value: append([]byte(nil), operation.NewValue...), found: true, revision: s.model.revision}
			result.ResultSwapped = true
			result.ResultRevision = s.model.revision
		} else {
			result.ResultSwapped = false
			result.ResultRevision = s.model.revision
		}
	}
	if !mutationOutputEqual(result, operation) {
		return false
	}
	s.model.requests[requestKey] = mutationResult{signature: signature, result: result}
	return true
}

func equalValue(current valueState, value []byte, found bool, revision uint64) bool {
	return current.found == found && current.revision == revision && (!found || bytes.Equal(current.value, value))
}

func mutationOutputEqual(expected, actual Operation) bool {
	if expected.ResultRevision != actual.ResultRevision || expected.ResultSwapped != actual.ResultSwapped {
		return false
	}
	if expected.Kind != OperationCAS {
		return true
	}
	return expected.ResultPreviousPresent == actual.ResultPreviousPresent && bytes.Equal(expected.ResultPreviousValue, actual.ResultPreviousValue)
}

func mutationSignature(operation Operation) [sha256.Size]byte {
	hasher := sha256.New()
	writeSignatureString(hasher, string(operation.Kind))
	writeSignatureString(hasher, operation.ClientID)
	writeSignatureString(hasher, operation.RequestID)
	writeSignatureBytes(hasher, operation.Key)
	if operation.ExpectedPresent {
		_, _ = hasher.Write([]byte{1})
	} else {
		_, _ = hasher.Write([]byte{0})
	}
	writeSignatureBytes(hasher, operation.ExpectedValue)
	writeSignatureBytes(hasher, operation.NewValue)
	var signature [sha256.Size]byte
	copy(signature[:], hasher.Sum(nil))
	return signature
}

func writeSignatureString(hasher interface{ Write([]byte) (int, error) }, value string) {
	writeSignatureBytes(hasher, []byte(value))
}

func writeSignatureBytes(hasher interface{ Write([]byte) (int, error) }, value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = hasher.Write(length[:])
	_, _ = hasher.Write(value)
}

func cloneModel(source model) model {
	clone := model{values: make(map[string]valueState, len(source.values)), requests: make(map[string]mutationResult, len(source.requests)), revision: source.revision}
	for key, value := range source.values {
		clone.values[key] = valueState{value: append([]byte(nil), value.value...), found: value.found, revision: value.revision}
	}
	for key, value := range source.requests {
		result := value.result
		result.ResultPreviousValue = append([]byte(nil), result.ResultPreviousValue...)
		clone.requests[key] = mutationResult{signature: value.signature, result: result}
	}
	return clone
}

func buildPredecessors(operations []Operation) []uint64 {
	predecessors := make([]uint64, len(operations))
	for current, operation := range operations {
		for previous, candidate := range operations {
			if previous == current {
				continue
			}
			if candidate.Completion < operation.Invocation || candidate.Completion == operation.Invocation && previous < current {
				predecessors[current] |= uint64(1) << previous
			}
		}
	}
	return predecessors
}

func predecessorsPlaced(mask uint64, placed []bool) bool {
	for index, isPlaced := range placed {
		if mask&(uint64(1)<<index) != 0 && !isPlaced {
			return false
		}
	}
	return true
}

func placedMask(placed []bool) uint64 {
	var mask uint64
	for index, isPlaced := range placed {
		if isPlaced {
			mask |= uint64(1) << index
		}
	}
	return mask
}

// SortOperationsByInvocation provides stable presentation order for CLI tools.
func SortOperationsByInvocation(operations []Operation) []Operation {
	copyOfOperations := append([]Operation(nil), operations...)
	sort.SliceStable(copyOfOperations, func(i, j int) bool {
		if copyOfOperations[i].Invocation == copyOfOperations[j].Invocation {
			return copyOfOperations[i].ID < copyOfOperations[j].ID
		}
		return copyOfOperations[i].Invocation < copyOfOperations[j].Invocation
	})
	return copyOfOperations
}
