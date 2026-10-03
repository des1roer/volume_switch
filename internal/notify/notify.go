// Package notify shows a native Windows balloon/toast notification anchored
// to this process's own tray icon (the one created by
// github.com/getlantern/systray), so switching the audio device gives
// visible feedback even when the tray tooltip alone would go unnoticed.
//
// There is no public systray API to show a balloon, so this locates the
// hidden tray window systray created (window class "SystrayClass", icon ID
// 100 - both hardcoded in systray v1.2.2) and calls Shell_NotifyIcon on it
// directly. If a future systray upgrade changes either value, Show simply
// becomes a silent no-op.
package notify

import (
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	nimModify = 0x00000001

	nifInfo = 0x00000010

	niifInfo             = 0x00000001
	niifRespectQuietTime = 0x00000080

	systrayClassName = "SystrayClass"
	trayIconID       = 100
)

type notifyIconDataW struct {
	cbSize            uint32
	hWnd              windows.Handle
	uID               uint32
	uFlags            uint32
	uCallbackMessage  uint32
	hIcon             windows.Handle
	szTip             [128]uint16
	dwState           uint32
	dwStateMask       uint32
	szInfo            [256]uint16
	uTimeoutOrVersion uint32
	szInfoTitle       [64]uint16
	dwInfoFlags       uint32
	guidItem          windows.GUID
	hBalloonIcon      windows.Handle
}

var (
	user32  = windows.NewLazySystemDLL("user32.dll")
	shell32 = windows.NewLazySystemDLL("shell32.dll")

	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetClassNameW            = user32.NewProc("GetClassNameW")
	procGetWindowThreadProcessId = user32.NewProc("GetWindowThreadProcessId")
	procShellNotifyIconW         = shell32.NewProc("Shell_NotifyIconW")
)

// Show displays a balloon notification with the given title and body. It is
// a no-op if this process's tray window cannot be found (e.g. called before
// systray finished initializing, or after a dependency upgrade changed the
// implementation details above).
func Show(title, body string) {
	hwnd := findOwnTrayWindow()
	if hwnd == 0 {
		return
	}

	var nid notifyIconDataW
	nid.cbSize = uint32(unsafe.Sizeof(nid))
	nid.hWnd = hwnd
	nid.uID = trayIconID
	nid.uFlags = nifInfo
	nid.dwInfoFlags = niifInfo | niifRespectQuietTime
	copyUTF16(nid.szInfo[:], body)
	copyUTF16(nid.szInfoTitle[:], title)

	procShellNotifyIconW.Call(uintptr(nimModify), uintptr(unsafe.Pointer(&nid)))
}

func copyUTF16(dst []uint16, s string) {
	src, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	n := len(src)
	if n > len(dst) {
		n = len(dst)
	}
	copy(dst, src[:n])
	if n == len(dst) {
		dst[n-1] = 0
	}
}

// findOwnTrayWindow enumerates top-level windows looking for the hidden
// systray window that belongs to this process.
func findOwnTrayWindow() windows.Handle {
	pid := windows.GetCurrentProcessId()
	var found windows.Handle

	cb := windows.NewCallback(func(hwnd windows.Handle, _ uintptr) uintptr {
		var winPid uint32
		procGetWindowThreadProcessId.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&winPid)))
		if winPid != pid {
			return 1 // continue enumeration
		}

		var cls [64]uint16
		n, _, _ := procGetClassNameW.Call(uintptr(hwnd), uintptr(unsafe.Pointer(&cls[0])), uintptr(len(cls)))
		if n == 0 {
			return 1
		}
		if windows.UTF16ToString(cls[:n]) == systrayClassName {
			found = hwnd
			return 0 // stop enumeration
		}
		return 1
	})

	procEnumWindows.Call(cb, 0)
	return found
}
