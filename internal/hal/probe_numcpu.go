//go:build linux || android || darwin

package hal

import "runtime"

func numCPU() int {
	return runtime.NumCPU()
}
