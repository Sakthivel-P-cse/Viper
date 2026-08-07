package storage

import "os"

type failurePoint uint8

const (
	failureNone failurePoint = iota
	failureWALWrite
	failureWALSync
	failureSnapshotWrite
	failureSnapshotSync
	failureSnapshotRename
	failureSnapshotDirectorySync
	failureWALCompactWrite
	failureWALCompactSync
	failureWALRename
	failureWALDirectorySync
	failureWALReopen
)

type failureInjector func(failurePoint) error

type walWriteFunc func(*os.File, []byte) error

func injectFailure(inject failureInjector, point failurePoint) error {
	if inject == nil || point == failureNone {
		return nil
	}
	return inject(point)
}
