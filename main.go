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
	"slices"
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
	logFileName     = "volume_switch.log"

	volumeOverlayDuration = 1200 * time.Millisecond
	volumeOverlayWidth    = float32(260)
	volumeOverlayHeight   = float32(160)
	volumeTickCount       = 10 // ползунок размечен от 0 до 100 с шагом 10 (11 отметок)
)

// Заголовки окон-оверлеев. По ним fixOverlayWindowChrome находит нативные
// окна (FindWindowW), поэтому они должны быть уникальны в системе. Это
// переменные, а не константы, только ради тестов: там заголовки подменяются,
// чтобы тест не задел окна запущенной копии программы.
var (
	windowTitle       = "MPC-HC Progress"
	volumeWindowTitle = "Volume Overlay"
)

var (
	volumeFillColor  = color.NRGBA{R: 90, G: 200, B: 250, A: 255}
	volumeMuteColor  = color.NRGBA{R: 220, G: 70, B: 70, A: 255}
	volumeTrackColor = color.NRGBA{R: 60, G: 60, B: 66, A: 255}
	volumeTickColor  = color.NRGBA{R: 150, G: 150, B: 150, A: 255}
	volumeLabelColor = color.NRGBA{R: 170, G: 170, B: 170, A: 255}
)

// win32Error оборачивает errno, который LazyProc.Call отдаёт третьим
// значением. Он осмыслен только когда сама функция сообщила о неудаче своим
// возвращаемым значением (обычно 0), поэтому вызывать win32Error нужно только
// после такой проверки.
func win32Error(name string, errno error) error {
	if e, ok := errors.AsType[syscall.Errno](errno); ok && e == 0 {
		return fmt.Errorf("%s: неизвестная ошибка", name)
	}
	return fmt.Errorf("%s: %w", name, errno)
}

// Win32-функции, которых нет готовыми в golang.org/x/sys/windows.
var (
	kernel32 = windows.NewLazySystemDLL("kernel32.dll")
	user32   = windows.NewLazySystemDLL("user32.dll")
	shell32  = windows.NewLazySystemDLL("shell32.dll")

	procAllocConsole     = kernel32.NewProc("AllocConsole")
	procFreeConsole      = kernel32.NewProc("FreeConsole")
	procGetConsoleWindow = kernel32.NewProc("GetConsoleWindow")
	procGetModuleHandleW = kernel32.NewProc("GetModuleHandleW")

	procShowWindow          = user32.NewProc("ShowWindow")
	procGetCursorPos        = user32.NewProc("GetCursorPos")
	procMonitorFromPoint    = user32.NewProc("MonitorFromPoint")
	procGetMonitorInfoW     = user32.NewProc("GetMonitorInfoW")
	procGetSystemMetrics    = user32.NewProc("GetSystemMetrics")
	procFindWindowW         = user32.NewProc("FindWindowW")
	procGetWindowLongPtrW   = user32.NewProc("GetWindowLongPtrW")
	procSetWindowLongPtrW   = user32.NewProc("SetWindowLongPtrW")
	procSetWindowPos        = user32.NewProc("SetWindowPos")
	procSetWindowsHookExW   = user32.NewProc("SetWindowsHookExW")
	procUnhookWindowsHookEx = user32.NewProc("UnhookWindowsHookEx")
	procCallNextHookEx      = user32.NewProc("CallNextHookEx")
	procGetMessageW         = user32.NewProc("GetMessageW")
	procPeekMessageW        = user32.NewProc("PeekMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")
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
func ensureConsole() error {
	const swShow = 5

	// GetConsoleWindow не сообщает об ошибках: 0 означает «консоли нет».
	hwnd, _, _ := procGetConsoleWindow.Call()
	if hwnd == 0 {
		if r, _, errno := procAllocConsole.Call(); r == 0 {
			return win32Error("AllocConsole", errno)
		}
		hwnd, _, _ = procGetConsoleWindow.Call()
		if hwnd == 0 {
			return errors.New("GetConsoleWindow: консоль создана, но окна у неё нет")
		}
		out, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
		if err != nil {
			return fmt.Errorf("открытие CONOUT$: %w", err)
		}
		os.Stdout = out
		os.Stderr = out
		log.SetOutput(out)
		in, err := os.OpenFile("CONIN$", os.O_RDONLY, 0)
		if err != nil {
			return fmt.Errorf("открытие CONIN$: %w", err)
		}
		os.Stdin = in
	}
	// ShowWindow возвращает прежнюю видимость окна, а не признак ошибки.
	_, _, _ = procShowWindow.Call(hwnd, swShow)
	return nil
}

// setupLogging настраивает вывод: в консоль (debug) или в файл. Возвращает
// функцию, закрывающую лог-файл; вызывается из main при завершении.
//
// Ошибки, возникшие до того, как у лога появился вывод (консоль, открытие
// файла), пишутся в stderr — его видно при запуске из терминала.
func setupLogging(debug bool) (closeLog func()) {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if debug {
		consoleErr := ensureConsole()
		log.SetOutput(os.Stderr)
		log.Printf("=== volume_switch starting (debug mode) ===")
		if consoleErr != nil {
			log.Printf("console: %v", consoleErr)
		}
		return func() {}
	}
	consoleErr := hideConsole()
	f, err := os.OpenFile(logFileName, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "volume_switch: не удалось открыть %s, лог отключён: %v\n", logFileName, err)
		if consoleErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "volume_switch: console: %v\n", consoleErr)
		}
		log.SetOutput(io.Discard)
		return func() {}
	}
	log.SetOutput(f)
	log.Printf("=== volume_switch starting (log -> %s) ===", logFileName)
	if consoleErr != nil {
		log.Printf("console: %v", consoleErr)
	}
	return func() {
		log.SetOutput(io.Discard)
		if err := f.Close(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "volume_switch: закрытие %s: %v\n", logFileName, err)
		}
	}
}

