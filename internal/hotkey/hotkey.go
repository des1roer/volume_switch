// Package hotkey installs a global, process-wide low-level keyboard hook
// (WH_KEYBOARD_LL) to catch F7/F8 even when the application has no focused
// window, and swallows the key so it does not reach other applications -
// the same approach Volume2 uses.
package hotkey

import (
	"errors"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	whKeyboardLL = 13

	wmKeyDown    = 0x0100
	wmSysKeyDown = 0x0104

	vkF7 = 0x76
	vkF8 = 0x77
)

type kbdllhookstruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

type msg struct {
	Hwnd    uintptr
	Message uint32
	WParam  uintptr
	LParam  uintptr
	Time    uint32
	Pt      struct{ X, Y int32 }
}

var (
	user32 = windows.NewLazySystemDLL("user32.dll")

	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")
)

const wmQuit = 0x0012

// Listener installs the hook and dispatches F7/F8 presses to the provided
// callbacks. Callbacks run on the hook's dedicated OS thread, so they
// should return quickly (hand off work via a channel/goroutine if needed).
type Listener struct {
	OnF7 func()
	OnF8 func()

	threadID uint32
	hook     uintptr
	started  chan error
	stopped  chan struct{}
}

// Start installs the keyboard hook on a new, dedicated, locked OS thread
// and blocks until the hook is installed (or installation failed).
func (l *Listener) Start() error {
	l.started = make(chan error, 1)
	l.stopped = make(chan struct{})

	go l.loop()

	return <-l.started
}

// Stop removes the hook and terminates the hook's message loop.
func (l *Listener) Stop() {
	if l.threadID == 0 {
		return
	}
	procPostThreadMessageW.Call(uintptr(l.threadID), wmQuit, 0, 0)
	<-l.stopped
}

func (l *Listener) loop() {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	defer close(l.stopped)

	l.threadID = windows.GetCurrentThreadId()

	callback := windows.NewCallback(l.hookProc)
	hook, _, callErr := procSetWindowsHookExW.Call(
		uintptr(whKeyboardLL),
		callback,
		0,
		0,
	)
	if hook == 0 {
		l.started <- errors.New("SetWindowsHookExW failed: " + callErr.Error())
		return
	}
	l.hook = hook
	l.started <- nil
	defer procUnhookWindowsHookEx.Call(l.hook)

	var m msg
	for {
		ret, _, _ := procGetMessageW.Call(uintptr(unsafe.Pointer(&m)), 0, 0, 0)
		// ret == 0 means WM_QUIT, ^uintptr(0) (-1) means an error.
		if ret == 0 || ret == ^uintptr(0) {
			return
		}
	}
}

func (l *Listener) hookProc(nCode, wParam, lParam uintptr) uintptr {
	if int32(nCode) >= 0 && (wParam == wmKeyDown || wParam == wmSysKeyDown) {
		kb := (*kbdllhookstruct)(unsafe.Pointer(lParam))
		switch kb.VkCode {
		case vkF7:
			if l.OnF7 != nil {
				go l.OnF7()
			}
			return 1
		case vkF8:
			if l.OnF8 != nil {
				go l.OnF8()
			}
			return 1
		}
	}
	ret, _, _ := procCallNextHookEx.Call(0, nCode, wParam, lParam)
	return ret
}
