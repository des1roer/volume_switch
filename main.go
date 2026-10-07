// Command volume_switch sits in the system tray. F5 toggles an MPC-HC
// progress overlay window, F7/F8 cycle the default Windows playback
// device, the same way Volume2 does.
package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"flag"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"log"
	"math"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"github.com/getlantern/systray"

	"fyne.io/fyne/v2/app"
	"fyne.io/fyne/v2/canvas"
	"fyne.io/fyne/v2/container"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/layout"
	"fyne.io/fyne/v2/theme"
	"fyne.io/fyne/v2/widget"

	"github.com/diegosz/go-wca/pkg/wca"
	"github.com/go-ole/go-ole"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const (
	defaultURL      = "http://localhost:7777/variables.html"
	defaultInterval = 500 * time.Millisecond
	windowTitle     = "MPC-HC Progress"
	logFileName     = "volume_switch.log"

	volumeWindowTitle     = "Volume Overlay"
	volumeOverlayDuration = 1200 * time.Millisecond
	volumeOverlayWidth    = float32(260)
	volumeOverlayHeight   = float32(160)
	volumeTickCount       = 10 // ползунок размечен от 0 до 100 с шагом 10 (11 отметок)
)

var (
	volumeFillColor  = color.NRGBA{R: 90, G: 200, B: 250, A: 255}
	volumeMuteColor  = color.NRGBA{R: 220, G: 70, B: 70, A: 255}
	volumeTrackColor = color.NRGBA{R: 60, G: 60, B: 66, A: 255}
	volumeTickColor  = color.NRGBA{R: 150, G: 150, B: 150, A: 255}
	volumeLabelColor = color.NRGBA{R: 170, G: 170, B: 170, A: 255}
)

// ---------------------------------------------------------------------------
// Логирование
// ---------------------------------------------------------------------------

// ensureConsole делает консоль видимой.
//   - Если консоли нет (сборка с -H windowsgui) — создаёт её через AllocConsole.
//   - Если консоль есть, но скрыта библиотекой systray — показывает обратно.
//
// systray при инициализации сам вызывает ShowWindow(SW_HIDE), поэтому
// без этого трюка в консоль ничего не видно.
func ensureConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	user32 := syscall.NewLazyDLL("user32.dll")

	getConsoleWindow := kernel32.NewProc("GetConsoleWindow")
	allocConsole := kernel32.NewProc("AllocConsole")
	showWindow := user32.NewProc("ShowWindow")

	const SW_SHOW = 5

	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		if r, _, _ := allocConsole.Call(); r == 0 {
			return
		}
		hwnd, _, _ = getConsoleWindow.Call()
		if hwnd == 0 {
			return
		}
		if f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0); err == nil {
			os.Stdout = f
			os.Stderr = f
			log.SetOutput(f)
		}
		if f, err := os.OpenFile("CONIN$", os.O_RDONLY, 0); err == nil {
			os.Stdin = f
		}
	}
	showWindow.Call(hwnd, SW_SHOW)
}

// setupLogging настраивает вывод: в консоль (debug) или в файл.
func setupLogging(debug bool) {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if debug {
		ensureConsole()
		log.SetOutput(os.Stderr)
		log.Printf("=== volume_switch starting (debug mode) ===")
		return
	}
	hideConsole()
	f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		log.SetOutput(io.Discard)
		return
	}
	log.SetOutput(io.MultiWriter(f))
	log.Printf("=== volume_switch starting (log -> %s) ===", logFileName)
}

// hideConsole отвязывает процесс от унаследованной консоли и закрывает её
// окно. Нужно для сборок без `-ldflags "-H=windowsgui"`: загрузчик Windows
// подключает консоль по subsystem в PE-заголовке ещё до того, как этот код
// успевает выполниться, независимо от флага -debug. FreeConsole() убирает
// этот случайный флеш консоли; правильная же сборка с -H=windowsgui вообще
// не создаёт консоль, и этот вызов там становится no-op.
func hideConsole() {
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	freeConsole := kernel32.NewProc("FreeConsole")
	freeConsole.Call()
}

// ---------------------------------------------------------------------------
// .env
// ---------------------------------------------------------------------------

// loadDotEnv читает KEY=VALUE построчно из .env и выставляет
// переменные окружения, не перезаписывая уже заданные извне.
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf(".env not loaded (%v) — использую переменные окружения/флаги", err)
		return
	}
	loaded := 0
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		value = strings.Trim(strings.TrimSpace(value), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			os.Setenv(key, value)
			loaded++
			log.Printf(".env: %s=%s", key, value)
		} else {
			log.Printf(".env: %s уже задан извне, пропускаю", key)
		}
	}
	log.Printf(".env loaded: %d переменных", loaded)
}

// ---------------------------------------------------------------------------
// Разбор variables.html MPC-HC
// ---------------------------------------------------------------------------

var (
	rePosition    = regexp.MustCompile(`<p id="position">(-?\d+)</p>`)
	reDuration    = regexp.MustCompile(`<p id="duration">(-?\d+)</p>`)
	rePositionStr = regexp.MustCompile(`<p id="positionstring">([^<]*)</p>`)
	reDurationStr = regexp.MustCompile(`<p id="durationstring">([^<]*)</p>`)
	reStateString = regexp.MustCompile(`<p id="statestring">([^<]*)</p>`)
	reFile        = regexp.MustCompile(`<p id="file">([^<]*)</p>`)
	reFilePath    = regexp.MustCompile(`<p id="filepath">([^<]*)</p>`)
)

type MPCState struct {
	Position    int64
	Duration    int64
	PositionStr string
	DurationStr string
	StateString string
	File        string
	FilePath    string
}

func extractInt(html string, re *regexp.Regexp) int64 {
	m := re.FindStringSubmatch(html)
	if len(m) < 2 {
		return 0
	}
	v, _ := strconv.ParseInt(m[1], 10, 64)
	return v
}

