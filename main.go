// Command volume_switch sits in the system tray and lets F5 toggle the
// mpcprogress indicator while F7/F8 cycle the default Windows playback
// device, the same way Volume2 does.
package main

import (
	"errors"
	"log"
	"os/exec"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/getlantern/systray"

	"volume_switch/internal/audio"
	"volume_switch/internal/notify"
	"volume_switch/internal/trayicon"
)

// mpcProgressPath — путь к внешнему плееру-индикатору, который
// запускается/закрывается по F5.
const mpcProgressPath = `C:\dev\mpcprogress\mpcprogress.exe`

// mpcProgressTitle — заголовок окна mpcprogress (см. a.NewWindow в его коде).
// Используется для вежливого закрытия через WM_CLOSE.
const mpcProgressTitle = "MPC-HC Progress"

func main() {
	systray.Run(onReady, onExit)
}

var (
	switcher *audio.Switcher
	hk       *hotkeyListener

	menuMu     sync.Mutex
	menuItems  []*systray.MenuItem
	deviceByID map[string]int

	// Состояние mpcprogress: cmd != nil, пока процесс запущен.
	mpcMu  sync.Mutex
	mpcCmd *exec.Cmd
)

func onReady() {
	systray.SetIcon(trayicon.Data)
	systray.SetTitle("")
	systray.SetTooltip("Volume Switch")

	var err error
	switcher, err = audio.NewSwitcher()
	if err != nil {
		log.Printf("audio init failed: %v", err)
		systray.SetTooltip("Volume Switch: audio init failed")
		return
	}

	devices, err := switcher.ListActiveDevices()
	if err != nil {
		log.Printf("list devices failed: %v", err)
	}
	currentID, err := switcher.DefaultDeviceID()
	if err != nil {
		log.Printf("get default device failed: %v", err)
	}

	buildDeviceMenu(devices, currentID)

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Выход", "Закрыть Volume Switch")
	go func() {
		<-mQuit.ClickedCh
		systray.Quit()
	}()

	hk = &hotkeyListener{
		OnF5: func() { toggleMPCProgress() },
		OnF7: func() { onCycle(-1) },
		OnF8: func() { onCycle(1) },
	}
	if err := hk.Start(); err != nil {
		log.Printf("hotkey hook failed: %v", err)
	}
}

func onExit() {
	if hk != nil {
		hk.Stop()
	}
	// На выходе гасим mpcprogress, если он жив.
	mpcMu.Lock()
	if mpcCmd != nil && mpcCmd.Process != nil {
		_ = mpcCmd.Process.Kill()
	}
	mpcCmd = nil
	mpcMu.Unlock()

	if switcher != nil {
		switcher.Close()
	}
}

// toggleMPCProgress запускает mpcprogress, если он не запущен,
// и закрывает его, если запущен.
func toggleMPCProgress() {
	mpcMu.Lock()
	defer mpcMu.Unlock()

	if mpcCmd != nil && mpcCmd.Process != nil {
		closeMPCProgressLocked()
		return
	}

	cmd := exec.Command(mpcProgressPath)
	if err := cmd.Start(); err != nil {
		log.Printf("launch mpcprogress failed: %v", err)
		return
	}
	mpcCmd = cmd

	// Ждём завершения в фоне и сбрасываем состояние.
	go func(c *exec.Cmd) {
		err := c.Wait()
		if err != nil {
			log.Printf("mpcprogress exited: %v", err)
		}
		mpcMu.Lock()
		if mpcCmd == c {
			mpcCmd = nil
		}
		mpcMu.Unlock()
	}(cmd)
}

// closeMPCProgressLocked вызывается под удерживаемым mpcMu.
// Сначала посылаем окну WM_CLOSE (вежливо), и если через секунду процесс
// всё ещё жив — убиваем его жёстко.
func closeMPCProgressLocked() {
	cmd := mpcCmd
	proc := cmd.Process

	// Вежливая попытка: FindWindow по заголовку + PostMessage(WM_CLOSE).
	posted := postCloseMessage(mpcProgressTitle)

	if posted {
		// Дадим приложению секунду на корректное завершение.
		done := make(chan struct{}, 1)
		go func() {
			_, _ = proc.Wait()
			done <- struct{}{}
		}()
		select {
		case <-done:
			// закрылось само
		case <-time.After(time.Second):
			_ = proc.Kill()
		}
	} else {
		// Окно не нашли — убиваем сразу.
		if err := proc.Kill(); err != nil {
			log.Printf("kill mpcprogress failed: %v", err)
		}
	}

	mpcCmd = nil
}

// ---------------------------------------------------------------------------
// Win32-хелперы для вежливого закрытия окна по заголовку.
// ---------------------------------------------------------------------------

var (
	user32Win = syscall.NewLazyDLL("user32.dll")

	pFindWindowW  = user32Win.NewProc("FindWindowW")
	pPostMessageW = user32Win.NewProc("PostMessageW")
)

const wmClose = 0x0010

// postCloseMessage находит окно по заголовку и посылает ему WM_CLOSE.
// Возвращает true, если окно найдено и сообщение поставлено в очередь.
func postCloseMessage(title string) bool {
	t, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return false
	}
	hwnd, _, _ := pFindWindowW.Call(0, uintptr(unsafe.Pointer(t)))
	if hwnd == 0 {
		return false
	}
	pPostMessageW.Call(hwnd, uintptr(wmClose), 0, 0)
	return true
}

// ---------------------------------------------------------------------------
// Меню устройств, переключение и уведомления.
// ---------------------------------------------------------------------------

