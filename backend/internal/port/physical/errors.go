package physical

import "errors"

// ErrClientRetired reports that a cached egress client handle was retired.
// It is a local admission fact, not an upstream node failure.
var ErrClientRetired = errors.New("egress client binding retired")