func extractStr(html string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(html)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func fetchState(url string, client *http.Client) (*MPCState, error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(body)

	return &MPCState{
		Position:    extractInt(html, rePosition),
		Duration:    extractInt(html, reDuration),
		PositionStr: extractStr(html, rePositionStr),
		DurationStr: extractStr(html, reDurationStr),
		StateString: extractStr(html, reStateString),
		File:        extractStr(html, reFile),
		FilePath:    extractStr(html, reFilePath),
	}, nil
}

func formatTime(ms int64) string {
	if ms < 0 {
		ms = 0
	}
	total := ms / 1000
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	if h > 0 {
		return fmt.Sprintf("%d:%02d:%02d", h, m, s)
	}
	return fmt.Sprintf("%02d:%02d", m, s)
}

// ---------------------------------------------------------------------------
// Win32-хелперы: монитор под курсором и высота заголовка окна
// ---------------------------------------------------------------------------

type winRect struct {
	Left, Top, Right, Bottom int32
}

type winPoint struct {
	X, Y int32
}

type winMonitorInfo struct {
	CbSize    uint32
	RcMonitor winRect
	RcWork    winRect
	DwFlags   uint32
}

// screenLayout возвращает рабочую область (без панели задач) и полные границы
// монитора, на котором сейчас находится курсор мыши.
func screenLayout() (work, monitor winRect) {
	user32 := syscall.NewLazyDLL("user32.dll")
	getCursorPos := user32.NewProc("GetCursorPos")
	monitorFromPoint := user32.NewProc("MonitorFromPoint")
	getMonitorInfoW := user32.NewProc("GetMonitorInfoW")

	const monitorDefaultToNearest = 2

	var pt winPoint
	getCursorPos.Call(uintptr(unsafe.Pointer(&pt)))

	ptPacked := uintptr(uint32(pt.X)) | uintptr(uint32(pt.Y))<<32
	hMonitor, _, _ := monitorFromPoint.Call(ptPacked, monitorDefaultToNearest)

	var mi winMonitorInfo
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	getMonitorInfoW.Call(hMonitor, uintptr(unsafe.Pointer(&mi)))

	return mi.RcWork, mi.RcMonitor
}

// titleBarHeight возвращает высоту заголовка окна Windows (вместе с рамкой).
func titleBarHeight() int32 {
	user32 := syscall.NewLazyDLL("user32.dll")
	getSystemMetrics := user32.NewProc("GetSystemMetrics")

	const (
		smCyCaption      = 4
		smCySizeFrame    = 33
		smCxPaddedBorder = 92
	)

	cyCaption, _, _ := getSystemMetrics.Call(smCyCaption)
	cySizeFrame, _, _ := getSystemMetrics.Call(smCySizeFrame)
	cxPaddedBorder, _, _ := getSystemMetrics.Call(smCxPaddedBorder)

	return int32(cyCaption) + int32(cySizeFrame) + int32(cxPaddedBorder)
}

// ---------------------------------------------------------------------------
// Переключение аудиоустройств (WASAPI/COM)
//
// Switcher owns the COM objects needed to list and change the default
// playback device: IMMDeviceEnumerator for enumeration and the
// undocumented IPolicyConfig COM interface (the same one used by Volume2,
// EarTrumpet, AudioSwitcher, etc.) for changing the system default.
//
// All COM calls happen on a single dedicated OS thread (the worker
// goroutine) because the COM objects are created in that thread's
// apartment; exec() serializes every request onto it.
// ---------------------------------------------------------------------------

// Device is a playback endpoint exposed to the UI layer.
type Device struct {
	ID   string
	Name string
}

// Switcher owns the COM objects needed to list and change the default
// playback device.
type Switcher struct {
	reqCh chan func()
	quit  chan struct{}

	mmde   *wca.IMMDeviceEnumerator
	policy *wca.IPolicyConfigVista
}

// NewSwitcher starts the worker goroutine, initializes COM on it and
// creates the long-lived COM objects. It blocks until initialization
// finishes (or fails).
func NewSwitcher() (*Switcher, error) {
	s := &Switcher{
		reqCh: make(chan func()),
		quit:  make(chan struct{}),
	}

	errCh := make(chan error, 1)
	go s.run(errCh)

	if err := <-errCh; err != nil {
		return nil, err
	}
	return s, nil
}

func (s *Switcher) run(errCh chan error) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	if err := ole.CoInitializeEx(0, ole.COINIT_APARTMENTTHREADED); err != nil {
		errCh <- err
		return
	}
	defer ole.CoUninitialize()

	var mmde *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(wca.CLSID_MMDeviceEnumerator, 0, wca.CLSCTX_ALL, wca.IID_IMMDeviceEnumerator, &mmde); err != nil {
		errCh <- err
		return
	}
	defer mmde.Release()

	var policy *wca.IPolicyConfigVista
	if err := wca.CoCreateInstance(wca.GUID_CPolicyConfigVistaClient, 0, wca.CLSCTX_ALL, wca.GUID_IPolicyConfigVista, &policy); err != nil {
		errCh <- err
		return
	}
	defer policy.Release()

	s.mmde = mmde
	s.policy = policy
	errCh <- nil

	for {
		select {
		case f := <-s.reqCh:
			f()
		case <-s.quit:
			return
		}
	}
}

// exec runs f on the worker goroutine/thread and waits for it to finish.
func (s *Switcher) exec(f func() error) error {
	done := make(chan error, 1)
	s.reqCh <- func() { done <- f() }
	return <-done
}

// Close stops the worker goroutine and releases every COM object.
func (s *Switcher) Close() {
	close(s.quit)
}

// ListActiveDevices returns every active playback endpoint.
func (s *Switcher) ListActiveDevices() ([]Device, error) {
	var devices []Device
	err := s.exec(func() error {
		var err error
		devices, err = s.listActiveDevicesLocked()
		return err
	})
	return devices, err
}

// DefaultDeviceID returns the ID of the current default playback endpoint
// (console role).
func (s *Switcher) DefaultDeviceID() (string, error) {
	var id string
	err := s.exec(func() error {
		var err error
		id, err = s.defaultDeviceIDLocked()
		return err
	})
	return id, err
}

// SetDefault makes the device with the given ID the default endpoint for
// every role (console, multimedia, communications), matching what
// Volume2-style switchers do.
func (s *Switcher) SetDefault(id string) error {
	return s.exec(func() error {
		return s.setDefaultLocked(id)
	})
}

