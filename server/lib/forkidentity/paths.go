package forkidentity

const (
	WaitEnv = "KERNEL_FORK_IDENTITY_WAIT"

	DefaultReadyFile   = "/run/kernel/fork-identity-ready"
	DefaultPayloadFile = "/run/kernel/fork-identity.json"
	DefaultAppliedFile = "/run/kernel/fork-identity-applied"
)

var (
	ReadyFile   = DefaultReadyFile
	PayloadFile = DefaultPayloadFile
	AppliedFile = DefaultAppliedFile
)
