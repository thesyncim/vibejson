package simd

// Stage2Walk provides the legacy bitmap API through the Go-native machine.
func Stage2Walk(base *byte, emit []uint64, kinds *[Stage2KindsLen]byte, scalars []uint32, st *Stage2State) int {
	return Stage2WalkGo(base, emit, kinds, scalars, st)
}