// Cycle moves the default device forward (direction > 0) or backward
// (direction < 0) through the list of active devices and returns the newly
// selected device. The whole read-modify-write happens as a single
// operation on the worker goroutine so it cannot race with a concurrent
// menu click or hotkey press.
func (s *Switcher) Cycle(direction int) (Device, error) {
	var result Device
	err := s.exec(func() error {
		devices, err := s.listActiveDevicesLocked()
		if err != nil {
			return err
		}
		if len(devices) == 0 {
			return errors.New("no active playback devices")
		}

		currentID, err := s.defaultDeviceIDLocked()
		if err != nil {
			return err
		}

		idx := 0
		for i, d := range devices {
			if d.ID == currentID {
				idx = i
				break
			}
		}

		next := ((idx+direction)%len(devices) + len(devices)) % len(devices)
		if err := s.setDefaultLocked(devices[next].ID); err != nil {
			return err
		}
		result = devices[next]
		return nil
	})
	return result, err
}

// --- internal helpers; must only be called from the worker goroutine ---

func (s *Switcher) listActiveDevicesLocked() ([]Device, error) {
	var collection *wca.IMMDeviceCollection
	if err := s.mmde.EnumAudioEndpoints(wca.ERender, wca.DEVICE_STATE_ACTIVE, &collection); err != nil {
		return nil, err
	}
	defer collection.Release()

	var count uint32
	if err := collection.GetCount(&count); err != nil {
		return nil, err
	}

	devices := make([]Device, 0, count)
	for i := uint32(0); i < count; i++ {
		var dev *wca.IMMDevice
		if err := collection.Item(i, &dev); err != nil {
			return nil, err
		}

		id, idErr := deviceID(dev)
		name, nameErr := deviceName(dev)
		dev.Release()

		if idErr != nil {
			return nil, idErr
		}
		if nameErr != nil {
			return nil, nameErr
		}
		devices = append(devices, Device{ID: id, Name: name})
	}
	return devices, nil
}

func (s *Switcher) defaultDeviceIDLocked() (string, error) {
	var dev *wca.IMMDevice
	if err := s.mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &dev); err != nil {
		return "", err
	}
	defer dev.Release()

	var id string
	if err := dev.GetId(&id); err != nil {
		return "", err
	}
	return id, nil
}

func (s *Switcher) setDefaultLocked(id string) error {
	for _, role := range []wca.ERole{wca.EConsole, wca.EMultimedia, wca.ECommunications} {
		if err := s.policy.SetDefaultEndpoint(id, role); err != nil {
			return err
		}
	}
	return nil
}

// CurrentVolume возвращает текущий уровень громкости (0..1) и состояние mute
// текущего default-устройства воспроизведения. Используется volumePoller
// (см. ниже) для отслеживания изменений громкости.
func (s *Switcher) CurrentVolume() (level float64, muted bool, err error) {
	err = s.exec(func() error {
		l, m, e := s.currentVolumeLocked()
		level, muted = l, m
		return e
	})
	return
}

func (s *Switcher) currentVolumeLocked() (level float64, muted bool, err error) {
	var dev *wca.IMMDevice
	if err = s.mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &dev); err != nil {
		return
	}
	defer dev.Release()

	var aev *wca.IAudioEndpointVolume
	if err = dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
		return
	}
	defer aev.Release()

	var scalar float32
	if err = aev.GetMasterVolumeLevelScalar(&scalar); err != nil {
		return
	}
	var isMuted bool
	if err = aev.GetMute(&isMuted); err != nil {
		return
	}
	level, muted = float64(scalar), isMuted
	return
}

func deviceID(dev *wca.IMMDevice) (string, error) {
	var id string
	if err := dev.GetId(&id); err != nil {
		return "", err
	}
	return id, nil
}

func deviceName(dev *wca.IMMDevice) (string, error) {
	var ps *wca.IPropertyStore
	if err := dev.OpenPropertyStore(wca.STGM_READ, &ps); err != nil {
		return "", err
	}
	defer ps.Release()

	var pv wca.PROPVARIANT
	if err := ps.GetValue(&wca.PKEY_Device_FriendlyName, &pv); err != nil {
		return "", err
	}
	return pv.String(), nil
}

// ---------------------------------------------------------------------------
// Монитор глобальной громкости (поллинг через Switcher)
//
// Изначально громкость отслеживалась через push-уведомления WASAPI
// (IAudioEndpointVolume::RegisterControlChangeNotify + самодельный
// IAudioEndpointVolumeCallback, по аналогии с IMMNotificationClient из
// github.com/diegosz/go-wca). Регистрация проходила успешно (Windows вызывал
// AddRef на наш колбэк), но сам OnNotify ни разу не сработал — даже после
// того как в цикл сообщений STA-потока добавили TranslateMessage/
// DispatchMessageW. Разбираться дальше в недрах доставки COM-уведомлений для
// конкретного драйвера/окружения оказалось не ценой задачи — вместо этого
// громкость просто периодически опрашивается через Switcher.CurrentVolume().
// Это не так элегантно, как push-модель, зато гарантированно работает
// независимо от драйвера и не требует отдельного потока/COM-квартиры/
// самодельного vtable.
// ---------------------------------------------------------------------------

const volumePollInterval = 120 * time.Millisecond

// startVolumePoller периодически опрашивает switcher.CurrentVolume() и
// вызывает onChange при изменении уровня (с точностью до процента) или
// состояния mute. Первое измерение только запоминается как база для
// сравнения — не вызывает onChange, чтобы оверлей не всплывал при старте
// программы.
func startVolumePoller(switcher *Switcher, onChange func(level float64, muted bool)) {
	go func() {
		ticker := time.NewTicker(volumePollInterval)
		defer ticker.Stop()

		lastPercent := -1
		lastMuted := false
		for range ticker.C {
			level, muted, err := switcher.CurrentVolume()
			if err != nil {
				continue
			}
			percent := int(math.Round(level * 100))
			if lastPercent == -1 {
				lastPercent, lastMuted = percent, muted
				continue
			}
			if percent == lastPercent && muted == lastMuted {
				continue
			}
			lastPercent, lastMuted = percent, muted
			onChange(level, muted)
		}
	}()
}

