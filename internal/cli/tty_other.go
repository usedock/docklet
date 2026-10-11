//go:build !windows

package cli

func enableVT(int) func() { return func() {} }
