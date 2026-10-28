//go:build race

package run

// raceEnabled skips tests that run act's host environment, which races
// between its exec and pty writer (nektos/act v0.2.89, not selfaction code).
const raceEnabled = true