// ---------------------------------------------------------------------------
// Баллон-уведомления
//
// showNotification shows a native Windows balloon/toast notification
// anchored to this process's own tray icon (the one created by
// github.com/getlantern/systray), so switching the audio device gives
// visible feedback even when the tray tooltip alone would go unnoticed.
//
// There is no public systray API to show a balloon, so this locates the
// hidden tray window systray created (window class "SystrayClass", icon ID
// 100 - both hardcoded in systray v1.2.2) and calls Shell_NotifyIcon on it
// directly. If a future systray upgrade changes either value, showNotification
// simply becomes a silent no-op.
// ---------------------------------------------------------------------------

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
	user32N  = windows.NewLazySystemDLL("user32.dll")
	shell32N = windows.NewLazySystemDLL("shell32.dll")

	procEnumWindows              = user32N.NewProc("EnumWindows")
	procGetClassNameW            = user32N.NewProc("GetClassNameW")
	procGetWindowThreadProcessId = user32N.NewProc("GetWindowThreadProcessId")
	procShellNotifyIconW         = shell32N.NewProc("Shell_NotifyIconW")
)

// showNotification displays a balloon notification with the given title and
// body. It is a no-op if this process's tray window cannot be found (e.g.
// called before systray finished initializing, or after a dependency
// upgrade changed the implementation details above).
func showNotification(title, body string) {
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

// ---------------------------------------------------------------------------
// Генерация иконки трея
//
// trayIconData holds a small icon generated at runtime (in .ico format,
// PNG-compressed, supported since Windows Vista) so the project does not
// depend on any external .ico asset.
// ---------------------------------------------------------------------------

var trayIconData = renderTrayIcon()

func renderTrayIcon() []byte {
	const size = 32

	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{R: 30, G: 30, B: 35, A: 255}
	fg := color.RGBA{R: 90, G: 200, B: 250, A: 255}

	cx, cy, r := size/2, size/2, size/2-4
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			dx, dy := x-cx, y-cy
			if dx*dx+dy*dy <= r*r {
				img.Set(x, y, fg)
			} else {
				img.Set(x, y, bg)
			}
		}
	}

	var pngBuf bytes.Buffer
	if err := png.Encode(&pngBuf, img); err != nil {
		panic(err)
	}

	return wrapICO(pngBuf.Bytes(), size, size)
}

// wrapICO wraps a single PNG image into a minimal valid .ico container.
func wrapICO(pngData []byte, w, h int) []byte {
	var buf bytes.Buffer

	// ICONDIR
	binary.Write(&buf, binary.LittleEndian, uint16(0)) // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // type: icon
	binary.Write(&buf, binary.LittleEndian, uint16(1)) // image count

	// ICONDIRENTRY (dimensions >=256 are encoded as 0 per spec; not needed here)
	buf.WriteByte(byte(w))
	buf.WriteByte(byte(h))
	buf.WriteByte(0)                                              // color palette
	buf.WriteByte(0)                                              // reserved
	binary.Write(&buf, binary.LittleEndian, uint16(1))            // color planes
	binary.Write(&buf, binary.LittleEndian, uint16(32))           // bits per pixel
	binary.Write(&buf, binary.LittleEndian, uint32(len(pngData))) // size of image data
	binary.Write(&buf, binary.LittleEndian, uint32(6+16))         // offset to image data

	buf.Write(pngData)
	return buf.Bytes()
}

// ---------------------------------------------------------------------------
// Fyne-окно (встроенный mpcprogress)
// ---------------------------------------------------------------------------

var (
	fyneApp    fyne.App
	mainWindow fyne.Window

	winMu      sync.Mutex
	winVisible bool

	elapsedLabel   *widget.Label
	remainingLabel *widget.Label
	progressBar    *widget.ProgressBar

	// progressText — текущий текст поверх полосы прогресса (имя файла и
	// статус воспроизведения), читается из progressBar.TextFormatter.
	progressText string

	// currentFilePath — полный путь к текущему файлу MPC-HC (поле filepath
	// из variables.html), используется по клику колесом для открытия папки.
	currentFilePath string

	// volumeWindow и компоненты ниже — оверлей-ползунок уровня громкости,
	// показывается по событиям volumeMonitor (см. showVolumeOverlay). Все
	// позиции/размеры примитивов выставляются вручную в layoutVolumeOverlay
	// при каждом показе (окно без автоматического layout-менеджера, т.к.
	// размер оверлея каждый раз пересчитывается под монитор курсора).
	volumeWindow     fyne.Window
	volumePercent    *canvas.Text
	volumeBarTrack   *canvas.Rectangle
	volumeBarFill    *canvas.Rectangle
	volumeTicks      []*canvas.Line
	volumeTickLabels []*canvas.Text

	volumeMu        sync.Mutex
	volumeHideTimer *time.Timer
)

// clickCatcher — прозрачный виджет на весь оверлей, который ловит клик
// средней кнопкой мыши (колесом) и открывает папку с текущим файлом в
// проводнике. Остальные клики (левая/правая кнопка) не перехватывает.
type clickCatcher struct {
	widget.BaseWidget
}

func newClickCatcher() *clickCatcher {
	c := &clickCatcher{}
	c.ExtendBaseWidget(c)
	return c
}

func (c *clickCatcher) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(canvas.NewRectangle(color.Transparent))
}

func (c *clickCatcher) MouseDown(*desktop.MouseEvent) {}

func (c *clickCatcher) MouseUp(ev *desktop.MouseEvent) {
	if ev.Button != desktop.MouseButtonTertiary {
		return
	}
	log.Printf("overlay: клик колесом, открываю папку с файлом")
	openContainingFolder(currentFilePath)
}

