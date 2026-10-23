package jumphost

// AiperfSSHExecFn exposes the aiperf SSH-exec seam for unit tests.
// Tests replace it to assert command construction without network.
var AiperfSSHExecFn = &aiperfSSHExecFn

// BuildAiperfCmd exposes buildAiperfCmd for command-construction tests.
var BuildAiperfCmd = buildAiperfCmd

// BuildMooncakeCmd exposes buildMooncakeCmd for unit tests.
var BuildMooncakeCmd = buildMooncakeCmd

// ValidateTraceURL exposes validateTraceURL for unit tests.
var ValidateTraceURL = validateTraceURL

// DilateTimestamp exposes dilateTimestamp for unit tests.
var DilateTimestamp = dilateTimestamp

// BuildModelsQueryCmd exposes buildModelsQueryCmd for unit tests.
var BuildModelsQueryCmd = buildModelsQueryCmd
