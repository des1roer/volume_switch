// Command volume_switch sits in the system tray. F5 toggles an MPC-HC
// progress overlay window, F7/F8 cycle the default Windows playback
// device, the same way Volume2 does, and F2/F3 lower/raise the master
// volume. Ctrl+F2 turns all of these hotkeys off and back on.
package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
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
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	defaultURL      = "http://localhost:7777/" // корень web-интерфейса MPC-HC
	defaultInterval = 500 * time.Millisecond
	logFileName     = "volume_switch.log"
	appDirName      = "volume_switch" // каталог программы в %LOCALAPPDATA%: лог и .env

	volumeOverlayDuration = 1200 * time.Millisecond
	volumeOverlayWidth    = float32(260)
	volumeOverlayHeight   = float32(160)
	volumeTickCount       = 10 // ползунок размечен от 0 до 100 с шагом 10 (11 отметок)
)

// version — версия программы; релизная сборка подставляет её из тега через
// -ldflags "-X main.version=1.2.3" (см. .github/workflows/release.yml).
var version = "dev"

// dataDir возвращает каталог для лога и .env — %LOCALAPPDATA%\volume_switch.
// Сам каталог не создаёт. Рабочий каталог для этого не годится: при
// автозапуске это System32, а при установке через Chocolatey/winget/Scoop
// exe лежит в каталоге пакета. Переменная только ради тестов.
var dataDir = func() (string, error) {
	base, err := windows.KnownFolderPath(windows.FOLDERID_LocalAppData, 0)
	if err != nil {
		return "", err
	}
	return filepath.Join(base, appDirName), nil
}

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
	winmm    = windows.NewLazySystemDLL("winmm.dll")

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
	procGetAsyncKeyState    = user32.NewProc("GetAsyncKeyState")
	procPeekMessageW        = user32.NewProc("PeekMessageW")
	procPostThreadMessageW  = user32.NewProc("PostThreadMessageW")

	procShellNotifyIconW = shell32.NewProc("Shell_NotifyIconW")

	procPlaySoundW = winmm.NewProc("PlaySoundW")
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
		log.Printf("=== volume_switch %s starting (debug mode) ===", version)
		if consoleErr != nil {
			log.Printf("console: %v", consoleErr)
		}
		return func() {}
	}
	consoleErr := hideConsole()
	path, f, err := openLogFile()
	if err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "volume_switch: не удалось открыть лог, лог отключён: %v\n", err)
		if consoleErr != nil {
			_, _ = fmt.Fprintf(os.Stderr, "volume_switch: console: %v\n", consoleErr)
		}
		log.SetOutput(io.Discard)
		return func() {}
	}
	log.SetOutput(f)
	log.Printf("=== volume_switch %s starting (log -> %s) ===", version, path)
	if consoleErr != nil {
		log.Printf("console: %v", consoleErr)
	}
	return func() {
		log.SetOutput(io.Discard)
		if err := f.Close(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "volume_switch: закрытие %s: %v\n", path, err)
		}
	}
}

// openLogFile открывает на дозапись лог в dataDir, создавая каталог при
// необходимости, и возвращает путь к нему.
func openLogFile() (string, *os.File, error) {
	dir, err := dataDir()
	if err != nil {
		return "", nil, fmt.Errorf("каталог данных: %w", err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	path := filepath.Join(dir, logFileName)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	return path, f, err
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

// dotEnvPaths возвращает, где искать .env, в порядке приоритета: рабочий
// каталог (разработка, `go run .`), каталог exe (переносная установка) и
// dataDir. Повторяющиеся каталоги отбрасываются.
func dotEnvPaths() []string {
	var dirs []string
	if wd, err := os.Getwd(); err == nil {
		dirs = append(dirs, wd)
	}
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, filepath.Dir(exe))
	}
	if dir, err := dataDir(); err == nil {
		dirs = append(dirs, dir)
	}
	var paths []string
	for _, dir := range dirs {
		path := filepath.Join(dir, ".env")
		if !slices.ContainsFunc(paths, func(p string) bool { return strings.EqualFold(p, path) }) {
			paths = append(paths, path)
		}
	}
	return paths
}

// loadDotEnv читает KEY=VALUE построчно из .env и выставляет
// переменные окружения, не перезаписывая уже заданные извне (в том числе
// загруженные из .env с более высоким приоритетом).
func loadDotEnv(path string) {
	data, err := os.ReadFile(path)
	if err != nil {
		log.Printf(".env: %s не загружен: %v", path, err)
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
	log.Printf(".env: %s загружен, %d переменных", path, loaded)
}

// config — итоговые настройки программы.
type config struct {
	debug    bool
	mpcURL   string        // адрес web-интерфейса MPC-HC
	interval time.Duration // интервал опроса MPC-HC для оверлея
}

// resolveConfig собирает настройки по приоритету: флаги, затем окружение (к
// этому моменту в нём уже и значения из .env), затем умолчания. Пустой флаг
// (urlFlag == "", intervalFlag <= 0) означает «не задан».
func resolveConfig(debug bool, urlFlag string, intervalFlag time.Duration) config {
	cfg := config{debug: debug, mpcURL: defaultURL, interval: defaultInterval}

	if v := os.Getenv("MPC_URL"); v != "" {
		cfg.mpcURL = v
	}
	if urlFlag != "" {
		cfg.mpcURL = urlFlag
	}

	if v := os.Getenv("MPC_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			cfg.interval = d
		} else {
			log.Printf("MPC_INTERVAL=%q не парсится как duration, беру %s", v, defaultInterval)
		}
	}
	if intervalFlag > 0 {
		cfg.interval = intervalFlag
	}
	return cfg
}

// ---------------------------------------------------------------------------
// MPC-HC: состояние через JSON API web-интерфейса (status.json), команды и
// запуск плеера
// ---------------------------------------------------------------------------

// MPCState — ответ /status.json web-интерфейса MPC-HC. Из JSON берутся только
// поля, нужные оверлею; позиция и длительность — в миллисекундах. Без
// открытого файла MPC-HC отдаёт пустые строки и нули.
type MPCState struct {
	File        string `json:"file"`
	FilePath    string `json:"path"`
	StateString string `json:"stateString"`
	Position    int64  `json:"position"`
	Duration    int64  `json:"duration"`
}

// mpcEndpoint строит URL страницы web-интерфейса MPC-HC из адреса MPC_URL:
// последний сегмент пути заменяется на page (page может содержать query), а
// прежние query и fragment отбрасываются. Поэтому в MPC_URL годится и корень
// web-интерфейса, и любая его страница — например, прежнее значение по
// умолчанию variables.html:
//
//	http://localhost:7777                + status.json -> http://localhost:7777/status.json
//	http://localhost:7777/variables.html + status.json -> http://localhost:7777/status.json
//
// Пустой или неразбираемый адрес даёт пустую строку.
func mpcEndpoint(base, page string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	b, err := url.Parse(base)
	if err != nil {
		return ""
	}
	ref, err := url.Parse(page)
	if err != nil {
		return ""
	}
	return b.ResolveReference(ref).String()
}

// mpcClient — MPC-HC с точки зрения программы: его web-интерфейс (опрос
// состояния и команды) и сам exe, который запускается, если плеер не открыт.
type mpcClient struct {
	baseURL string // MPC_URL — корень web-интерфейса или любая его страница
	http    *http.Client

	// Где искать exe MPC-HC (см. exePath): путь по умолчанию и ключ HKCU, в
	// который MPC-HC сам пишет ExePath. В тестах подменяются.
	defaultExe  string
	registryKey string
}

func newMPCClient(baseURL string) *mpcClient {
	return &mpcClient{
		baseURL:     baseURL,
		http:        &http.Client{Timeout: 2 * time.Second},
		defaultExe:  defaultMPCExe,
		registryKey: mpcRegistryKey,
	}
}

// State запрашивает состояние плеера из /status.json. Эта страница есть
// в MPC-HC 2.x (форк clsid2, его же ставит K-Lite); у старых версий её нет —
// тогда приходит 404.
func (c *mpcClient) State() (_ *MPCState, err error) {
	statusURL := mpcEndpoint(c.baseURL, "status.json")
	if statusURL == "" {
		return nil, fmt.Errorf("некорректный адрес web-интерфейса %q", c.baseURL)
	}
	resp, err := c.http.Get(statusURL)
	if err != nil {
		return nil, err
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("закрытие ответа: %w", cerr)
		}
	}()
	switch resp.StatusCode {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, fmt.Errorf("HTTP %s: нет status.json — нужен MPC-HC 2.x", resp.Status)
	default:
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}

	var st MPCState
	if err = json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, fmt.Errorf("разбор status.json: %w", err)
	}
	return &st, nil
}