// openContainingFolder открывает проводник с выделенным файлом (как
// «Показать в папке»). Если путь ещё не известен (MPC-HC не опрошен или
// ничего не загружено), ничего не делает.
//
// explorer.exe ожидает `/select,"путь"` — свитч БЕЗ кавычек, путь В кавычках
// сразу после запятой, без пробела между ними. exec.Command так собрать
// нельзя: если склеить "/select,"+path в один элемент Args, Go из-за пробелов
// в пути оборачивает весь токен в кавычки целиком ("/select,E:\...\file.mkv"),
// explorer не распознаёт свитч внутри такой строки и открывает папку по
// умолчанию (Документы) вместо нужной. Поэтому командная строка собирается
// вручную через SysProcAttr.CmdLine, в обход автоматического квотирования Go.
func openContainingFolder(path string) {
	if path == "" {
		log.Printf("overlay: путь к файлу ещё неизвестен, пропускаю")
		return
	}
	clean := filepath.Clean(path)
	cmd := exec.Command("explorer")
	// SysProcAttr.CmdLine заменяет ВЕСЬ GetCommandLineW() процесса, поэтому
	// имя программы нужно указать в нём же самим — иначе explorer видит argv0
	// как наш "/select,..." свитч и тоже не распознаёт его.
	cmd.SysProcAttr = &syscall.SysProcAttr{
		CmdLine: `explorer.exe /select,"` + clean + `"`,
	}
	if err := cmd.Start(); err != nil {
		log.Printf("overlay: не удалось открыть проводник: %v", err)
	}
}

// runFyne запускает Fyne-приложение. Вызывается в отдельной горутине.
// Блокируется до закрытия приложения (обычно — до systray.Quit()).
func runFyne(url string, interval time.Duration) {
	runtime.LockOSThread()
	log.Printf("fyne: инициализация (url=%s, interval=%s)", url, interval)

	fyneApp = app.New()
	fyneApp.Settings().SetTheme(theme.DarkTheme())
	mainWindow = fyneApp.NewWindow(windowTitle)

	// --- Размер/позиция окна под монитор с курсором ---
	windowWidth, windowHeight := float32(900), float32(170)
	posX, posY := 0, 0
	if work, monitor := screenLayout(); work.Right > work.Left {
		titleBar := titleBarHeight()
		windowWidth = float32(work.Right - work.Left)
		windowHeight = float32(monitor.Bottom-monitor.Top)*0.10 - float32(titleBar)
		posX, posY = int(work.Left), int(work.Top)+int(titleBar)
		log.Printf("fyne: монитор work=(%d,%d)-(%d,%d) monitor=(%d,%d)-(%d,%d) titleBar=%d",
			work.Left, work.Top, work.Right, work.Bottom,
			monitor.Left, monitor.Top, monitor.Right, monitor.Bottom, titleBar)
	}
	log.Printf("fyne: окно %0.fx%0.f @ (%d,%d)", windowWidth, windowHeight, posX, posY)

	mainWindow.Resize(fyne.NewSize(windowWidth, windowHeight))
	if dw, ok := mainWindow.(desktop.Window); ok {
		// Fyne игнорирует RequestPosition(0, 0), считая это «позиция не задана».
		if posX == 0 && posY == 0 {
			posX = 1
		}
		dw.RequestPosition(posX, posY)
	}

	// --- Виджеты ---
	// Имя файла и статус выводятся прямо на полосе прогресса (через
	// TextFormatter), а не отдельной строкой — главный элемент оверлея это
	// сама полоса, текст встроен в неё.
	elapsedLabel = widget.NewLabelWithStyle("00:00",
		fyne.TextAlignLeading, fyne.TextStyle{Bold: true, Monospace: true})
	remainingLabel = widget.NewLabelWithStyle("00:00",
		fyne.TextAlignTrailing, fyne.TextStyle{Bold: true, Monospace: true})

	progressText = "Ожидание подключения к MPC-HC..."
	progressBar = widget.NewProgressBar()
	progressBar.TextFormatter = func() string { return progressText }

	progressRow := container.NewBorder(nil, nil, elapsedLabel, remainingLabel, progressBar)
	pad := theme.Padding()
	body := container.New(layout.NewCustomPaddedLayout(pad, pad/2, pad, pad), progressRow)
	mainWindow.SetContent(container.NewStack(body, newClickCatcher()))

	// --- Цикл опроса MPC-HC в отдельной горутине ---
	client := &http.Client{Timeout: 2 * time.Second}
	go func() {
		log.Printf("mpc poll: старт цикла (interval=%s)", interval)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		var lastErr string
		for range ticker.C {
			st, err := fetchState(url, client)
			if err != nil {
				if err.Error() != lastErr {
					log.Printf("mpc poll: ошибка: %v", err)
					lastErr = err.Error()
				}
				fyne.Do(func() {
					progressText = "⚠ Нет связи с MPC-HC: " + err.Error()
					progressBar.Refresh()
				})
				continue
			}
			if lastErr != "" {
				log.Printf("mpc poll: связь восстановлена")
				lastErr = ""
			}
			stCopy := st
			fyne.Do(func() {
				currentFilePath = stCopy.FilePath
				if stCopy.Duration <= 0 {
					progressText = "Нет активного воспроизведения"
					progressBar.SetValue(0)
					elapsedLabel.SetText("00:00")
					remainingLabel.SetText("00:00")
					return
				}
				progress := float64(stCopy.Position) / float64(stCopy.Duration)
				if progress < 0 {
					progress = 0
				}
				if progress > 1 {
					progress = 1
				}

				elapsed := stCopy.PositionStr
				if elapsed == "" {
					elapsed = formatTime(stCopy.Position)
				}
				remaining := formatTime(stCopy.Duration - stCopy.Position)

				elapsedLabel.SetText(elapsed)
				remainingLabel.SetText("-" + remaining)
				progressText = fmt.Sprintf("%s   —   %.1f%%  %s  (из %s)",
					stCopy.File, progress*100, stCopy.StateString, formatTime(stCopy.Duration))
				progressBar.SetValue(progress)
			})
		}
	}()

	// --- Оверлей уровня громкости: большой ползунок с градацией 0..100,
	// см. volumeMonitor. Размер и позиция пересчитываются под монитор
	// курсора при каждом показе (layoutVolumeOverlay), поэтому все примитивы
	// собраны в контейнер без автоматического layout-менеджера.
	volumeWindow = fyneApp.NewWindow(volumeWindowTitle)

	volumePercent = canvas.NewText("", color.White)
	volumePercent.Alignment = fyne.TextAlignCenter
	volumePercent.TextStyle = fyne.TextStyle{Bold: true}

	volumeBarTrack = canvas.NewRectangle(volumeTrackColor)
	volumeBarFill = canvas.NewRectangle(volumeFillColor)

	overlayObjects := []fyne.CanvasObject{volumeBarTrack, volumeBarFill, volumePercent}
	volumeTicks = make([]*canvas.Line, volumeTickCount+1)
	volumeTickLabels = make([]*canvas.Text, volumeTickCount+1)
	for i := range volumeTicks {
		line := canvas.NewLine(volumeTickColor)
		label := canvas.NewText(strconv.Itoa(i*100/volumeTickCount), volumeLabelColor)
		label.Alignment = fyne.TextAlignCenter
		volumeTicks[i] = line
		volumeTickLabels[i] = label
		overlayObjects = append(overlayObjects, line, label)
	}
	volumeWindow.SetContent(container.NewWithoutLayout(overlayObjects...))

	// Стартуем скрытыми — mainWindow покажем по F5, volumeWindow по событию
	// от volumeMonitor.
	mainWindow.Hide()
	volumeWindow.Hide()
	log.Printf("fyne: окно создано, стартует скрытым")

	fyneApp.Run()
	log.Printf("fyne: цикл приложения завершён")
}

