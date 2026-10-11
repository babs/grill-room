package main

import "golang.org/x/sys/windows"

// alive reports whether pid still runs.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid)) //nolint:gosec // a pid fits
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h) //nolint:errcheck // a query handle
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == 259 // STILL_ACTIVE
}
