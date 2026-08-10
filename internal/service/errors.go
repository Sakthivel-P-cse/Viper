package service

import "errors"

// ErrInvalidService indicates that service dependencies are missing.
var ErrInvalidService = errors.New("invalid service configuration")