var (
	fixOverlayChromeOnce       sync.Once
	fixVolumeOverlayChromeOnce sync.Once
)

// fixOverlayWindowChrome снимает нативное окно с заданным заголовком с панели
// задач (WS_EX_TOOLWINDOW — приложение представлено только иконкой в трее) и
// перекрашивает его системный заголовок в тёмный через DWM, чтобы белая
// полоса сверху не выбивалась из тёмной темы Fyne. Рамка/заголовок при этом
// остаются — попытка убрать их совсем (GWL_STYLE) приводила к тому, что GLFW
// пересчитывал размер окна по устаревшим метрикам бывшего заголовка и
// раздувал окно заново при каждом изменении; DWM-атрибут такой проблемы не
// создаёт, т.к. не трогает геометрию. Fyne/GLFW создаёт нативный HWND лениво,
// только при первом Show(), поэтому вызывать это нужно уже после него;
// делается один раз за всё время работы процесса — но отдельно для каждого
// окна (каждый вызывающий передаёт свой собственный *sync.Once).
func fixOverlayWindowChrome(title string, once *sync.Once) {
	once.Do(func() {
		user32 := syscall.NewLazyDLL("user32.dll")
		findWindowW := user32.NewProc("FindWindowW")
		getWindowLongPtrW := user32.NewProc("GetWindowLongPtrW")
		setWindowLongPtrW := user32.NewProc("SetWindowLongPtrW")
		setWindowPos := user32.NewProc("SetWindowPos")

		dwmapi := syscall.NewLazyDLL("dwmapi.dll")
		dwmSetWindowAttribute := dwmapi.NewProc("DwmSetWindowAttribute")

		const (
			wsExToolWindow  = 0x00000080
			wsExAppWindow   = 0x00040000
			swpNoMove       = 0x0002
			swpNoSize       = 0x0001
			swpNoZOrder     = 0x0004
			swpNoActivate   = 0x0010
			swpFrameChanged = 0x0020

			dwmwaUseImmersiveDarkMode = 20
		)
		// -20 переполняет uintptr как константа компиляции; через typed
		// переменную Go делает корректное sign-extension в рантайме.
		var gwlExStyle int32 = -20

		titlePtr, err := syscall.UTF16PtrFromString(title)
		if err != nil {
			log.Printf("overlay-chrome: не удалось подготовить заголовок: %v", err)
			return
		}
		hwnd, _, _ := findWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
		if hwnd == 0 {
			log.Printf("overlay-chrome: окно %q не найдено, пропускаю", title)
			return
		}

		exStyle, _, _ := getWindowLongPtrW.Call(hwnd, uintptr(gwlExStyle))
		newExStyle := (exStyle &^ uintptr(wsExAppWindow)) | uintptr(wsExToolWindow)
		setWindowLongPtrW.Call(hwnd, uintptr(gwlExStyle), newExStyle)

		// SWP_FRAMECHANGED заставляет Explorer пересчитать кнопку в панели
		// задач без скрытия/показа окна — в отличие от ShowWindow(HIDE)+
		// ShowWindow(SHOW), это не ломает перерисовку содержимого GLFW/Fyne.
		setWindowPos.Call(hwnd, 0, 0, 0, 0, 0,
			uintptr(swpNoMove|swpNoSize|swpNoZOrder|swpNoActivate|swpFrameChanged))

		darkMode := int32(1)
		ret, _, _ := dwmSetWindowAttribute.Call(hwnd, uintptr(dwmwaUseImmersiveDarkMode),
			uintptr(unsafe.Pointer(&darkMode)), unsafe.Sizeof(darkMode))

		log.Printf("overlay-chrome: кнопка в панели задач для %q скрыта, тёмный заголовок: ok=%v", title, ret == 0)
	})
}

// toggleMPCWindow показывает окно, если оно скрыто, и прячет, если показано.
func toggleMPCWindow() {
	fyne.Do(func() {
		if mainWindow == nil {
			log.Printf("F5: окно ещё не создано")
			return
		}
		winMu.Lock()
		defer winMu.Unlock()
		if winVisible {
			mainWindow.Hide()
			winVisible = false
			log.Printf("F5: окно скрыто")
		} else {
			mainWindow.Show()
			fixOverlayWindowChrome(windowTitle, &fixOverlayChromeOnce)
			winVisible = true
			log.Printf("F5: окно показано")
		}
	})
}

