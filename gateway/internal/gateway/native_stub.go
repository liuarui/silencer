//go:build !silencer_engine || !cgo

package gateway

import "fmt"

// The default binary intentionally has no native dependency. Build with
// -tags silencer_engine on a host that has libsilencer_engine installed.
func newNativeEngine(Config) (Engine, error) {
	return nil, fmt.Errorf("native C++ engine is unavailable in this build (use -tags silencer_engine)")
}
