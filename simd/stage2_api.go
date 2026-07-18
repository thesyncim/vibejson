package simd

// Stage2Enabled reports whether the stage-2 grammar backend is available.
//
// Deprecated: The Go-native backend is available on every supported build;
// this function always returns true.
func Stage2Enabled() bool { return true }

// Stage2NativeEnabled reports whether Stage2Walk uses a native bitmap machine.
//
// Deprecated: The native bitmap machine was removed; this function always
// returns false.
func Stage2NativeEnabled() bool { return false }

// Stage2Walk provides the legacy bitmap API through the Go-native machine.
func Stage2Walk(base *byte, emit []uint64, kinds *[Stage2KindsLen]byte, scalars []uint32, st *Stage2State) int {
	return Stage2WalkGo(base, emit, kinds, scalars, st)
}