// hideConsole отвязывает процесс от унаследованной консоли и закрывает её
// окно. Нужно для сборок без `-ldflags "-H=windowsgui"`: загрузчик Windows
// подключает консоль по subsystem в PE-заголовке ещё до того, как этот код
// успевает выполниться, независимо от флага -debug. FreeConsole() убирает
// этот случайный флеш консоли; правильная же сборка с -H=windowsgui вообще
// не создаёт консоль, и этот вызов там становится no-op.
func hideConsole() error {
	r, _, errno := procFreeConsole.Call()
	// Без консоли (сборка с -H=windowsgui) FreeConsole по документации
	// завершается с ERROR_INVALID_PARAMETER — это штатный случай.
	if r == 0 && !errors.Is(errno, windows.ERROR_INVALID_PARAMETER) {
		return win32Error("FreeConsole", errno)
	}
	return nil
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
			if err := os.Setenv(key, value); err != nil {
				log.Printf(".env: не удалось задать %s: %v", key, err)
				continue
			}
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

// mpcField собирает регулярку для поля <p id="...">значение</p> страницы.
func mpcField(id string) *regexp.Regexp {
	return regexp.MustCompile(`<p id="` + id + `">([^<]*)</p>`)
}

var (
	rePosition    = mpcField("position")
	reDuration    = mpcField("duration")
	rePositionStr = mpcField("positionstring")
	reStateString = mpcField("statestring")
	reFile        = mpcField("file")
	reFilePath    = mpcField("filepath")
)

type MPCState struct {
	Position    int64
	Duration    int64
	PositionStr string
	StateString string
	File        string
	FilePath    string
}

// extractInt возвращает 0, если поля в странице нет или оно пустое (MPC-HC
// без открытого файла), и ошибку, если число в поле не разбирается.
func extractInt(html string, re *regexp.Regexp) (int64, error) {
	s := strings.TrimSpace(extractStr(html, re))
	if s == "" {
		return 0, nil
	}
	return strconv.ParseInt(s, 10, 64)
}

func extractStr(html string, re *regexp.Regexp) string {
	m := re.FindStringSubmatch(html)
	if len(m) < 2 {
		return ""
	}
	return m[1]
}

func fetchState(url string, client *http.Client) (_ *MPCState, err error) {
	resp, err := client.Get(url)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("закрытие ответа: %w", cerr)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	html := string(body)

	position, err := extractInt(html, rePosition)
	if err != nil {
		return nil, fmt.Errorf("position: %w", err)
	}
	duration, err := extractInt(html, reDuration)
	if err != nil {
		return nil, fmt.Errorf("duration: %w", err)
	}

	return &MPCState{
		Position:    position,
		Duration:    duration,
		PositionStr: extractStr(html, rePositionStr),
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

type winMonitorInfo struct {
	CbSize    uint32
	RcMonitor windows.Rect
	RcWork    windows.Rect
	DwFlags   uint32
}

// screenLayout возвращает рабочую область (без панели задач) и полные границы
// монитора, на котором сейчас находится курсор мыши.
func screenLayout() (work, monitor windows.Rect, err error) {
	const monitorDefaultToNearest = 2

	var pt struct{ X, Y int32 } // POINT
	if r, _, errno := procGetCursorPos.Call(uintptr(unsafe.Pointer(&pt))); r == 0 {
		return work, monitor, win32Error("GetCursorPos", errno)
	}

	ptPacked := uintptr(uint32(pt.X)) | uintptr(uint32(pt.Y))<<32
	// С MONITOR_DEFAULTTONEAREST MonitorFromPoint всегда возвращает монитор
	// и ошибок не сообщает.
	hMonitor, _, _ := procMonitorFromPoint.Call(ptPacked, monitorDefaultToNearest)

	var mi winMonitorInfo
	mi.CbSize = uint32(unsafe.Sizeof(mi))
	if r, _, errno := procGetMonitorInfoW.Call(hMonitor, uintptr(unsafe.Pointer(&mi))); r == 0 {
		return work, monitor, win32Error("GetMonitorInfoW", errno)
	}

	return mi.RcWork, mi.RcMonitor, nil
}

// titleBarHeight возвращает высоту заголовка окна Windows (вместе с рамкой).
func titleBarHeight() int32 {
	const (
		smCyCaption      = 4
		smCySizeFrame    = 33
		smCxPaddedBorder = 92
	)

	// GetSystemMetrics не сообщает об ошибках (GetLastError не выставляет),
	// при неудаче возвращает 0 — это даёт лишь чуть меньший отступ.
	var total int32
	for _, metric := range []uintptr{smCyCaption, smCySizeFrame, smCxPaddedBorder} {
		v, _, _ := procGetSystemMetrics.Call(metric)
		total += int32(v)
	}
	return total
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

// execValue is exec for operations that return a value.
func execValue[T any](s *Switcher, f func() (T, error)) (T, error) {
	var v T
	err := s.exec(func() error {
		var err error
		v, err = f()
		return err
	})
	return v, err
}

// Close stops the worker goroutine and releases every COM object.
func (s *Switcher) Close() {
	close(s.quit)
}

// ListActiveDevices returns every active playback endpoint.
func (s *Switcher) ListActiveDevices() ([]Device, error) {
	return execValue(s, s.listActiveDevicesLocked)
}

// DefaultDeviceID returns the ID of the current default playback endpoint
// (console role).
func (s *Switcher) DefaultDeviceID() (string, error) {
	return execValue(s, s.defaultDeviceIDLocked)
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
	return execValue(s, func() (Device, error) {
		devices, err := s.listActiveDevicesLocked()
		if err != nil {
			return Device{}, err
		}
		if len(devices) == 0 {
			return Device{}, errors.New("no active playback devices")
		}

		currentID, err := s.defaultDeviceIDLocked()
		if err != nil {
			return Device{}, err
		}

		// Если текущего устройства нет в списке (IndexFunc вернул -1),
		// отсчёт идёт от первого.
		idx := max(slices.IndexFunc(devices, func(d Device) bool { return d.ID == currentID }), 0)
		next := devices[wrapIndex(idx+direction, len(devices))]
		return next, s.setDefaultLocked(next.ID)
	})
}

// wrapIndex приводит i к диапазону [0, n) по кругу, в том числе для
// отрицательных i (оператор % в Go сохраняет знак делимого).
func wrapIndex(i, n int) int {
	return (i%n + n) % n
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
	return deviceID(dev)
}

func (s *Switcher) setDefaultLocked(id string) error {
	for _, role := range []wca.ERole{wca.EConsole, wca.EMultimedia, wca.ECommunications} {
		if err := s.policy.SetDefaultEndpoint(id, role); err != nil {
			return err
		}
	}
	return nil
}

// volumeReading — громкость текущего default-устройства воспроизведения.
// DeviceID нужен поллеру (см. volumeChanged), чтобы отличить реальное
// изменение громкости от переключения на другое устройство с другим
// сохранённым уровнем — это два разных события, и оверлей поднимается только
// для первого.
type volumeReading struct {
	Level    float64 // 0..1
	Muted    bool
	DeviceID string
}

// CurrentVolume возвращает громкость текущего default-устройства.
func (s *Switcher) CurrentVolume() (volumeReading, error) {
	return execValue(s, s.currentVolumeLocked)
}

func (s *Switcher) currentVolumeLocked() (volumeReading, error) {
	var dev *wca.IMMDevice
	if err := s.mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &dev); err != nil {
		return volumeReading{}, err
	}
	defer dev.Release()

	id, err := deviceID(dev)
	if err != nil {
		return volumeReading{}, err
	}

	var aev *wca.IAudioEndpointVolume
	if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
		return volumeReading{}, err
	}
	defer aev.Release()

	var scalar float32
	if err := aev.GetMasterVolumeLevelScalar(&scalar); err != nil {
		return volumeReading{}, err
	}
	var muted bool
	if err := aev.GetMute(&muted); err != nil {
		return volumeReading{}, err
	}
	return volumeReading{Level: float64(scalar), Muted: muted, DeviceID: id}, nil
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

// pollVolume на каждый тик читает громкость через read и вызывает onChange,
// когда она изменилась (см. volumeChanged). Работает, пока не закрыт ticks.
func pollVolume(read func() (volumeReading, error), onChange func(level float64, muted bool), ticks <-chan time.Time) {
	var prev volumeReading
	for range ticks {
		cur, err := read()
		if err != nil {
			continue
		}
		if volumeChanged(prev, cur) {
			onChange(cur.Level, cur.Muted)
		} else if prev.DeviceID != "" && prev.DeviceID != cur.DeviceID {
			log.Printf("volume poller: устройство сменилось, громкость не показываю")
		}
		prev = cur
	}
}

// volumeChanged сообщает, что между двумя замерами на ОДНОМ И ТОМ ЖЕ
// устройстве изменился уровень (с точностью до процента) или mute. Первый
// замер (prev пустой — у реального устройства ID не бывает пустым), как и
// замер сразу после смены default-устройства (F7/F8, трей, панель задач
// Windows), изменением не считается: иначе оверлей всплывал бы при каждом
// запуске и при каждом переключении вывода (у устройств обычно разный
// сохранённый уровень громкости, и это не то же самое, что пользователь
// покрутил громкость).
func volumeChanged(prev, cur volumeReading) bool {
	return prev.DeviceID != "" && prev.DeviceID == cur.DeviceID &&
		(math.Round(prev.Level*100) != math.Round(cur.Level*100) || prev.Muted != cur.Muted)
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

	trayIconID = 100
)

// systrayClassName — класс скрытого окна трея; переменная только ради тестов.
var systrayClassName = "SystrayClass"

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

// showNotification displays a balloon notification with the given title and
// body. It is a no-op if this process's tray window cannot be found (e.g.
// called before systray finished initializing, or after a dependency
// upgrade changed the implementation details above).
func showNotification(title, body string) {
	hwnd, err := findOwnTrayWindow()
	if err != nil {
		log.Printf("notification: поиск окна трея: %v", err)
		return
	}
	if hwnd == 0 {
		log.Printf("notification: окно трея %q не найдено, уведомление не показано", systrayClassName)
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

	if r, _, errno := procShellNotifyIconW.Call(uintptr(nimModify), uintptr(unsafe.Pointer(&nid))); r == 0 {
		log.Printf("notification: %v", win32Error("Shell_NotifyIconW", errno))
	}
}

// copyUTF16 копирует s в нулевой буфер dst, при необходимости обрезая;
// последний элемент dst не трогается и остаётся завершающим нулём.
func copyUTF16(dst []uint16, s string) {
	src, err := windows.UTF16FromString(s)
	if err != nil {
		return
	}
	copy(dst[:len(dst)-1], src)
}

// findOwnTrayWindow enumerates top-level windows looking for the hidden
// systray window that belongs to this process.
//
// Ошибки по отдельным окнам (окно успело закрыться во время перебора и т.п.)
// не прерывают поиск — такое окно просто пропускается.
func findOwnTrayWindow() (windows.Handle, error) {
	pid := windows.GetCurrentProcessId()
	var found windows.Handle

	cb := windows.NewCallback(func(hwnd windows.HWND, _ uintptr) uintptr {
		var winPid uint32
		if _, err := windows.GetWindowThreadProcessId(hwnd, &winPid); err != nil || winPid != pid {
			return 1 // окно чужое или уже недействительно — продолжаем перебор
		}

		var cls [64]uint16
		n, err := windows.GetClassName(hwnd, &cls[0], int32(len(cls)))
		if err != nil {
			return 1 // имя класса не получено — пропускаем
		}
		if windows.UTF16ToString(cls[:n]) == systrayClassName {
			found = windows.Handle(hwnd)
			return 0 // stop enumeration
		}
		return 1
	})

	// EnumWindows сообщает об ошибке и тогда, когда перебор остановил сам
	// колбэк (окно найдено), поэтому ошибкой это считается только без находки.
	if err := windows.EnumWindows(cb, nil); err != nil && found == 0 {
		return 0, fmt.Errorf("EnumWindows: %w", err)
	}
	return found, nil
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
	for y := range size {
		for x := range size {
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
	const headerSize = 6 + 16
	le := binary.LittleEndian
	b := make([]byte, 0, headerSize+len(pngData))

	// ICONDIR
	b = le.AppendUint16(b, 0) // reserved
	b = le.AppendUint16(b, 1) // type: icon
	b = le.AppendUint16(b, 1) // image count

	// ICONDIRENTRY (dimensions >=256 are encoded as 0 per spec; not needed here)
	b = append(b, byte(w), byte(h), 0, 0)        // width, height, color palette, reserved
	b = le.AppendUint16(b, 1)                    // color planes
	b = le.AppendUint16(b, 32)                   // bits per pixel
	b = le.AppendUint32(b, uint32(len(pngData))) // size of image data
	b = le.AppendUint32(b, headerSize)           // offset to image data

	return append(b, pngData...)
}

// ---------------------------------------------------------------------------
// Fyne-окно (встроенный mpcprogress)
// ---------------------------------------------------------------------------

var (
	fyneApp    fyne.App
	mainWindow fyne.Window

	// winVisible читается и меняется только внутри fyne.Do, т.е. всегда в
	// одном UI-потоке, поэтому мьютекс не нужен.
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

// clickCatcher — прозрачный виджет на весь оверлей, который ловит клики по
// нему:
//   - средняя кнопка (колесо) — открывает папку с текущим файлом в проводнике;
//   - левая кнопка по полосе прогресса — ставит воспроизведение на паузу или
//     возобновляет его (команда 889 «Play/Pause» web-интерфейса MPC-HC); если
//     web-интерфейс недоступен (MPC-HC не запущен) — запускает MPC-HC.
//
// Правая кнопка не перехватывается вовсе. URL variables.html передаётся в
// конструктор, чтобы клик мог отправить команду в тот же web-интерфейс
// MPC-HC, который опрашивает оверлей.
type clickCatcher struct {
	widget.BaseWidget

	mpcURL string
}

func newClickCatcher(mpcURL string) *clickCatcher {
	c := &clickCatcher{mpcURL: mpcURL}
	c.ExtendBaseWidget(c)
	return c
}

func (c *clickCatcher) CreateRenderer() fyne.WidgetRenderer {
	return widget.NewSimpleRenderer(canvas.NewRectangle(color.Transparent))
}

func (c *clickCatcher) MouseDown(*desktop.MouseEvent) {}

func (c *clickCatcher) MouseUp(ev *desktop.MouseEvent) {
	switch ev.Button {
	case desktop.MouseButtonTertiary:
		log.Printf("overlay: клик колесом, открываю папку с файлом")
		openContainingFolder(currentFilePath)
	case desktop.MouseButtonPrimary:
		if !pointInProgressBar(ev.Position) {
			return
		}
		log.Printf("overlay: клик левой кнопкой по полосе прогресса — переключаю воспроизведение")
		// Запрос уходит из отдельной горутины: если MPC-HC не отвечает,
		// HTTP-клиент ждёт до своего таймаута, и держать на этом событийный
		// цикл Fyne нельзя.
		go toggleMPCPlayback(c.mpcURL)
	}
}

// pointInProgressBar сообщает, попала ли точка клика (в координатах окна) в
// границы полосы прогресса. Полоса занимает почти всю ширину оверлея, но
// клик по подписям времени или по пустым полям окна воспроизведение не
// переключает.
func pointInProgressBar(p fyne.Position) bool {
	if progressBar == nil || fyne.CurrentApp() == nil {
		return false
	}
	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(progressBar)
	size := progressBar.Size()
	return p.X >= pos.X && p.X <= pos.X+size.Width &&
		p.Y >= pos.Y && p.Y <= pos.Y+size.Height
}

// mpcPlayPauseCommand — WM_COMMAND ID пункта «Play/Pause» в MPC-HC
// (ID_PLAY_PLAYPAUSE). Соседние команды: 887 Play, 888 Pause, 890 Stop — те
// же номера принимает web-интерфейс в command.html.
const mpcPlayPauseCommand = 889

// mpcCommandURL строит URL команды web-интерфейса MPC-HC из URL
// variables.html: берётся тот же каталог, файл заменяется на command.html, а
// номер команды передаётся параметром wm_command.
//
//	http://localhost:7777/variables.html -> http://localhost:7777/command.html?wm_command=889
func mpcCommandURL(variablesURL string, command int) string {
	base := strings.TrimSpace(variablesURL)
	if base == "" {
		return ""
	}
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	if i := strings.LastIndex(base, "/"); i >= 0 {
		base = base[:i+1]
	} else {
		base += "/"
	}
	return fmt.Sprintf("%scommand.html?wm_command=%d", base, command)
}

// toggleMPCPlayback отправляет в web-интерфейс MPC-HC команду «Play/Pause»,
// то есть ставит воспроизведение на паузу либо возобновляет его — в
// зависимости от текущего состояния плеера. Состояние заранее не
// запрашивается: команда сама по себе переключающая, а лишний опрос добавил
// бы только гонку между чтением состояния и его изменением.
func toggleMPCPlayback(variablesURL string) {
	cmdURL := mpcCommandURL(variablesURL, mpcPlayPauseCommand)
	if cmdURL == "" {
		log.Printf("mpc command: URL variables.html неизвестен, команда не отправлена")
		return
	}

	client := &http.Client{Timeout: 2 * time.Second}
	resp, err := client.Get(cmdURL)
	if err != nil {
		log.Printf("mpc command: не удалось отправить %s: %v — запускаю MPC-HC", cmdURL, err)
		launchMPC()
		return
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			log.Printf("mpc command: закрытие ответа: %v", err)
		}
	}()
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		log.Printf("mpc command: чтение ответа: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		log.Printf("mpc command: play/pause -> %s отклонена: %s", cmdURL, resp.Status)
		return
	}
	log.Printf("mpc command: play/pause -> %s (%s)", cmdURL, resp.Status)
}

// Где искать MPC-HC (см. mpcExePath). Переменные, а не константы, только ради
// тестов.
var (
	defaultMPCExe   = `C:\Program Files (x86)\K-Lite Codec Pack\MPC-HC64\mpc-hc64.exe`
	mpcRegistryPath = `Software\MPC-HC\MPC-HC`
)

// mpcExePath возвращает путь к MPC-HC: MPC_EXE из окружения/.env; иначе
// defaultMPCExe, если такой файл есть; иначе путь, который MPC-HC сам пишет в
// реестр при каждом запуске (HKCU\Software\MPC-HC\MPC-HC\ExePath).
func mpcExePath() string {
	if p := os.Getenv("MPC_EXE"); p != "" {
		return p
	}
	_, err := os.Stat(defaultMPCExe)
	if err == nil {
		return defaultMPCExe
	}
	if !errors.Is(err, os.ErrNotExist) {
		log.Printf("mpc launch: проверка %s: %v", defaultMPCExe, err)
	}
	if p := mpcExeFromRegistry(); p != "" {
		return p
	}
	return defaultMPCExe
}

// mpcExeFromRegistry читает HKCU\Software\MPC-HC\MPC-HC\ExePath; "" — если
// значения нет или его не удалось прочитать.
func mpcExeFromRegistry() string {
	key, err := registry.OpenKey(registry.CURRENT_USER, mpcRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		if !errors.Is(err, registry.ErrNotExist) {
			log.Printf("mpc launch: открытие ключа реестра MPC-HC: %v", err)
		}
		return ""
	}
	defer logRegistryClose(key, "MPC-HC")
	p, _, err := key.GetStringValue("ExePath")
	if err != nil && !errors.Is(err, registry.ErrNotExist) {
		log.Printf("mpc launch: чтение ExePath из реестра: %v", err)
	}
	return p
}

// launchMPC запускает MPC-HC. Повторный запуск при уже открытом плеере
// безвреден: MPC-HC по умолчанию работает в режиме одного экземпляра и просто
// выводит своё окно на передний план.
func launchMPC() {
	exe := mpcExePath()
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	if err := startDetached(cmd); err != nil {
		log.Printf("mpc launch: не удалось запустить %s: %v", exe, err)
		return
	}
	log.Printf("mpc launch: запущен %s", exe)
}

// startDetached запускает процесс и не ждёт его завершения: дескриптор
// процесса сразу освобождается. Переменная, чтобы тесты не запускали
// настоящие explorer и MPC-HC.
var startDetached = func(cmd *exec.Cmd) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
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
	if err := startDetached(cmd); err != nil {
		log.Printf("overlay: не удалось открыть проводник: %v", err)
	}
}

// runFyne запускает Fyne-приложение. Вызывается в отдельной горутине.
// Блокируется до закрытия приложения (обычно — до systray.Quit()).
func runFyne(url string, interval time.Duration) {
	runtime.LockOSThread()
	log.Printf("fyne: инициализация (url=%s, interval=%s)", url, interval)

	fyneApp = app.New()
	buildUI(url)
	go pollMPC(url, time.Tick(interval))

	fyneApp.Run()
	log.Printf("fyne: цикл приложения завершён")
}

// buildUI создаёт в fyneApp оба окна-оверлея — прогресс MPC-HC (mainWindow)
// и ползунок громкости (volumeWindow) — и оставляет их скрытыми.
func buildUI(url string) {
	fyneApp.Settings().SetTheme(theme.DarkTheme())
	mainWindow = fyneApp.NewWindow(windowTitle)

	// --- Размер/позиция окна под монитор с курсором ---
	windowWidth, windowHeight := float32(900), float32(170)
	posX, posY := 0, 0
	if work, monitor, err := screenLayout(); err != nil {
		log.Printf("fyne: не удалось определить монитор, размер по умолчанию: %v", err)
	} else if work.Right > work.Left {
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
	requestPosition(mainWindow, posX, posY)

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
	mainWindow.SetContent(container.NewStack(body, newClickCatcher(url)))

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

	// Нативный HWND у Fyne/GLFW создаётся лениво, только при первом Show().
	// Если применить WS_EX_TOOLWINDOW уже ПОСЛЕ того, как окно хотя бы раз
	// показалось с обычным стилем, Explorer иногда всё равно продолжает
	// держать кнопку в панели задач (подтверждено на практике: однократный
	// fixOverlayWindowChrome после первого Show() отрабатывал успешно, но
	// кнопка оставалась) — похоже, таскбар кеширует факт появления окна с
	// «обычным» стилем. Поэтому здесь окно показывается и сразу прячется
	// один раз вхолостую ещё до первого реального показа — только чтобы
	// форсировать создание HWND, применить стиль, и лишь после этого окно
	// хоть раз станет видимым пользователю. Касается обоих окон: mainWindow
	// без этого оставалось в панели задач после первого показа по F5.
	//
	// После этого оба окна стартуют скрытыми: mainWindow покажем по F5,
	// volumeWindow — по событию от volume poller.
	for _, w := range []fyne.Window{mainWindow, volumeWindow} {
		showOverlay(w, w == mainWindow)
		w.Hide()
	}
	log.Printf("fyne: окно создано, стартует скрытым")
}

// pollMPC на каждый тик опрашивает web-интерфейс MPC-HC и обновляет оверлей
// прогресса. Работает, пока не закрыт ticks.
func pollMPC(url string, ticks <-chan time.Time) {
	client := &http.Client{Timeout: 2 * time.Second}
	var lastErr string
	for range ticks {
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
		fyne.Do(func() { showProgress(st) })
	}
}

// showProgress выводит состояние MPC-HC на оверлей. Вызывается только из
// UI-потока (через fyne.Do).
func showProgress(st *MPCState) {
	currentFilePath = st.FilePath
	if st.Duration <= 0 {
		progressText = "Нет активного воспроизведения"
		progressBar.SetValue(0)
		elapsedLabel.SetText("00:00")
		remainingLabel.SetText("00:00")
		return
	}
	progress := min(max(float64(st.Position)/float64(st.Duration), 0), 1)

	elapsed := st.PositionStr
	if elapsed == "" {
		elapsed = formatTime(st.Position)
	}
	elapsedLabel.SetText(elapsed)
	remainingLabel.SetText("-" + formatTime(st.Duration-st.Position))
	progressText = fmt.Sprintf("%s   —   %.1f%%  %s  (из %s)",
		st.File, progress*100, st.StateString, formatTime(st.Duration))
	progressBar.SetValue(progress)
}

// requestPosition ставит окно в точку (x, y) экрана.
func requestPosition(w fyne.Window, x, y int) {
	dw, ok := w.(desktop.Window)
	if !ok {
		return
	}
	// Fyne игнорирует RequestPosition(0, 0), считая это «позиция не задана».
	if x == 0 && y == 0 {
		x = 1
	}
	dw.RequestPosition(x, y)
}

// showOverlay показывает окно и сразу снимает его с панели задач (см.
// fixOverlayWindowChrome). С topmost окно ещё и поднимается поверх всех окон.
func showOverlay(w fyne.Window, topmost bool) {
	w.Show()
	if err := fixOverlayWindowChrome(w.Title(), topmost); err != nil {
		log.Printf("overlay-chrome: %v", err)
	}
}

// fixOverlayWindowChrome снимает нативное окно с заданным заголовком с панели
// задач (WS_EX_TOOLWINDOW — приложение представлено только иконкой в трее) и
// перекрашивает его системный заголовок в тёмный через DWM, чтобы белая
// полоса сверху не выбивалась из тёмной темы Fyne. Рамка/заголовок при этом
// остаются — попытка убрать их совсем (GWL_STYLE) приводила к тому, что GLFW
// пересчитывал размер окна по устаревшим метрикам бывшего заголовка и
// раздувал окно заново при каждом изменении; DWM-атрибут такой проблемы не
// создаёт, т.к. не трогает геометрию.
//
// Применяется при КАЖДОМ показе окна, а не один раз за процесс: для
// volumeWindow, которое показывается/прячется очень часто (на каждое
// изменение громкости), один разовый фикс не держится — после очередного
// цикла Hide()+Show() Explorer иногда всё равно возвращает кнопку в панель
// задач. Сами вызовы дешёвые (несколько syscall), так что переприменять их
// при каждом показе не проблема.
//
// С topmost окно становится «поверх всех» (HWND_TOPMOST) и поднимается на
// самый верх среди таких окон. Это тоже делается при каждом показе, а не
// один раз через RequestAlwaysOnTop Fyne: иначе окно, ставшее «поверх
// всех» позже (например, MPC-HC в режиме «Поверх всех окон»), осталось бы
// над оверлеем.
func fixOverlayWindowChrome(title string, topmost bool) error {
	const (
		wsExToolWindow  = 0x00000080
		wsExAppWindow   = 0x00040000
		swpNoMove       = 0x0002
		swpNoSize       = 0x0001
		swpNoZOrder     = 0x0004
		swpNoActivate   = 0x0010
		swpFrameChanged = 0x0020

		dwmwaUseImmersiveDarkMode = 20

		hwndTopmost = ^uintptr(0) // HWND_TOPMOST = (HWND)-1
	)
	// -20 переполняет uintptr как константа компиляции; через typed
	// переменную Go делает корректное sign-extension в рантайме.
	var gwlExStyle int32 = -20

	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return fmt.Errorf("заголовок %q: %w", title, err)
	}
	hwnd, _, errno := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	if hwnd == 0 {
		return fmt.Errorf("окно %q: %w", title, win32Error("FindWindowW", errno))
	}

	// У окна GLFW расширенный стиль никогда не нулевой, поэтому 0 от
	// Get/SetWindowLongPtrW здесь однозначно означает ошибку.
	exStyle, _, errno := procGetWindowLongPtrW.Call(hwnd, uintptr(gwlExStyle))
	if exStyle == 0 {
		return win32Error("GetWindowLongPtrW", errno)
	}
	newExStyle := (exStyle &^ uintptr(wsExAppWindow)) | uintptr(wsExToolWindow)
	if prev, _, errno := procSetWindowLongPtrW.Call(hwnd, uintptr(gwlExStyle), newExStyle); prev == 0 {
		return win32Error("SetWindowLongPtrW", errno)
	}

	// SWP_FRAMECHANGED заставляет Explorer пересчитать кнопку в панели
	// задач без скрытия/показа окна — в отличие от ShowWindow(HIDE)+
	// ShowWindow(SHOW), это не ломает перерисовку содержимого GLFW/Fyne.
	// Z-порядок меняется только для topmost, фокус окно не забирает.
	var insertAfter uintptr
	flags := uintptr(swpNoMove | swpNoSize | swpNoActivate | swpFrameChanged)
	if topmost {
		insertAfter = hwndTopmost
	} else {
		flags |= swpNoZOrder
	}
	if r, _, errno := procSetWindowPos.Call(hwnd, insertAfter, 0, 0, 0, 0, flags); r == 0 {
		return win32Error("SetWindowPos", errno)
	}

	darkMode := int32(1)
	if err := windows.DwmSetWindowAttribute(windows.HWND(hwnd), dwmwaUseImmersiveDarkMode,
		unsafe.Pointer(&darkMode), uint32(unsafe.Sizeof(darkMode))); err != nil {
		return fmt.Errorf("DwmSetWindowAttribute: %w", err)
	}
	return nil
}

// toggleMPCWindow показывает окно, если оно скрыто, и прячет, если показано.
func toggleMPCWindow() {
	fyne.Do(func() {
		if mainWindow == nil {
			log.Printf("F5: окно ещё не создано")
			return
		}
		if winVisible {
			mainWindow.Hide()
			log.Printf("F5: окно скрыто")
		} else {
			showOverlay(mainWindow, true)
			log.Printf("F5: окно показано поверх всех окон")
		}
		winVisible = !winVisible
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

	clamped := min(max(level, 0), 1)
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
		if work, _, err := screenLayout(); err != nil {
			log.Printf("volume overlay: не удалось определить монитор, размер по умолчанию: %v", err)
		} else if work.Right > work.Left {
			width = float32(work.Right-work.Left) / 2
			height = float32(work.Bottom-work.Top) / 2
			x = int(work.Left)
			y = int(work.Bottom) - int(height)
		}

		layoutVolumeOverlay(width, height, level, muted)
		volumeWindow.Resize(fyne.NewSize(width, height))
		requestPosition(volumeWindow, x, y)
		log.Printf("volume overlay: показываю размер=%.0fx%.0f позиция=(%d,%d) level=%.0f%% muted=%v",
			width, height, x, y, level*100, muted)

		showOverlay(volumeWindow, false)
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

// ---------------------------------------------------------------------------
// Автозапуск через реестр (HKCU\...\Run)
// ---------------------------------------------------------------------------

const autostartValueName = "VolumeSwitch"

// autostartRegistryPath — ключ автозапуска в HKCU; переменная только ради
// тестов, чтобы они не трогали настоящий Run.
var autostartRegistryPath = `Software\Microsoft\Windows\CurrentVersion\Run`

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

// logRegistryClose закрывает ключ реестра, открытый только на чтение, и
// логирует ошибку закрытия; предназначена для defer.
func logRegistryClose(key registry.Key, name string) {
	if err := key.Close(); err != nil {
		log.Printf("registry: закрытие ключа %s: %v", name, err)
	}
}

// isAutostartEnabled проверяет, что значение в HKCU\...\Run существует и
// указывает именно на текущий exe (а не на устаревший путь).
func isAutostartEnabled() bool {
	cmd, err := autostartCommand()
	if err != nil {
		log.Printf("autostart: путь к exe: %v", err)
		return false
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryPath, registry.QUERY_VALUE)
	if err != nil {
		log.Printf("autostart: открытие ключа Run: %v", err)
		return false
	}
	defer logRegistryClose(key, "Run")
	val, _, err := key.GetStringValue(autostartValueName)
	if err != nil {
		if !errors.Is(err, registry.ErrNotExist) {
			log.Printf("autostart: чтение %s: %v", autostartValueName, err)
		}
		return false
	}
	return val == cmd
}

// setAutostart включает или выключает автозапуск, записывая/удаляя значение
// в HKCU\...\Run.
func setAutostart(enable bool) (err error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, autostartRegistryPath, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := key.Close(); cerr != nil {
			err = errors.Join(err, fmt.Errorf("закрытие ключа Run: %w", cerr))
		}
	}()

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
	switcher    deviceSwitcher
	stopHotkeys func() error

	menuMu      sync.Mutex
	deviceItems map[string]checkbox // пункты меню по ID устройства
)

// deviceSwitcher — то, что меню трея и хоткеи используют от *Switcher;
// интерфейс нужен, чтобы их можно было проверить без COM и аудиоустройств.
type deviceSwitcher interface {
	SetDefault(id string) error
	Cycle(direction int) (Device, error)
	Close()
}

// checkbox — пункт меню с галочкой (*systray.MenuItem); интерфейс нужен,
// чтобы меню можно было проверить без настоящего трея.
type checkbox interface {
	Check()
	Uncheck()
}

// Вызовы systray, нужные вне onReady. Переменные только ради тестов: без
// запущенного трея systray падает (nil-указатель внутри SetTooltip).
var (
	setTrayTooltip  = systray.SetTooltip
	addTrayCheckbox = func(title, tooltip string, checked bool) (checkbox, <-chan struct{}) {
		item := systray.AddMenuItemCheckbox(title, tooltip, checked)
		return item, item.ClickedCh
	}
)

// singleInstanceMutex — имя именованного мьютекса, по которому второй
// экземпляр узнаёт, что программа уже запущена. Префикс Local\ ограничивает
// проверку сеансом текущего пользователя: глобальные хоткеи и трей всё равно
// работают только в своём сеансе. Переменная только ради тестов.
var singleInstanceMutex = `Local\volume_switch_single_instance`

// acquireSingleInstance создаёт именованный мьютекс. Возвращает already=true,
// если мьютекс уже существует, т.е. другой экземпляр программы запущен.
// Возвращённую функцию release нужно вызвать при завершении; до этого
// мьютекс держится открытым и блокирует повторный запуск. Windows закрывает
// дескриптор и при аварийном завершении процесса, так что «зависшей»
// блокировки после падения не остаётся.
func acquireSingleInstance() (release func(), already bool, err error) {
	name, err := windows.UTF16PtrFromString(singleInstanceMutex)
	if err != nil {
		return nil, false, fmt.Errorf("имя мьютекса: %w", err)
	}
	h, err := windows.CreateMutex(nil, false, name)
	if h == 0 {
		return nil, false, fmt.Errorf("CreateMutex: %w", err)
	}
	release = func() {
		if err := windows.CloseHandle(h); err != nil {
			log.Printf("single instance: закрытие мьютекса: %v", err)
		}
	}
	if errors.Is(err, windows.ERROR_ALREADY_EXISTS) {
		release()
		return nil, true, nil
	}
	return release, false, nil
}

func main() {
	debug := flag.Bool("debug", false, "Показывать консоль с дебаг-логами")
	urlFlag := flag.String("url", "", "URL variables.html (перекрывает MPC_URL)")
	intervalFlag := flag.Duration("interval", 0, "Интервал опроса (перекрывает MPC_INTERVAL)")
	flag.Parse()

	closeLog := setupLogging(*debug)
	defer closeLog()

	releaseInstance, already, err := acquireSingleInstance()
	switch {
	case err != nil:
		// Проверку выполнить не удалось — лучше запуститься, чем не работать вовсе.
		log.Printf("single instance: %v — запускаюсь без проверки", err)
	case already:
		log.Printf("single instance: программа уже запущена, выхожу")
		return
	default:
		defer releaseInstance()
	}

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
		systray.Run(func() { onReady(*debug) }, onExit)
		log.Printf("systray: завершён, выход")
	}()

	runFyne(url, interval)
}

func onReady(debug bool) {
	log.Printf("systray: onReady")
	if debug {
		// systray мог спрятать консоль повторно
		if err := ensureConsole(); err != nil {
			log.Printf("console: %v", err)
		}
	}
	systray.SetIcon(trayIconData)
	systray.SetTitle("")
	systray.SetTooltip("Volume Switch")

	// В глобальный switcher (интерфейс) попадает только успешно созданный
	// *Switcher: nil-указатель в интерфейсе прошёл бы проверку на nil в onExit.
	sw, err := NewSwitcher()
	if err != nil {
		log.Printf("audio: ошибка инициализации: %v", err)
		systray.SetTooltip("Volume Switch: audio init failed")
		return
	}
	switcher = sw
	log.Printf("audio: switcher создан")

	devices, err := sw.ListActiveDevices()
	if err != nil {
		log.Printf("audio: не удалось получить список устройств: %v", err)
	}
	log.Printf("audio: найдено активных устройств: %d", len(devices))
	for i, d := range devices {
		log.Printf("  [%d] id=%s name=%q", i, d.ID, d.Name)
	}

	currentID, err := sw.DefaultDeviceID()
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

	stopHotkeys, err = startHotkeys(map[uint32]func(){
		vkF5: func() { log.Printf("hotkey: F5"); toggleMPCWindow() },
		vkF7: func() { log.Printf("hotkey: F7"); onCycle(-1) },
		vkF8: func() { log.Printf("hotkey: F8"); onCycle(1) },
	})
	if err != nil {
		log.Printf("hotkey: не удалось установить хук: %v", err)
	} else {
		log.Printf("hotkey: хук установлен (F5/F7/F8)")
	}

	// Колбэк вызывается из горутины поллера; вся работа с UI внутри
	// showVolumeOverlay идёт через fyne.Do.
	go pollVolume(sw.CurrentVolume, showVolumeOverlay, time.Tick(volumePollInterval))
}

func onExit() {
	log.Printf("onExit: остановка")
	if stopHotkeys != nil {
		if err := stopHotkeys(); err != nil {
			log.Printf("onExit: не удалось снять хук: %v", err)
		} else {
			log.Printf("onExit: хук снят")
		}
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

	deviceItems = make(map[string]checkbox, len(devices))
	for _, dev := range devices {
		item, clicked := addTrayCheckbox(dev.Name, "Сделать устройством по умолчанию", dev.ID == currentID)
		deviceItems[dev.ID] = item

		go func() {
			for range clicked {
				log.Printf("tray: выбран %q", dev.Name)
				selectDevice(dev)
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
	setTrayTooltip("Volume Switch: " + dev.Name)
	showNotification("Аудиовыход переключён", dev.Name)
}

func setChecked(checkedID string) {
	menuMu.Lock()
	defer menuMu.Unlock()

	for id, item := range deviceItems {
		if id == checkedID {
			item.Check()
		} else {
			item.Uncheck()
		}
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

// hotkeys — обработчики перехватываемых клавиш по virtual-key коду.
// Заполняется в startHotkeys до установки хука и дальше не меняется, поэтому
// hookProc читает её без блокировок.
var hotkeys map[uint32]func()

// startHotkeys устанавливает глобальный хук клавиатуры WH_KEYBOARD_LL и
// возвращает функцию, которая его снимает. Хук живёт в отдельном закреплённом
// OS-потоке со своим циклом сообщений: без цикла сообщений в потоке, который
// поставил хук, Windows не вызывает hookProc.
func startHotkeys(handlers map[uint32]func()) (stop func() error, err error) {
	hInst, _, errno := procGetModuleHandleW.Call(0)
	if hInst == 0 {
		return nil, win32Error("GetModuleHandleW", errno)
	}
	hotkeys = handlers

	type result struct {
		hook     uintptr
		threadID uint32
		err      error
	}
	ready := make(chan result, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// PeekMessage создаёт очередь сообщений потока заранее: иначе stop,
		// вызванный сразу после установки хука, не смог бы отправить ей
		// WM_QUIT (PostThreadMessage требует существующей очереди).
		const pmNoRemove = 0
		var msg [48]byte
		procPeekMessageW.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0, pmNoRemove)

		// Колбэки syscall.NewCallback никогда не освобождаются, так что
		// хранить ссылку на cb, чтобы его не собрал GC, не нужно.
		cb := syscall.NewCallback(hookProc)
		hook, _, errno := procSetWindowsHookExW.Call(whKeyboardLL, cb, hInst, 0)
		if hook == 0 {
			ready <- result{err: win32Error("SetWindowsHookExW", errno)}
			return
		}
		ready <- result{hook: hook, threadID: windows.GetCurrentThreadId()}

		for {
			ret, _, errno := procGetMessageW.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0)
			switch ret {
			case 0:
				log.Printf("hotkey: цикл сообщений завершён (WM_QUIT)")
				return
			case ^uintptr(0): // GetMessage возвращает -1 при ошибке
				log.Printf("hotkey: цикл сообщений прерван: %v", win32Error("GetMessageW", errno))
				return
			}
		}
	}()

	res := <-ready
	if res.err != nil {
		return nil, res.err
	}
	// stop снимает хук и завершает цикл сообщений его потока.
	return func() error {
		const wmQuit = 0x0012
		if r, _, errno := procUnhookWindowsHookEx.Call(res.hook); r == 0 {
			return win32Error("UnhookWindowsHookEx", errno)
		}
		if r, _, errno := procPostThreadMessageW.Call(uintptr(res.threadID), wmQuit, 0, 0); r == 0 {
			return win32Error("PostThreadMessageW", errno)
		}
		return nil
	}, nil
}

// hookProc — процедура хука WH_KEYBOARD_LL. Наши клавиши «съедаются»
// (возврат 1), обработчик запускается в отдельной горутине, чтобы хук
// возвращался быстро; остальные передаются дальше по цепочке хуков.
//
// lParam хука — указатель на KBDLLHOOKSTRUCT в памяти Windows;
// syscall.NewCallback сразу отдаёт его типизированным указателем, без
// небезопасного преобразования uintptr -> unsafe.Pointer.
//
// CallNextHookEx возвращает результат следующего хука в цепочке, а не
// признак ошибки, поэтому третье значение Call здесь не нужно.
func hookProc(nCode int, wParam uintptr, kb *kbdllHookStruct) uintptr {
	if nCode >= 0 && (wParam == wmKeyDown || wParam == wmSysKeyDown) {
		if handler := hotkeys[kb.VkCode]; handler != nil {
			go handler()
			return 1
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(unsafe.Pointer(kb)))
	return r
}
