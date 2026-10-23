package jumphost

// BuildCurlBodyCmd exposes buildCurlBodyCmd for unit tests in the external _test package.
var BuildCurlBodyCmd = buildCurlBodyCmd

// BuildHTTPResponderCmd exposes buildHTTPResponderCmd for unit tests.
var BuildHTTPResponderCmd = buildHTTPResponderCmd

// BuildHTTPResponderStopCmd exposes buildHTTPResponderStopCmd for unit tests.
var BuildHTTPResponderStopCmd = buildHTTPResponderStopCmd

// BuildGrpcurlInstallCmd is an alias for GrpcurlInstallCmd for unit tests.
// Uses the same var pattern as the other test exports for consistency.
var BuildGrpcurlInstallCmd = GrpcurlInstallCmd

// BuildSSHArgs exposes buildSSHArgs for unit tests.
var BuildSSHArgs = buildSSHArgs

// PrepareProbeKey exposes prepareProbeKey for unit tests.
var PrepareProbeKey = prepareProbeKey

// Seam overrides — tests swap these to avoid real ssh/aws subprocess calls.

// PrepareEICEKeyFn is the overridable seam for prepareEICEKey consumed by
// RunStagingCommands and CopyFileViaEICE. Tests replace it to assert
// mint-once behaviour without network.
var PrepareEICEKeyFn = &prepareEICEKeyFn

// SSHRunViaEICEFn is the overridable seam for SSHRunViaEICE consumed by
// RunStagingCommands and CopyFileViaEICE. Tests replace it to capture
// commands without network.
var SSHRunViaEICEFn = &sshRunViaEICEFn

// SSHRunViaEICEStdinFn is the overridable seam for SSHRunViaEICEWithStdin
// consumed by RunStagingCommandWithStdin and CopyFileViaEICEStdin. Tests
// replace it to capture commands + stdin payloads without network.
var SSHRunViaEICEStdinFn = &sshRunViaEICEStdinFn

// PushSSHPublicKeyFn is the overridable seam for PushSSHPublicKey consumed by
// RunStagingCommands and CopyFileViaEICE. Tests replace it to assert
// re-push-per-step behaviour without network.
var PushSSHPublicKeyFn = &pushSSHPublicKeyFn