// layoutVolumeOverlay расставляет все примитивы ползунка (трек, заливку,
// текст процента, отметки и подписи градации 0..100) под уже вычисленный
// размер окна width x height. Окно создано через container.NewWithoutLayout,
// поэтому автоматической раскладки нет — координаты считаются вручную и
// пересчитываются при каждом показе, т.к. размер каждый раз подгоняется под
// монитор курсора.
func layoutVolumeOverlay(width, height float32, level float64, muted bool) {
	pad := width * 0.06
	barX := pad
	barWidth := width - 2*pad
	barHeight := height * 0.22
	barY := height * 0.5
	radius := barHeight / 2

	volumeBarTrack.CornerRadius = radius
	volumeBarTrack.Resize(fyne.NewSize(barWidth, barHeight))
	volumeBarTrack.Move(fyne.NewPos(barX, barY))
	volumeBarTrack.Refresh()

	clamped := level
	if clamped < 0 {
		clamped = 0
	} else if clamped > 1 {
		clamped = 1
	}
	volumeBarFill.FillColor = volumeFillColor
	if muted {
		volumeBarFill.FillColor = volumeMuteColor
	}
	volumeBarFill.CornerRadius = radius
	volumeBarFill.Resize(fyne.NewSize(barWidth*float32(clamped), barHeight))
	volumeBarFill.Move(fyne.NewPos(barX, barY))
	volumeBarFill.Refresh()

	text := fmt.Sprintf("%d%%", int(math.Round(level*100)))
	if muted {
		text = "MUTE"
	}
	volumePercent.Text = text
	volumePercent.TextSize = height * 0.26
	volumePercent.Resize(fyne.NewSize(width, height*0.4))
	volumePercent.Move(fyne.NewPos(0, height*0.05))
	volumePercent.Refresh()

	tickY := barY + barHeight + 6
	labelSize := height * 0.06
	const labelWidth = float32(48)
	for i, line := range volumeTicks {
		x := barX + barWidth*float32(i)/float32(volumeTickCount)
		line.Position1 = fyne.NewPos(x, tickY)
		line.Position2 = fyne.NewPos(x, tickY+10)
		line.StrokeWidth = 2
		line.Refresh()

		label := volumeTickLabels[i]
		label.TextSize = labelSize
		label.Resize(fyne.NewSize(labelWidth, labelSize+4))
		label.Move(fyne.NewPos(x-labelWidth/2, tickY+12))
		label.Refresh()
	}
}

// showVolumeOverlay показывает оверлей-ползунок уровня громкости (0..100,
// с градацией через каждые 10) в четверть экрана (половина ширины и половина
// высоты рабочей области монитора под курсором мыши), прижатый к левому
// нижнему углу, и прячет его через volumeOverlayDuration после последнего
// вызова. Таймер скрытия каждый раз перезапускается (debounce), чтобы при
// быстрой серии событий — например, при удержании клавиши громкости —
// оверлей не мигал, а гас один раз после того, как изменения прекратились.
func showVolumeOverlay(level float64, muted bool) {
	fyne.Do(func() {
		if volumeWindow == nil {
			return
		}

		width, height := volumeOverlayWidth, volumeOverlayHeight
		x, y := 0, 0
		if work, _ := screenLayout(); work.Right > work.Left {
			width = float32(work.Right-work.Left) / 2
			height = float32(work.Bottom-work.Top) / 2
			x = int(work.Left)
			y = int(work.Bottom) - int(height)
			if x == 0 && y == 0 {
				x = 1
			}
		}

		layoutVolumeOverlay(width, height, level, muted)
		volumeWindow.Resize(fyne.NewSize(width, height))
		if dw, ok := volumeWindow.(desktop.Window); ok {
			dw.RequestPosition(x, y)
		}
		log.Printf("volume overlay: показываю размер=%.0fx%.0f позиция=(%d,%d) level=%.0f%% muted=%v",
			width, height, x, y, level*100, muted)

		volumeWindow.Show()
		fixOverlayWindowChrome(volumeWindowTitle, &fixVolumeOverlayChromeOnce)
	})

	volumeMu.Lock()
	defer volumeMu.Unlock()
	if volumeHideTimer != nil {
		volumeHideTimer.Stop()
	}
	volumeHideTimer = time.AfterFunc(volumeOverlayDuration, func() {
		fyne.Do(func() {
			if volumeWindow != nil {
				volumeWindow.Hide()
			}
		})
	})
}

// onVolumeChanged — коллбэк volumeMonitor; вызывается с его собственного
// потока, поэтому вся работа с UI внутри showVolumeOverlay идёт через
// fyne.Do.
func onVolumeChanged(level float64, muted bool) {
	showVolumeOverlay(level, muted)
}

// ---------------------------------------------------------------------------
// Автозапуск через реестр (HKCU\...\Run)
// ---------------------------------------------------------------------------

