//go:build !go1.28 && goexperiment.simd && amd64 && amd64.v3

package kernels

import "math/bits"

func stage1BatchAvailable() bool { return true }

func stage1SamplePopcount(x uint64) int { return bits.OnesCount64(x) }