func buildDeviceMenu(devices []audio.Device, currentID string) {
	menuMu.Lock()
	defer menuMu.Unlock()

	deviceByID = make(map[string]int, len(devices))
	menuItems = make([]*systray.MenuItem, len(devices))

	for i, dev := range devices {
		item := systray.AddMenuItemCheckbox(dev.Name, "Сделать устройством по умолчанию", dev.ID == currentID)
		menuItems[i] = item
		deviceByID[dev.ID] = i

		idx := i
		go func() {
			for range item.ClickedCh {
				selectDevice(devices[idx])
			}
		}()
	}
}

func selectDevice(dev audio.Device) {
	if err := switcher.SetDefault(dev.ID); err != nil {
		log.Printf("set default failed: %v", err)
		return
	}
	setChecked(dev.ID)
	announceSwitch(dev)
}

func onCycle(direction int) {
	dev, err := switcher.Cycle(direction)
	if err != nil {
		log.Printf("cycle failed: %v", err)
		return
	}
	setChecked(dev.ID)
	announceSwitch(dev)
}

func announceSwitch(dev audio.Device) {
	systray.SetTooltip("Volume Switch: " + dev.Name)
	notify.Show("Аудиовыход переключён", dev.Name)
}

func setChecked(id string) {
	menuMu.Lock()
	defer menuMu.Unlock()

	for _, item := range menuItems {
		item.Uncheck()
	}
	if idx, ok := deviceByID[id]; ok {
		menuItems[idx].Check()
	}
}

// ---------------------------------------------------------------------------
// Ниже — реализация глобального хука клавиатуры Windows (бывший пакет hotkey).
// ---------------------------------------------------------------------------

const (
	whKeyboardLL = 13
	wmKeyDown    = 0x0100
	wmSysKeyDown = 0x0104

	vkF5 = 0x74
	vkF7 = 0x76
	vkF8 = 0x77
)

// kbdllHookStruct — структура, которую Windows передаёт в hook-процедуру.
type kbdllHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// hotkeyListener подписывается на F5/F7/F8. Все поля опциональны.
type hotkeyListener struct {
	OnF5 func()
	OnF7 func()
	OnF8 func()

	mu   sync.Mutex
	hook uintptr
	proc uintptr // держим ссылку на callback, иначе GC его выкинет
}

var (
	user32HK   = syscall.NewLazyDLL("user32.dll")
	kernel32HK = syscall.NewLazyDLL("kernel32.dll")

	pSetWindowsHookEx    = user32HK.NewProc("SetWindowsHookExW")
	pUnhookWindowsHookEx = user32HK.NewProc("UnhookWindowsHookEx")
	pCallNextHookEx      = user32HK.NewProc("CallNextHookEx")
	pGetMessageW         = user32HK.NewProc("GetMessageW")

	pGetModuleHandleW = kernel32HK.NewProc("GetModuleHandleW")
)

// activeHK — текущий активный листенер. Хук глобальный (один на процесс),
// а обработчик берёт функции из этой переменной.
var (
	activeHKMu sync.Mutex
	activeHK   *hotkeyListener
)

// Start устанавливает хук и запускает цикл сообщений.
// Цикл сообщений обязателен: без него система не вызывает hook-процедуру.
func (l *hotkeyListener) Start() error {
	activeHKMu.Lock()
	if activeHK != nil {
		activeHKMu.Unlock()
		return errors.New("hotkey: listener already running")
	}
	activeHK = l
	activeHKMu.Unlock()

	hInst, _, _ := pGetModuleHandleW.Call(0)

	ready := make(chan error, 1)
	go func() {
		// Цикл сообщений Windows привязан к конкретному потоку.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		cb := syscall.NewCallback(hookProc)

		hook, _, errno := pSetWindowsHookEx.Call(
			uintptr(whKeyboardLL),
			cb,
			hInst,
			0,
		)
		if hook == 0 {
			ready <- errno
			return
		}

		l.mu.Lock()
		l.hook = hook
		l.proc = cb
		l.mu.Unlock()

		ready <- nil

		var msg [48]byte // MSG с запасом по размеру
		for {
			ret, _, _ := pGetMessageW.Call(
				uintptr(unsafe.Pointer(&msg[0])),
				0, 0, 0,
			)
			// 0 = WM_QUIT, ^uintptr(0) = -1 (ошибка) → выходим.
			if ret == 0 || ret == ^uintptr(0) {
				return
			}
		}
	}()

	return <-ready
}

// Stop снимает хук. После Stop листенер можно создать заново.
func (l *hotkeyListener) Stop() {
	l.mu.Lock()
	hook := l.hook
	l.hook = 0
	l.mu.Unlock()

	if hook != 0 {
		pUnhookWindowsHookEx.Call(hook)
	}

	activeHKMu.Lock()
	if activeHK == l {
		activeHK = nil
	}
	activeHKMu.Unlock()
}

// hookProc — глобальный callback. Не должен делать ничего тяжёлого.
func hookProc(nCode int, wParam uintptr, lParam uintptr) uintptr {
	if nCode < 0 {
		r, _, _ := pCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return r
	}

	if wParam != wmKeyDown && wParam != wmSysKeyDown {
		r, _, _ := pCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return r
	}

	kb := (*kbdllHookStruct)(unsafe.Pointer(lParam))

	activeHKMu.Lock()
	l := activeHK
	activeHKMu.Unlock()

	if l == nil {
		r, _, _ := pCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
		return r
	}

	switch kb.VkCode {
	case vkF5:
		if l.OnF5 != nil {
			go l.OnF5()
		}
		return 1
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

	r, _, _ := pCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return r
}
