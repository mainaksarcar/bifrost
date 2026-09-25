//go:build copilot_inprocess

package githubcopilot_test

// The SDK keeps its own availability flag unexported, so the live suite needs its own
// build-tag probe to tell "not built for in-process dispatch" apart from a real failure.
const inProcessBuild = true
