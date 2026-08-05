//go:build !race

package localembed_test

// raceEnabled is false in an ordinary build; see race_test.go for why it exists.
const raceEnabled = false
