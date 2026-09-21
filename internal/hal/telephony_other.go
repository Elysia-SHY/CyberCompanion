//go:build !linux && !android

package hal

// ProbeCellular 在非 Linux/Android 平台无蜂窝模组接口，返回 nil。
func ProbeCellular() *CellularInfo {
	return nil
}
