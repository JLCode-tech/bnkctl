package forge

// BenchmarkHTTPDoFn exposes the benchmark HTTP transport seam for unit tests.
// Tests replace it to assert POST payload shape + path without a live forge server.
var BenchmarkHTTPDoFn = &benchmarkHTTPDoFn
