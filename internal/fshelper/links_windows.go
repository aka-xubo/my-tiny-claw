//go:build windows

package fshelper

import "os"

// Restricted profiles are unsupported on Windows. Fail closed for an in-place
// fallback whose hardlink count cannot be established here.
func hasMultipleLinks(os.FileInfo) bool { return true }
