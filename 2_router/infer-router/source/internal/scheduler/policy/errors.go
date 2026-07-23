package policy

import "errors"

// Sentinel errors returned by Select and BatchSelect methods.
// Package-level variables avoid per-call heap allocations — critical in
// BatchSelect loops where up to 8192 identical errors could be allocated per batch.
var (
	ErrNoInstances       = errors.New("no available instances registered")
	ErrNoInstancesChosen = errors.New("no available instances chosed")
	ErrAllOverloaded     = errors.New("all instances overloaded (load >= MaxRequestLoad), reduce traffic or increase MaxRequestLoad")
	ErrAllExceedSession  = errors.New("all instances exceed session load threshold (sessions >= MaxSessionLoad)")
)