const (
	autostartRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Run`
	autostartValueName    = "VolumeSwitch"
)

// autostartCommand возвращает командную строку для записи в Run: путь к
// текущему exe в кавычках (без экранирования backslash — простое
// оборачивание в кавычки, т.к. %q из fmt испортил бы пути с обратным слэшем).
func autostartCommand() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return `"` + exe + `"`, nil
}

// isAutostartEnabled проверяет, что значение в HKCU\...\Run существует и
// указывает именно на текущий exe (а не на устаревший путь).
func isAutostartEnabled() bool {
	cmd, err := autostartCommand()
	if err != nil {
		return false
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer key.Close()
	val, _, err := key.GetStringValue(autostartValueName)
	if err != nil {
		return false
	}
	return val == cmd
}

// setAutostart включает или выключает автозапуск, записывая/удаляя значение
// в HKCU\...\Run.
func setAutostart(enable bool) error {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer key.Close()

	if !enable {
		err := key.DeleteValue(autostartValueName)
		if errors.Is(err, registry.ErrNotExist) {
			return nil
		}
		return err
	}

	cmd, err := autostartCommand()
	if err != nil {
		return err
	}
	return key.SetStringValue(autostartValueName, cmd)
}

// ---------------------------------------------------------------------------
// systray + аудиоустройства
// ---------------------------------------------------------------------------

var (
	switcher *Switcher
	hk       *hotkeyListener

	menuMu     sync.Mutex
	menuItems  []*systray.MenuItem
	deviceByID map[string]int
)

// debugMode хранит значение флага -debug для использования вне main()
// (см. onReady, где решается, нужно ли повторно показывать консоль).
var debugMode bool

func main() {
	debug := flag.Bool("debug", false, "Показывать консоль с дебаг-логами")
	urlFlag := flag.String("url", "", "URL variables.html (перекрывает MPC_URL)")
	intervalFlag := flag.Duration("interval", 0, "Интервал опроса (перекрывает MPC_INTERVAL)")
	flag.Parse()

	debugMode = *debug
	setupLogging(*debug)

	loadDotEnv(".env")

	url := os.Getenv("MPC_URL")
	if url == "" {
		url = defaultURL
	}
	if *urlFlag != "" {
		url = *urlFlag
	}

	interval := defaultInterval
	if v := os.Getenv("MPC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			interval = d
		} else {
			log.Printf("MPC_INTERVAL=%q не парсится как duration, беру %s", v, defaultInterval)
		}
	}
	if *intervalFlag > 0 {
		interval = *intervalFlag
	}

	log.Printf("config: url=%s interval=%s debug=%v", url, interval, *debug)

	// Fyne 2.8+ требует, чтобы app.New/NewWindow/Run вызывались на горутине main()
	// (см. https://docs.fyne.io/started/goroutines) — именно она закреплена за
	// главным потоком процесса пакетом systray в своём init(). Поэтому systray
	// переезжает в отдельную закреплённую горутину, а Fyne остаётся в main().
	go func() {
		runtime.LockOSThread()
		log.Printf("systray: запуск")
		systray.Run(onReady, onExit)
		log.Printf("systray: завершён, выход")
	}()

	runFyne(url, interval)
}

func onReady() {
	log.Printf("systray: onReady")
	if debugMode {
		ensureConsole() // systray мог спрятать консоль повторно
	}
	systray.SetIcon(trayIconData)
	systray.SetTitle("")
	systray.SetTooltip("Volume Switch")

	var err error
	switcher, err = NewSwitcher()
	if err != nil {
		log.Printf("audio: ошибка инициализации: %v", err)
		systray.SetTooltip("Volume Switch: audio init failed")
		return
	}
	log.Printf("audio: switcher создан")

	devices, err := switcher.ListActiveDevices()
	if err != nil {
		log.Printf("audio: не удалось получить список устройств: %v", err)
	}
	log.Printf("audio: найдено активных устройств: %d", len(devices))
	for i, d := range devices {
		log.Printf("  [%d] id=%s name=%q", i, d.ID, d.Name)
	}

	currentID, err := switcher.DefaultDeviceID()
	if err != nil {
		log.Printf("audio: не удалось получить устройство по умолчанию: %v", err)
	} else {
		log.Printf("audio: текущее устройство по умолчанию: %s", currentID)
	}

	buildDeviceMenu(devices, currentID)

	systray.AddSeparator()
	mShow := systray.AddMenuItem("Показать/скрыть MPC", "F5")
	go func() {
		for range mShow.ClickedCh {
			log.Printf("tray: пункт «Показать/скрыть MPC» нажат")
			toggleMPCWindow()
		}
	}()

	mAutostart := systray.AddMenuItemCheckbox(
		"Запускать при входе в Windows",
		"Добавить/убрать из автозагрузки Windows",
		isAutostartEnabled(),
	)
	go func() {
		for range mAutostart.ClickedCh {
			enable := !mAutostart.Checked()
			if err := setAutostart(enable); err != nil {
				log.Printf("autostart: не удалось изменить: %v", err)
				continue
			}
			if enable {
				mAutostart.Check()
			} else {
				mAutostart.Uncheck()
			}
			log.Printf("autostart: включён=%v", enable)
		}
	}()

	mQuit := systray.AddMenuItem("Выход", "Закрыть Volume Switch")
	go func() {
		<-mQuit.ClickedCh
		log.Printf("tray: «Выход» нажат")
		systray.Quit()
	}()

	hk = &hotkeyListener{
		OnF5: func() { log.Printf("hotkey: F5"); toggleMPCWindow() },
		OnF7: func() { log.Printf("hotkey: F7"); onCycle(-1) },
		OnF8: func() { log.Printf("hotkey: F8"); onCycle(1) },
	}
	if err := hk.Start(); err != nil {
		log.Printf("hotkey: не удалось установить хук: %v", err)
	} else {
		log.Printf("hotkey: хук установлен (F5/F7/F8)")
	}

	startVolumePoller(switcher, onVolumeChanged)
}

func onExit() {
	log.Printf("onExit: остановка")
	if hk != nil {
		hk.Stop()
		log.Printf("onExit: хук снят")
	}
	if fyneApp != nil {
		fyneApp.Quit()
		log.Printf("onExit: fyne остановлен")
	}
	if switcher != nil {
		switcher.Close()
		log.Printf("onExit: switcher закрыт")
	}
	log.Printf("onExit: всё завершено")
}

func buildDeviceMenu(devices []Device, currentID string) {
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
				log.Printf("tray: выбран %q", devices[idx].Name)
				selectDevice(devices[idx])
			}
		}()
	}
	log.Printf("menu: построено %d пунктов", len(devices))
}

func selectDevice(dev Device) {
	log.Printf("audio: SetDefault id=%s name=%q", dev.ID, dev.Name)
	if err := switcher.SetDefault(dev.ID); err != nil {
		log.Printf("audio: SetDefault failed: %v", err)
		return
	}
	setChecked(dev.ID)
	announceSwitch(dev)
}

func onCycle(direction int) {
	log.Printf("audio: Cycle direction=%d", direction)
	dev, err := switcher.Cycle(direction)
	if err != nil {
		log.Printf("audio: Cycle failed: %v", err)
		return
	}
	log.Printf("audio: новое устройство: id=%s name=%q", dev.ID, dev.Name)
	setChecked(dev.ID)
	announceSwitch(dev)
}

func announceSwitch(dev Device) {
	systray.SetTooltip("Volume Switch: " + dev.Name)
	showNotification("Аудиовыход переключён", dev.Name)
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
// Глобальный хук клавиатуры Windows (F5/F7/F8)
// ---------------------------------------------------------------------------

const (
	whKeyboardLL = 13
	wmKeyDown    = 0x0100
	wmSysKeyDown = 0x0104

	vkF5 = 0x74
	vkF7 = 0x76
	vkF8 = 0x77
)

type kbdllHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

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

var (
	activeHKMu sync.Mutex
	activeHK   *hotkeyListener
)

// Start устанавливает хук и запускает цикл сообщений.
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

		var msg [48]byte
		for {
			ret, _, _ := pGetMessageW.Call(
				uintptr(unsafe.Pointer(&msg[0])),
				0, 0, 0,
			)
			if ret == 0 || ret == ^uintptr(0) {
				log.Printf("hotkey: цикл сообщений завершён (ret=%d)", ret)
				return
			}
		}
	}()

	return <-ready
}

// Stop снимает хук.
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