// mpcPlayPauseCommand — WM_COMMAND ID пункта «Play/Pause» в MPC-HC
// (ID_PLAY_PLAYPAUSE). Соседние команды: 887 Play, 888 Pause, 890 Stop — те
// же номера принимает web-интерфейс в command.html.
const mpcPlayPauseCommand = 889

// mpcCommandURL строит URL команды web-интерфейса MPC-HC (command.html, номер
// команды — параметром wm_command) из адреса MPC_URL, см. mpcEndpoint.
//
//	http://localhost:7777/ -> http://localhost:7777/command.html?wm_command=889
func mpcCommandURL(baseURL string, command int) string {
	return mpcEndpoint(baseURL, fmt.Sprintf("command.html?wm_command=%d", command))
}

// TogglePlayback отправляет в web-интерфейс MPC-HC команду «Play/Pause»,
// то есть ставит воспроизведение на паузу либо возобновляет его — в
// зависимости от текущего состояния плеера. Состояние заранее не
// запрашивается: команда сама по себе переключающая, а лишний опрос добавил
// бы только гонку между чтением состояния и его изменением. Если
// web-интерфейс не отвечает (MPC-HC не запущен), запускает MPC-HC.
func (c *mpcClient) TogglePlayback() {
	cmdURL := mpcCommandURL(c.baseURL, mpcPlayPauseCommand)
	if cmdURL == "" {
		log.Printf("mpc command: адрес web-интерфейса неизвестен, команда не отправлена")
		return
	}

	resp, err := c.http.Get(cmdURL)
	if err != nil {
		log.Printf("mpc command: не удалось отправить %s: %v — запускаю MPC-HC", cmdURL, err)
		c.Launch()
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

// Где по умолчанию искать MPC-HC (см. mpcClient.exePath).
const (
	defaultMPCExe  = `C:\Program Files (x86)\K-Lite Codec Pack\MPC-HC64\mpc-hc64.exe`
	mpcRegistryKey = `Software\MPC-HC\MPC-HC`
)

// exePath возвращает путь к MPC-HC: MPC_EXE из окружения/.env; иначе
// defaultExe, если такой файл есть; иначе путь, который MPC-HC сам пишет в
// реестр при каждом запуске (HKCU\Software\MPC-HC\MPC-HC\ExePath).
func (c *mpcClient) exePath() string {
	if p := os.Getenv("MPC_EXE"); p != "" {
		return p
	}
	_, err := os.Stat(c.defaultExe)
	if err == nil {
		return c.defaultExe
	}
	if !errors.Is(err, os.ErrNotExist) {
		log.Printf("mpc launch: проверка %s: %v", c.defaultExe, err)
	}
	if p := c.exeFromRegistry(); p != "" {
		return p
	}
	return c.defaultExe
}

// exeFromRegistry читает ExePath из ключа registryKey в HKCU; "" — если
// значения нет или его не удалось прочитать.
func (c *mpcClient) exeFromRegistry() string {
	key, err := registry.OpenKey(registry.CURRENT_USER, c.registryKey, registry.QUERY_VALUE)
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

// Launch запускает MPC-HC. Повторный запуск при уже открытом плеере
// безвреден: MPC-HC по умолчанию работает в режиме одного экземпляра и просто
// выводит своё окно на передний план.
func (c *mpcClient) Launch() {
	exe := c.exePath()
	cmd := exec.Command(exe)
	cmd.Dir = filepath.Dir(exe)
	if err := startDetached(cmd); err != nil {
		log.Printf("mpc launch: не удалось запустить %s: %v", exe, err)
		return
	}
	log.Printf("mpc launch: запущен %s", exe)
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

// StepVolume меняет громкость текущего default-устройства на delta (доля
// шкалы 0..1; отрицательная — тише) и, как клавиши громкости Windows,
// снимает mute.
func (s *Switcher) StepVolume(delta float64) error {
	return s.exec(func() error { return s.stepVolumeLocked(delta) })
}

// stepLevel возвращает уровень громкости после шага delta: в пределах 0..1
// и округлённый до целого процента, чтобы ошибки float32 не копились от
// нажатия к нажатию.
func stepLevel(level, delta float64) float64 {
	return min(max(math.Round((level+delta)*100)/100, 0), 1)
}

// endpointVolumeLocked возвращает ID текущего default-устройства и его
// IAudioEndpointVolume; вызывающий освобождает aev.
func (s *Switcher) endpointVolumeLocked() (string, *wca.IAudioEndpointVolume, error) {
	var dev *wca.IMMDevice
	if err := s.mmde.GetDefaultAudioEndpoint(wca.ERender, wca.EConsole, &dev); err != nil {
		return "", nil, err
	}
	defer dev.Release()

	id, err := deviceID(dev)
	if err != nil {
		return "", nil, err
	}
	var aev *wca.IAudioEndpointVolume
	if err := dev.Activate(wca.IID_IAudioEndpointVolume, wca.CLSCTX_ALL, nil, &aev); err != nil {
		return "", nil, err
	}
	return id, aev, nil
}

func (s *Switcher) stepVolumeLocked(delta float64) error {
	_, aev, err := s.endpointVolumeLocked()
	if err != nil {
		return err
	}
	defer aev.Release()

	var scalar float32
	if err := aev.GetMasterVolumeLevelScalar(&scalar); err != nil {
		return err
	}
	if err := aev.SetMasterVolumeLevelScalar(float32(stepLevel(float64(scalar), delta)), nil); err != nil {
		return err
	}
	// SetMute без изменения состояния возвращает S_FALSE, а go-wca считает
	// ошибкой любой ненулевой HRESULT, поэтому mute снимается, только если
	// он действительно включён.
	var muted bool
	if err := aev.GetMute(&muted); err != nil || !muted {
		return err
	}
	return aev.SetMute(false, nil)
}

func (s *Switcher) currentVolumeLocked() (volumeReading, error) {
	id, aev, err := s.endpointVolumeLocked()
	if err != nil {
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
	niifNoSound          = 0x00000010
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
//
// With silent the notification plays no sound of its own (the caller plays
// one instead).
func showNotification(title, body string, silent bool) {
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
	if silent {
		nid.dwInfoFlags |= niifNoSound
	}
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

// overlays — Fyne-приложение с двумя окнами-оверлеями: прогресс MPC-HC (по
// F5) и ползунок громкости (при изменении громкости).
type overlays struct {
	app      fyne.App
	progress *progressOverlay
	volume   *volumeOverlay
}

// newOverlays создаёт в приложении оба окна-оверлея и оставляет их скрытыми.
// Вызывается на горутине main (см. main), как и Run.
func newOverlays(a fyne.App, mpc *mpcClient) *overlays {
	a.Settings().SetTheme(theme.DarkTheme())
	o := &overlays{
		app:      a,
		progress: newProgressOverlay(a, mpc),
		volume:   newVolumeOverlay(a),
	}

	// Нативный HWND у Fyne/GLFW создаётся лениво, только при первом Show(),
	// и в том же вызове сразу становится видимым. Если окно хоть раз
	// показалось с обычным стилем, Explorer заводит ему кнопку в панели
	// задач и не убирает её, даже когда окно потом получает
	// WS_EX_TOOLWINDOW и прячется: на кнопке остаётся «призрак» (особенно
	// при автозапуске на входе в Windows, когда Explorer ещё не успевает
	// обработать смену стиля до скрытия). Поэтому окна создаются здесь
	// вхолостую (показ + скрытие) под CBT-хуком createAsToolWindows,
	// который выставляет WS_EX_TOOLWINDOW в момент создания HWND — до
	// того, как окно хоть раз станет видимым.
	//
	// После этого оба окна стартуют скрытыми: прогресс покажем по F5,
	// громкость — по событию от volume poller.
	createAsToolWindows(func() {
		for _, w := range []fyne.Window{o.progress.window, o.volume.window} {
			showOverlay(w, w == o.progress.window)
			w.Hide()
		}
	})
	log.Printf("fyne: окна созданы, стартуют скрытыми")
	return o
}

// Run запускает опрос MPC-HC и цикл приложения Fyne. Блокируется до Quit
// (обычно — до выхода из трея).
func (o *overlays) Run(interval time.Duration) {
	log.Printf("fyne: запуск (interval=%s)", interval)
	go o.progress.Poll(time.Tick(interval))
	o.app.Run()
	log.Printf("fyne: цикл приложения завершён")
}

// Quit завершает цикл приложения Fyne.
func (o *overlays) Quit() {
	o.app.Quit()
}

// progressOverlay — окно с прогрессом воспроизведения MPC-HC: полоса с
// именем файла и статусом, слева прошедшее время, справа оставшееся.
//
// Все поля, кроме неизменяемых window и mpc, читаются и меняются только в
// UI-потоке Fyne (внутри fyne.Do или в обработчиках событий), поэтому
// мьютекс не нужен.
type progressOverlay struct {
	window fyne.Window
	mpc    *mpcClient

	visible bool

	elapsed   *widget.Label
	remaining *widget.Label
	bar       *widget.ProgressBar

	// text — текущий текст поверх полосы прогресса (имя файла и статус
	// воспроизведения), читается из bar.TextFormatter.
	text string

	// filePath — полный путь к текущему файлу MPC-HC (поле path из
	// status.json), используется по клику колесом для открытия папки.
	filePath string
}

func newProgressOverlay(a fyne.App, mpc *mpcClient) *progressOverlay {
	p := &progressOverlay{window: a.NewWindow(windowTitle), mpc: mpc}

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

	p.window.Resize(fyne.NewSize(windowWidth, windowHeight))
	requestPosition(p.window, posX, posY)

	// --- Виджеты ---
	// Имя файла и статус выводятся прямо на полосе прогресса (через
	// TextFormatter), а не отдельной строкой — главный элемент оверлея это
	// сама полоса, текст встроен в неё.
	p.elapsed = widget.NewLabelWithStyle("00:00",
		fyne.TextAlignLeading, fyne.TextStyle{Bold: true, Monospace: true})
	p.remaining = widget.NewLabelWithStyle("00:00",
		fyne.TextAlignTrailing, fyne.TextStyle{Bold: true, Monospace: true})

	p.text = "Ожидание подключения к MPC-HC..."
	p.bar = widget.NewProgressBar()
	p.bar.TextFormatter = func() string { return p.text }

	progressRow := container.NewBorder(nil, nil, p.elapsed, p.remaining, p.bar)
	pad := theme.Padding()
	body := container.New(layout.NewCustomPaddedLayout(pad, pad/2, pad, pad), progressRow)
	p.window.SetContent(container.NewStack(body, newClickCatcher(p)))
	return p
}

// Toggle показывает окно, если оно скрыто, и прячет, если показано.
func (p *progressOverlay) Toggle() {
	fyne.Do(func() {
		if p.visible {
			p.window.Hide()
			log.Printf("F5: окно скрыто")
		} else {
			showOverlay(p.window, true)
			log.Printf("F5: окно показано поверх всех окон")
		}
		p.visible = !p.visible
	})
}

// Poll на каждый тик опрашивает web-интерфейс MPC-HC и обновляет оверлей.
// Работает, пока не закрыт ticks.
func (p *progressOverlay) Poll(ticks <-chan time.Time) {
	var lastErr string
	for range ticks {
		st, err := p.mpc.State()
		if err != nil {
			if err.Error() != lastErr {
				log.Printf("mpc poll: ошибка: %v", err)
				lastErr = err.Error()
			}
			fyne.Do(func() {
				p.text = "⚠ Нет связи с MPC-HC: " + err.Error()
				p.bar.Refresh()
			})
			continue
		}
		if lastErr != "" {
			log.Printf("mpc poll: связь восстановлена")
			lastErr = ""
		}
		fyne.Do(func() { p.show(st) })
	}
}

// show выводит состояние MPC-HC на оверлей. Вызывается только из UI-потока
// (через fyne.Do).
func (p *progressOverlay) show(st *MPCState) {
	p.filePath = st.FilePath
	if st.Duration <= 0 {
		p.text = "Нет активного воспроизведения"
		p.bar.SetValue(0)
		p.elapsed.SetText("00:00")
		p.remaining.SetText("00:00")
		return
	}
	progress := min(max(float64(st.Position)/float64(st.Duration), 0), 1)

	p.elapsed.SetText(formatTime(st.Position))
	p.remaining.SetText("-" + formatTime(st.Duration-st.Position))
	p.text = fmt.Sprintf("%s   —   %.1f%%  %s  (из %s)",
		st.File, progress*100, st.StateString, formatTime(st.Duration))
	p.bar.SetValue(progress)
}

// inBar сообщает, попала ли точка клика (в координатах окна) в границы
// полосы прогресса. Полоса занимает почти всю ширину оверлея, но клик по
// подписям времени или по пустым полям окна воспроизведение не переключает.
func (p *progressOverlay) inBar(pt fyne.Position) bool {
	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(p.bar)
	size := p.bar.Size()
	return pt.X >= pos.X && pt.X <= pos.X+size.Width &&
		pt.Y >= pos.Y && pt.Y <= pos.Y+size.Height
}

// clickCatcher — прозрачный виджет на весь оверлей прогресса, который ловит
// клики по нему:
//   - средняя кнопка (колесо) — открывает папку с текущим файлом в проводнике;
//   - левая кнопка по полосе прогресса — ставит воспроизведение на паузу или
//     возобновляет его (команда 889 «Play/Pause» web-интерфейса MPC-HC); если
//     web-интерфейс недоступен (MPC-HC не запущен) — запускает MPC-HC.
//
// Правая кнопка не перехватывается вовсе.
type clickCatcher struct {
	widget.BaseWidget

	overlay *progressOverlay
}

func newClickCatcher(overlay *progressOverlay) *clickCatcher {
	c := &clickCatcher{overlay: overlay}
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
		openContainingFolder(c.overlay.filePath)
	case desktop.MouseButtonPrimary:
		if !c.overlay.inBar(ev.Position) {
			return
		}
		log.Printf("overlay: клик левой кнопкой по полосе прогресса — переключаю воспроизведение")
		// Запрос уходит из отдельной горутины: если MPC-HC не отвечает,
		// HTTP-клиент ждёт до своего таймаута, и держать на этом событийный
		// цикл Fyne нельзя.
		go c.overlay.mpc.TogglePlayback()
	}
}

// volumeOverlay — оверлей-ползунок уровня громкости, показывается по
// событиям поллера громкости (см. Show). Все позиции/размеры примитивов
// выставляются вручную в layout при каждом показе (окно без автоматического
// layout-менеджера, т.к. размер оверлея каждый раз пересчитывается под
// монитор курсора).
type volumeOverlay struct {
	window     fyne.Window
	percent    *canvas.Text
	track      *canvas.Rectangle
	fill       *canvas.Rectangle
	ticks      []*canvas.Line
	tickLabels []*canvas.Text

	mu        sync.Mutex // защищает hideTimer
	hideTimer *time.Timer
}

// newVolumeOverlay создаёт окно ползунка: большой процент и полоса с
// градацией 0..100.
func newVolumeOverlay(a fyne.App) *volumeOverlay {
	v := &volumeOverlay{window: a.NewWindow(volumeWindowTitle)}

	v.percent = canvas.NewText("", color.White)
	v.percent.Alignment = fyne.TextAlignCenter
	v.percent.TextStyle = fyne.TextStyle{Bold: true}

	v.track = canvas.NewRectangle(volumeTrackColor)
	v.fill = canvas.NewRectangle(volumeFillColor)

	objects := []fyne.CanvasObject{v.track, v.fill, v.percent}
	v.ticks = make([]*canvas.Line, volumeTickCount+1)
	v.tickLabels = make([]*canvas.Text, volumeTickCount+1)
	for i := range v.ticks {
		line := canvas.NewLine(volumeTickColor)
		label := canvas.NewText(strconv.Itoa(i*100/volumeTickCount), volumeLabelColor)
		label.Alignment = fyne.TextAlignCenter
		v.ticks[i] = line
		v.tickLabels[i] = label
		objects = append(objects, line, label)
	}
	v.window.SetContent(container.NewWithoutLayout(objects...))
	return v
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
		swpNoMove       = 0x0002
		swpNoSize       = 0x0001
		swpNoZOrder     = 0x0004
		swpNoActivate   = 0x0010
		swpFrameChanged = 0x0020

		dwmwaUseImmersiveDarkMode = 20

		hwndTopmost = ^uintptr(0) // HWND_TOPMOST = (HWND)-1
	)

	titlePtr, err := syscall.UTF16PtrFromString(title)
	if err != nil {
		return fmt.Errorf("заголовок %q: %w", title, err)
	}
	hwnd, _, errno := procFindWindowW.Call(0, uintptr(unsafe.Pointer(titlePtr)))
	if hwnd == 0 {
		return fmt.Errorf("окно %q: %w", title, win32Error("FindWindowW", errno))
	}

	if err := setToolWindowStyle(hwnd); err != nil {
		return err
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

// setToolWindowStyle делает окно toolwindow: такие окна не получают кнопку
// в панели задач (WS_EX_APPWINDOW, принудительно её дающий, снимается).
func setToolWindowStyle(hwnd uintptr) error {
	const (
		wsExToolWindow = 0x00000080
		wsExAppWindow  = 0x00040000
	)
	// -20 переполняет uintptr как константа компиляции; через typed
	// переменную Go делает корректное sign-extension в рантайме.
	var gwlExStyle int32 = -20

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
	return nil
}

// overlayWindowClass — класс нативных окон Fyne/GLFW, которым
// createAsToolWindows ставит WS_EX_TOOLWINDOW при создании. Переменная, а не
// константа, только ради тестов: там окна создаются стандартного класса.
var overlayWindowClass = "GLFW30"

// cbtHookCallback создаётся один раз: колбэки syscall.NewCallback никогда не
// освобождаются, а их число в процессе ограничено.
var cbtHookCallback = syscall.NewCallback(cbtHookProc)

// createAsToolWindows выполняет f, пока на текущем потоке стоит хук WH_CBT,
// который делает каждое создаваемое окно класса overlayWindowClass
// toolwindow сразу в момент создания (HCBT_CREATEWND приходит до
// WM_NCCREATE, окно ещё ни разу не было видимым). Так у Explorer нет
// момента, когда окно видно с обычным стилем, и кнопка в панели задач для
// него не появляется вообще. Хук локальный для потока: окна Fyne создаются
// на потоке, вызвавшем Show() (горутина main, см. newOverlays и main).
//
// Если хук поставить не удалось, f всё равно выполняется — окно лишь
// рискует остаться на панели задач, как раньше.
func createAsToolWindows(f func()) {
	const whCBT = 5

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	hook, _, errno := procSetWindowsHookExW.Call(whCBT, cbtHookCallback, 0, uintptr(windows.GetCurrentThreadId()))
	if hook == 0 {
		log.Printf("overlay: %v — окна могут попасть на панель задач", win32Error("SetWindowsHookExW", errno))
		f()
		return
	}
	defer func() {
		if r, _, errno := procUnhookWindowsHookEx.Call(hook); r == 0 {
			log.Printf("overlay: %v", win32Error("UnhookWindowsHookEx", errno))
		}
	}()
	f()
}

// cbtHookProc — процедура хука WH_CBT из createAsToolWindows. На
// HCBT_CREATEWND wParam — HWND только что созданного окна (имена классов в
// Windows регистронезависимы, отсюда EqualFold). Возврат 0
// разрешает создание окна; CallNextHookEx возвращает результат следующего
// хука в цепочке, а не признак ошибки.
func cbtHookProc(nCode int, wParam, lParam uintptr) uintptr {
	const hcbtCreateWnd = 3
	if nCode == hcbtCreateWnd {
		var cls [64]uint16
		n, err := windows.GetClassName(windows.HWND(wParam), &cls[0], int32(len(cls)))
		if err == nil && strings.EqualFold(windows.UTF16ToString(cls[:n]), overlayWindowClass) {
			if err := setToolWindowStyle(wParam); err != nil {
				log.Printf("overlay: стиль toolwindow при создании окна: %v", err)
			}
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, lParam)
	return r
}

// layout расставляет все примитивы ползунка (трек, заливку, текст процента,
// отметки и подписи градации 0..100) под уже вычисленный размер окна
// width x height. Окно создано через container.NewWithoutLayout, поэтому
// автоматической раскладки нет — координаты считаются вручную и
// пересчитываются при каждом показе, т.к. размер каждый раз подгоняется под
// монитор курсора.
func (v *volumeOverlay) layout(width, height float32, level float64, muted bool) {
	pad := width * 0.06
	barX := pad
	barWidth := width - 2*pad
	barHeight := height * 0.22
	barY := height * 0.5
	radius := barHeight / 2

	v.track.CornerRadius = radius
	v.track.Resize(fyne.NewSize(barWidth, barHeight))
	v.track.Move(fyne.NewPos(barX, barY))
	v.track.Refresh()

	clamped := min(max(level, 0), 1)
	v.fill.FillColor = volumeFillColor
	if muted {
		v.fill.FillColor = volumeMuteColor
	}
	v.fill.CornerRadius = radius
	v.fill.Resize(fyne.NewSize(barWidth*float32(clamped), barHeight))
	v.fill.Move(fyne.NewPos(barX, barY))
	v.fill.Refresh()

	text := fmt.Sprintf("%d%%", int(math.Round(level*100)))
	if muted {
		text = "MUTE"
	}
	v.percent.Text = text
	v.percent.TextSize = height * 0.26
	v.percent.Resize(fyne.NewSize(width, height*0.4))
	v.percent.Move(fyne.NewPos(0, height*0.05))
	v.percent.Refresh()

	tickY := barY + barHeight + 6
	labelSize := height * 0.06
	const labelWidth = float32(48)
	for i, line := range v.ticks {
		x := barX + barWidth*float32(i)/float32(volumeTickCount)
		line.Position1 = fyne.NewPos(x, tickY)
		line.Position2 = fyne.NewPos(x, tickY+10)
		line.StrokeWidth = 2
		line.Refresh()

		label := v.tickLabels[i]
		label.TextSize = labelSize
		label.Resize(fyne.NewSize(labelWidth, labelSize+4))
		label.Move(fyne.NewPos(x-labelWidth/2, tickY+12))
		label.Refresh()
	}
}

// Show показывает оверлей-ползунок уровня громкости (0..100, с градацией
// через каждые 10) в четверть экрана (половина ширины и половина высоты
// рабочей области монитора под курсором мыши), прижатый к левому нижнему
// углу, и прячет его через volumeOverlayDuration после последнего вызова.
// Таймер скрытия каждый раз перезапускается (debounce), чтобы при быстрой
// серии событий — например, при удержании клавиши громкости — оверлей не
// мигал, а гас один раз после того, как изменения прекратились.
func (v *volumeOverlay) Show(level float64, muted bool) {
	fyne.Do(func() {
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

		v.layout(width, height, level, muted)
		v.window.Resize(fyne.NewSize(width, height))
		requestPosition(v.window, x, y)
		log.Printf("volume overlay: показываю размер=%.0fx%.0f позиция=(%d,%d) level=%.0f%% muted=%v",
			width, height, x, y, level*100, muted)

		showOverlay(v.window, false)
	})

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.hideTimer != nil {
		v.hideTimer.Stop()
	}
	v.hideTimer = time.AfterFunc(volumeOverlayDuration, func() {
		fyne.Do(v.window.Hide)
	})
}

// ---------------------------------------------------------------------------
// Автозапуск через реестр (HKCU\...\Run)
// ---------------------------------------------------------------------------

const (
	autostartValueName = "VolumeSwitch"
	autostartRunKey    = `Software\Microsoft\Windows\CurrentVersion\Run`
)

// autostart — автозапуск программы через значение autostartValueName в
// ключе HKCU\key (в программе — autostartRunKey, в тестах — временный ключ).
type autostart struct {
	key string
}

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

// Enabled проверяет, что значение в ключе автозапуска существует и указывает
// именно на текущий exe (а не на устаревший путь).
func (a autostart) Enabled() bool {
	cmd, err := autostartCommand()
	if err != nil {
		log.Printf("autostart: путь к exe: %v", err)
		return false
	}
	key, err := registry.OpenKey(registry.CURRENT_USER, a.key, registry.QUERY_VALUE)
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

// Set включает или выключает автозапуск, записывая/удаляя значение в ключе
// автозапуска.
func (a autostart) Set(enable bool) (err error) {
	key, err := registry.OpenKey(registry.CURRENT_USER, a.key, registry.SET_VALUE)
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

// deviceSwitcher — то, что меню трея и хоткеи используют от *Switcher;
// интерфейс нужен, чтобы их можно было проверить без COM и аудиоустройств.
type deviceSwitcher interface {
	SetDefault(id string) error
	Cycle(direction int) (Device, error)
	StepVolume(delta float64) error
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
	urlFlag := flag.String("url", "", "адрес web-интерфейса MPC-HC (перекрывает MPC_URL)")
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

	for _, path := range dotEnvPaths() {
		loadDotEnv(path)
	}
	cfg := resolveConfig(*debug, *urlFlag, *intervalFlag)
	log.Printf("config: url=%s interval=%s debug=%v", cfg.mpcURL, cfg.interval, cfg.debug)

	// Fyne 2.8+ требует, чтобы app.New/NewWindow/Run вызывались на горутине main()
	// (см. https://docs.fyne.io/started/goroutines) — именно она закреплена за
	// главным потоком процесса пакетом systray в своём init(). Поэтому оверлеи
	// создаются и работают здесь, а systray переезжает в отдельную закреплённую
	// горутину. Оверлеи создаются до запуска трея, так что хоткеи и меню трея
	// всегда находят готовые окна.
	runtime.LockOSThread()
	ui := newOverlays(app.New(), newMPCClient(cfg.mpcURL))
	tray := &trayApp{debug: cfg.debug, ui: ui}
	go func() {
		runtime.LockOSThread()
		log.Printf("systray: запуск")
		systray.Run(tray.onReady, tray.onExit)
		log.Printf("systray: завершён, выход")
	}()

	ui.Run(cfg.interval)
}

// trayApp — программа с точки зрения трея: связывает меню трея, аудио
// (switcher), хоткеи и оверлеи. onReady и onExit — колбэки systray.Run.
//
// switcher и hotkeys выставляются в onReady до того, как запускаются
// горутины меню и хук клавиатуры, которые их читают, поэтому без мьютекса.
type trayApp struct {
	debug bool
	ui    *overlays

	switcher deviceSwitcher // nil, если аудио не инициализировалось
	hotkeys  *hotkeyHook    // nil, если хук не установлен
	devices  deviceMenu
}

func (a *trayApp) onReady() {
	log.Printf("systray: onReady")
	if a.debug {
		// systray мог спрятать консоль повторно
		if err := ensureConsole(); err != nil {
			log.Printf("console: %v", err)
		}
	}
	systray.SetIcon(trayIconData)
	systray.SetTitle("")
	systray.SetTooltip("Volume Switch")

	// В switcher (интерфейс) попадает только успешно созданный *Switcher:
	// nil-указатель в интерфейсе прошёл бы проверку на nil в onExit.
	sw, err := NewSwitcher()
	if err != nil {
		log.Printf("audio: ошибка инициализации: %v", err)
		systray.SetTooltip("Volume Switch: audio init failed")
		return
	}
	a.switcher = sw
	log.Printf("audio: switcher создан")

	a.addDeviceItems(sw)
	systray.AddSeparator()
	a.addMPCItem()
	a.addAutostartItem(autostart{key: autostartRunKey})
	a.addQuitItem()
	a.startHotkeys()

	// Колбэк вызывается из горутины поллера; вся работа с UI внутри Show
	// идёт через fyne.Do.
	go pollVolume(sw.CurrentVolume, a.ui.volume.Show, time.Tick(volumePollInterval))
}

func (a *trayApp) onExit() {
	log.Printf("onExit: остановка")
	if a.hotkeys != nil {
		if err := a.hotkeys.Stop(); err != nil {
			log.Printf("onExit: не удалось снять хук: %v", err)
		} else {
			log.Printf("onExit: хук снят")
		}
	}
	if a.ui != nil {
		a.ui.Quit()
		log.Printf("onExit: fyne остановлен")
	}
	if a.switcher != nil {
		a.switcher.Close()
		log.Printf("onExit: switcher закрыт")
	}
	log.Printf("onExit: всё завершено")
}

// addDeviceItems добавляет в меню трея по пункту на каждое активное
// устройство воспроизведения.
func (a *trayApp) addDeviceItems(sw *Switcher) {
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

	a.devices.build(devices, currentID, a.selectDevice)
}

// addMPCItem добавляет пункт меню, который показывает/скрывает оверлей
// MPC-HC (как F5).
func (a *trayApp) addMPCItem() {
	item := systray.AddMenuItem("Показать/скрыть MPC", "F5")
	go func() {
		for range item.ClickedCh {
			log.Printf("tray: пункт «Показать/скрыть MPC» нажат")
			a.ui.progress.Toggle()
		}
	}()
}

// addAutostartItem добавляет пункт меню с галочкой, включающий/выключающий
// автозапуск.
func (a *trayApp) addAutostartItem(run autostart) {
	item := systray.AddMenuItemCheckbox(
		"Запускать при входе в Windows",
		"Добавить/убрать из автозагрузки Windows",
		run.Enabled(),
	)
	go func() {
		for range item.ClickedCh {
			enable := !item.Checked()
			if err := run.Set(enable); err != nil {
				log.Printf("autostart: не удалось изменить: %v", err)
				continue
			}
			if enable {
				item.Check()
			} else {
				item.Uncheck()
			}
			log.Printf("autostart: включён=%v", enable)
		}
	}()
}

func (a *trayApp) addQuitItem() {
	item := systray.AddMenuItem("Выход", "Закрыть Volume Switch")
	go func() {
		<-item.ClickedCh
		log.Printf("tray: «Выход» нажат")
		systray.Quit()
	}()
}

// startHotkeys ставит глобальный хук клавиатуры с обработчиками F2/F3/F5/F7/F8.
func (a *trayApp) startHotkeys() {
	hk := newHotkeyHook(map[uint32]func(){
		vkF2: func() { log.Printf("hotkey: F2"); a.stepVolume(-volumeKeyStep) },
		vkF3: func() { log.Printf("hotkey: F3"); a.stepVolume(volumeKeyStep) },
		vkF5: func() { log.Printf("hotkey: F5"); a.ui.progress.Toggle() },
		vkF7: func() { log.Printf("hotkey: F7"); a.cycle(-1) },
		vkF8: func() { log.Printf("hotkey: F8"); a.cycle(1) },
	}, announceHotkeys)
	if err := hk.Start(); err != nil {
		log.Printf("hotkey: не удалось установить хук: %v", err)
		return
	}
	a.hotkeys = hk
	log.Printf("hotkey: хук установлен (F2/F3/F5/F7/F8, Ctrl+F2 — вкл/выкл)")
}

// selectDevice делает dev устройством по умолчанию (клик по пункту меню).
func (a *trayApp) selectDevice(dev Device) {
	log.Printf("audio: SetDefault id=%s name=%q", dev.ID, dev.Name)
	if err := a.switcher.SetDefault(dev.ID); err != nil {
		log.Printf("audio: SetDefault failed: %v", err)
		return
	}
	a.devices.check(dev.ID)
	announceSwitch(dev)
}

// cycle переключает устройство по умолчанию на следующее (direction > 0) или
// предыдущее (direction < 0) — F8/F7.
func (a *trayApp) cycle(direction int) {
	log.Printf("audio: Cycle direction=%d", direction)
	dev, err := a.switcher.Cycle(direction)
	if err != nil {
		log.Printf("audio: Cycle failed: %v", err)
		return
	}
	log.Printf("audio: новое устройство: id=%s name=%q", dev.ID, dev.Name)
	a.devices.check(dev.ID)
	announceSwitch(dev)
}

// volumeKeyStep — шаг громкости по F2/F3, как у мультимедийных клавиш
// громкости Windows (2% шкалы).
const volumeKeyStep = 0.02

// stepVolume меняет общую громкость на delta (F2/F3). Оверлей громкости
// отдельно не показывается — его поднимает pollVolume, увидев изменение
// уровня.
func (a *trayApp) stepVolume(delta float64) {
	if err := a.switcher.StepVolume(delta); err != nil {
		log.Printf("audio: StepVolume(%+.2f) failed: %v", delta, err)
	}
}

// deviceMenu — пункты меню трея с устройствами воспроизведения; галочка
// стоит на текущем устройстве по умолчанию.
type deviceMenu struct {
	mu    sync.Mutex
	items map[string]checkbox // пункты меню по ID устройства
}

// build добавляет в меню трея по пункту на каждое устройство; клик по пункту
// вызывает onSelect с этим устройством.
func (m *deviceMenu) build(devices []Device, currentID string, onSelect func(Device)) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.items = make(map[string]checkbox, len(devices))
	for _, dev := range devices {
		item, clicked := addTrayCheckbox(dev.Name, "Сделать устройством по умолчанию", dev.ID == currentID)
		m.items[dev.ID] = item

		go func() {
			for range clicked {
				log.Printf("tray: выбран %q", dev.Name)
				onSelect(dev)
			}
		}()
	}
	log.Printf("menu: построено %d пунктов", len(devices))
}

// check ставит галочку на пункт устройства id и снимает с остальных.
func (m *deviceMenu) check(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()

	for itemID, item := range m.items {
		if itemID == id {
			item.Check()
		} else {
			item.Uncheck()
		}
	}
}

func announceSwitch(dev Device) {
	setTrayTooltip("Volume Switch: " + dev.Name)
	showNotification("Аудиовыход переключён", dev.Name, false)
}

// announceHotkeys сообщает о включении/выключении хоткеев по Ctrl+F2:
// уведомление в углу экрана и системный звук — разный для включения и
// выключения, чтобы состояние было понятно и на слух. Собственный звук
// уведомления отключён, иначе звучали бы оба.
func announceHotkeys(disabled bool) {
	title, body, sound := "Горячие клавиши включены", "F2/F3, F5, F7/F8 снова работают", "DeviceConnect"
	if disabled {
		title, body, sound = "Горячие клавиши выключены", "Ctrl+F2 — включить снова", "DeviceDisconnect"
	}
	log.Printf("hotkey: %s", strings.ToLower(title))
	showNotification(title, body, true)
	playSystemSound(sound)
}

// playSystemSound асинхронно проигрывает системный звук по его псевдониму
// из панели «Звуки» (DeviceConnect и т.п.). Переменная только ради тестов:
// там звук не нужен.
var playSystemSound = func(alias string) {
	const (
		sndAsync     = 0x00000001
		sndNoDefault = 0x00000002
		sndAlias     = 0x00010000
	)
	name, err := windows.UTF16PtrFromString(alias)
	if err != nil {
		log.Printf("audio: звук %q: %v", alias, err)
		return
	}
	if r, _, errno := procPlaySoundW.Call(uintptr(unsafe.Pointer(name)), 0, sndAlias|sndAsync|sndNoDefault); r == 0 {
		log.Printf("audio: звук %q: %v", alias, win32Error("PlaySoundW", errno))
	}
}

// ---------------------------------------------------------------------------
// Глобальный хук клавиатуры Windows (F2/F3/F5/F7/F8, Ctrl+F2)
// ---------------------------------------------------------------------------

const (
	whKeyboardLL = 13
	wmKeyDown    = 0x0100
	wmSysKeyDown = 0x0104
	wmKeyUp      = 0x0101
	wmSysKeyUp   = 0x0105
	llkhfAltDown = 0x20 // KBDLLHOOKSTRUCT.Flags: зажат Alt

	vkShift   = 0x10
	vkControl = 0x11
	vkLWin    = 0x5B
	vkRWin    = 0x5C

	vkF2 = 0x71
	vkF3 = 0x72
	vkF5 = 0x74
	vkF7 = 0x76
	vkF8 = 0x77
)

// keyHeld сообщает, зажата ли сейчас клавиша vk. Переменная только ради
// тестов: настоящее состояние клавиатуры в тесте не задать.
var keyHeld = func(vk uintptr) bool {
	r, _, _ := procGetAsyncKeyState.Call(vk)
	return r&0x8000 != 0
}

// modifierHeld сообщает, нажата ли клавиша вместе с Alt, Ctrl, Shift или
// Win. Такие сочетания хоткеями не считаются и уходят программам: иначе хук
// съедал бы Alt+F4 (закрыть окно), Ctrl+F4 (закрыть вкладку), Shift+F3,
// Ctrl+F5 и т.п.
func modifierHeld(kb *kbdllHookStruct) bool {
	if kb.Flags&llkhfAltDown != 0 {
		return true
	}
	for _, vk := range []uintptr{vkShift, vkControl, vkLWin, vkRWin} {
		if keyHeld(vk) {
			return true
		}
	}
	return false
}

type kbdllHookStruct struct {
	VkCode      uint32
	ScanCode    uint32
	Flags       uint32
	Time        uint32
	DwExtraInfo uintptr
}

// vkToggleHotkeys с зажатым Ctrl включает/выключает хоткеи (Ctrl+F2).
const vkToggleHotkeys = vkF2

// toggleComboPressed сообщает, что нажата клавиша переключения хоткеев
// вместе с Ctrl и без других модификаторов: Ctrl+Shift+F2 и т.п. уходят
// программам.
func toggleComboPressed(kb *kbdllHookStruct) bool {
	if kb.VkCode != vkToggleHotkeys || kb.Flags&llkhfAltDown != 0 || !keyHeld(vkControl) {
		return false
	}
	return !slices.ContainsFunc([]uintptr{vkShift, vkLWin, vkRWin}, keyHeld)
}

// hotkeyHook — глобальный хук клавиатуры WH_KEYBOARD_LL. Перехватывает
// клавиши из handlers (только без модификаторов) и Ctrl+F2, который
// выключает/включает все остальные.
type hotkeyHook struct {
	// handlers — обработчики перехватываемых клавиш по virtual-key коду. Не
	// меняются после создания, поэтому proc читает их без блокировок.
	handlers map[uint32]func()
	// onToggle вызывается в отдельной горутине после каждого Ctrl+F2.
	onToggle func(disabled bool)

	// disabled — хоткеи выключены по Ctrl+F2. Пишется только в proc,
	// читается там же и в тестах, поэтому атомарный.
	disabled atomic.Bool
	// toggleKeyDown — Ctrl+F2 нажат и F2 ещё не отпущена (подавление
	// автоповтора). Используется только в proc, который Windows вызывает
	// последовательно в потоке хука, поэтому без синхронизации.
	toggleKeyDown bool

	hook     uintptr // HHOOK установленного хука
	threadID uint32  // поток хука с его циклом сообщений
}

func newHotkeyHook(handlers map[uint32]func(), onToggle func(disabled bool)) *hotkeyHook {
	return &hotkeyHook{handlers: handlers, onToggle: onToggle}
}

// Start устанавливает хук. Хук живёт в отдельном закреплённом OS-потоке со
// своим циклом сообщений: без цикла сообщений в потоке, который поставил хук,
// Windows не вызывает процедуру хука.
func (h *hotkeyHook) Start() error {
	hInst, _, errno := procGetModuleHandleW.Call(0)
	if hInst == 0 {
		return win32Error("GetModuleHandleW", errno)
	}

	ready := make(chan error, 1)
	go func() {
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		// PeekMessage создаёт очередь сообщений потока заранее: иначе Stop,
		// вызванный сразу после установки хука, не смог бы отправить ей
		// WM_QUIT (PostThreadMessage требует существующей очереди).
		const pmNoRemove = 0
		var msg [48]byte
		procPeekMessageW.Call(uintptr(unsafe.Pointer(&msg[0])), 0, 0, 0, pmNoRemove)

		// Колбэки syscall.NewCallback никогда не освобождаются, так что
		// хранить ссылку на cb, чтобы его не собрал GC, не нужно.
		cb := syscall.NewCallback(h.proc)
		hook, _, errno := procSetWindowsHookExW.Call(whKeyboardLL, cb, hInst, 0)
		if hook == 0 {
			ready <- win32Error("SetWindowsHookExW", errno)
			return
		}
		h.hook, h.threadID = hook, windows.GetCurrentThreadId()
		ready <- nil

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
	return <-ready
}

// Stop снимает хук и завершает цикл сообщений его потока.
func (h *hotkeyHook) Stop() error {
	const wmQuit = 0x0012
	if r, _, errno := procUnhookWindowsHookEx.Call(h.hook); r == 0 {
		return win32Error("UnhookWindowsHookEx", errno)
	}
	if r, _, errno := procPostThreadMessageW.Call(uintptr(h.threadID), wmQuit, 0, 0); r == 0 {
		return win32Error("PostThreadMessageW", errno)
	}
	return nil
}

// proc — процедура хука WH_KEYBOARD_LL. Наши клавиши без модификаторов
// «съедаются» (возврат 1), обработчик запускается в отдельной горутине,
// чтобы хук возвращался быстро; остальные, включая наши клавиши с
// Alt/Ctrl/Shift/Win, передаются дальше по цепочке хуков.
//
// lParam хука — указатель на KBDLLHOOKSTRUCT в памяти Windows;
// syscall.NewCallback сразу отдаёт его типизированным указателем, без
// небезопасного преобразования uintptr -> unsafe.Pointer.
//
// CallNextHookEx возвращает результат следующего хука в цепочке, а не
// признак ошибки, поэтому третье значение Call здесь не нужно.
//
// Ctrl+F2 перехватывается всегда и включает/выключает остальные хоткеи;
// выключенные хоткеи уходят программам как обычные клавиши.
func (h *hotkeyHook) proc(nCode int, wParam uintptr, kb *kbdllHookStruct) uintptr {
	if nCode >= 0 && kb.VkCode == vkToggleHotkeys && (wParam == wmKeyUp || wParam == wmSysKeyUp) {
		h.toggleKeyDown = false
	}
	if nCode >= 0 && (wParam == wmKeyDown || wParam == wmSysKeyDown) {
		if toggleComboPressed(kb) {
			// Автоповтор зажатого Ctrl+F2 не переключает хоткеи туда-обратно:
			// повторное переключение — только после отпускания F2.
			if !h.toggleKeyDown {
				h.toggleKeyDown = true
				disabled := !h.disabled.Load()
				h.disabled.Store(disabled)
				go h.onToggle(disabled)
			}
			return 1
		}
		if handler := h.handlers[kb.VkCode]; handler != nil && !h.disabled.Load() && !modifierHeld(kb) {
			go handler()
			return 1
		}
	}
	r, _, _ := procCallNextHookEx.Call(0, uintptr(nCode), wParam, uintptr(unsafe.Pointer(kb)))
	return r
}
