// SPDX-License-Identifier: Apache-2.0

//go:build !unix

package local

// On non-unix platforms these conditions are detected only through the portable
// os error sentinels handled by the callers, so the syscall-specific probes are
// conservatively false.

func isNotEmpty(error) bool     { return false }
func isIsDir(error) bool        { return false }
func isNotSupported(error) bool { return false }
