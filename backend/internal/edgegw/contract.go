// Package edgegw implements the control-plane / data-plane split for the
// distributed gateway (see docs/tech/distributed-edge.md and
// docs/tech/ccdirect-auth-contract.md).
//
// The shared wire types live in the sibling contract package (imported by both
// ccdirect and cchub). These aliases keep existing edgegw.* references working
// during the ccdirect/cchub split; call sites migrate to contract.* in later
// steps.
package edgegw

import "github.com/Wei-Shaw/sub2api/internal/edgegw/contract"

type (
	LeaseRequest  = contract.LeaseRequest
	Candidate     = contract.Candidate
	LeaseResult   = contract.LeaseResult
	SettleRequest = contract.SettleRequest
	SettleResult  = contract.SettleResult
	AuthScheme    = contract.AuthScheme

	RegisterRequest  = contract.RegisterRequest
	HeartbeatRequest = contract.HeartbeatRequest
	RegisterResponse = contract.RegisterResponse
	EnrollRequest    = contract.EnrollRequest
	EnrollResponse   = contract.EnrollResponse
)

var (
	ErrWaitQueueFull     = contract.ErrWaitQueueFull
	ErrConcurrencyFull   = contract.ErrConcurrencyFull
	ErrBillingIneligible = contract.ErrBillingIneligible
	ErrNoAccount         = contract.ErrNoAccount
	ErrInvalidRequest    = contract.ErrInvalidRequest
)
