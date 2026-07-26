package tunnel

import "errors"

var (
	ErrSessionClosed = errors.New("tunnel session closed")
	ErrOpenTimeout   = errors.New("tunnel stream open timeout")
	ErrCapacity      = errors.New("tunnel stream capacity reached")
)
