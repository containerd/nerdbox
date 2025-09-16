//go:build !linux

package kvm

// CheckKVM is a no-op on non-Linux platforms.
func CheckKVM() error {
	return nil
}
