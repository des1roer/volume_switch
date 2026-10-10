package main

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"image/png"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
	"unsafe"

	"fyne.io/fyne/v2"
	"fyne.io/fyne/v2/driver/desktop"
	"fyne.io/fyne/v2/test"
	"github.com/diegosz/go-wca/pkg/wca"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/registry"
)

const sampleVariablesHTML = `<html><body>
<p id="file">movie.mkv</p>
<p id="filepath">E:\movies\movie.mkv</p>
<p id="statestring">Воспроизведение</p>
<p id="position">61000</p>
<p id="positionstring">00:01:01</p>
<p id="duration">3600000</p>
<p id="durationstring">01:00:00</p>
</body></html>`

func TestExtractInt(t *testing.T) {
	tests := []struct {
		name    string
		html    string
		want    int64
		wantErr bool
	}{
		{"число", `<p id="position">42</p>`, 42, false},
		{"отрицательное", `<p id="position">-7</p>`, -7, false},
		{"пробелы вокруг", `<p id="position"> 42 </p>`, 42, false},
		{"поля нет", `<p id="duration">5</p>`, 0, false},
		{"пустое поле", `<p id="position"></p>`, 0, false},
		{"не число", `<p id="position">abc</p>`, 0, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := extractInt(tt.html, rePosition)
			if (err != nil) != tt.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tt.wantErr)
			}
			if got != tt.want {
				t.Errorf("got %d, want %d", got, tt.want)
			}
		})
	}
}

func TestExtractStr(t *testing.T) {
	if got := extractStr(sampleVariablesHTML, reFile); got != "movie.mkv" {
		t.Errorf("file = %q", got)
	}
	if got := extractStr(sampleVariablesHTML, reFilePath); got != `E:\movies\movie.mkv` {
		t.Errorf("filepath = %q", got)
	}
	if got := extractStr("<html></html>", reFile); got != "" {
		t.Errorf("отсутствующее поле = %q, want пусто", got)
	}
}

func TestFetchState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(sampleVariablesHTML))
	}))
	defer srv.Close()

	st, err := fetchState(srv.URL, srv.Client())
	if err != nil {
		t.Fatalf("fetchState: %v", err)
	}
	want := MPCState{
		Position:    61000,
		Duration:    3600000,
		PositionStr: "00:01:01",
		StateString: "Воспроизведение",
		File:        "movie.mkv",
		FilePath:    `E:\movies\movie.mkv`,
	}
	if *st != want {
		t.Errorf("got %+v\nwant %+v", *st, want)
	}
}

func TestFetchStateErrors(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		errPart string
	}{
		{"HTTP-ошибка", http.StatusInternalServerError, "", "HTTP"},
		{"битая позиция", http.StatusOK, `<p id="position">x</p>`, "position"},
		{"битая длительность", http.StatusOK, `<p id="duration">x</p>`, "duration"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tt.status)
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()

			_, err := fetchState(srv.URL, srv.Client())
			if err == nil || !strings.Contains(err.Error(), tt.errPart) {
				t.Errorf("err = %v, want содержащую %q", err, tt.errPart)
			}
		})
	}
}

func TestFetchStateConnectionRefused(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()

	client := &http.Client{Timeout: time.Second}
	if _, err := fetchState(url, client); err == nil {
		t.Error("ожидалась ошибка соединения")
	}
}

func TestFormatTime(t *testing.T) {
	tests := []struct {
		ms   int64
		want string
	}{
		{-5000, "00:00"},
		{0, "00:00"},
		{999, "00:00"},
		{59_999, "00:59"},
		{61_000, "01:01"},
		{3_599_000, "59:59"},
		{3_600_000, "1:00:00"},
		{3_661_000, "1:01:01"},
		{9_423_000, "2:37:03"},
	}
	for _, tt := range tests {
		if got := formatTime(tt.ms); got != tt.want {
			t.Errorf("formatTime(%d) = %q, want %q", tt.ms, got, tt.want)
		}
	}
}

func TestMPCCommandURL(t *testing.T) {
	tests := []struct {
		in, want string
	}{
		{"http://localhost:7777/variables.html", "http://localhost:7777/command.html?wm_command=889"},
		{"  http://localhost:7777/variables.html  ", "http://localhost:7777/command.html?wm_command=889"},
		{"http://localhost:7777/variables.html?x=1#top", "http://localhost:7777/command.html?wm_command=889"},
		{"http://host/sub/dir/variables.html", "http://host/sub/dir/command.html?wm_command=889"},
		{"http://localhost:7777/", "http://localhost:7777/command.html?wm_command=889"},
		{"", ""},
		{"   ", ""},
		{"variables.html", "variables.html/command.html?wm_command=889"},
	}
	for _, tt := range tests {
		if got := mpcCommandURL(tt.in, mpcPlayPauseCommand); got != tt.want {
			t.Errorf("mpcCommandURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestToggleMPCPlaybackSendsCommand(t *testing.T) {
	got := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		got <- r.URL.RequestURI()
	}))
	defer srv.Close()

	toggleMPCPlayback(srv.URL + "/variables.html")

	select {
	case uri := <-got:
		if want := "/command.html?wm_command=889"; uri != want {
			t.Errorf("запрос %q, want %q", uri, want)
		}
	default:
		t.Fatal("команда не отправлена")
	}
}

func TestWrapIndex(t *testing.T) {
	tests := []struct {
		i, n, want int
	}{
		{0, 3, 0},
		{2, 3, 2},
		{3, 3, 0},
		{-1, 3, 2},
		{-4, 3, 2},
		{7, 3, 1},
		{-1, 1, 0},
	}
	for _, tt := range tests {
		if got := wrapIndex(tt.i, tt.n); got != tt.want {
			t.Errorf("wrapIndex(%d, %d) = %d, want %d", tt.i, tt.n, got, tt.want)
		}
	}
}

func TestCopyUTF16(t *testing.T) {
	t.Run("помещается", func(t *testing.T) {
		var dst [8]uint16
		copyUTF16(dst[:], "abc")
		if got := windows.UTF16ToString(dst[:]); got != "abc" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("обрезается с завершающим нулём", func(t *testing.T) {
		var dst [5]uint16
		copyUTF16(dst[:], "hello world")
		if got := windows.UTF16ToString(dst[:]); got != "hell" {
			t.Errorf("got %q, want %q", got, "hell")
		}
		if dst[len(dst)-1] != 0 {
			t.Error("последний элемент не ноль")
		}
	})
	t.Run("кириллица", func(t *testing.T) {
		var dst [16]uint16
		copyUTF16(dst[:], "Динамики")
		if got := windows.UTF16ToString(dst[:]); got != "Динамики" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("NUL в строке — буфер не трогается", func(t *testing.T) {
		var dst [8]uint16
		copyUTF16(dst[:], "a\x00b")
		if dst != [8]uint16{} {
			t.Errorf("буфер изменён: %v", dst)
		}
	})
}

func TestWrapICO(t *testing.T) {
	payload := []byte("PNGDATA")
	ico := wrapICO(payload, 32, 16)

	if len(ico) != 22+len(payload) {
		t.Fatalf("длина %d, want %d", len(ico), 22+len(payload))
	}
	le := binary.LittleEndian
	if le.Uint16(ico[0:]) != 0 || le.Uint16(ico[2:]) != 1 || le.Uint16(ico[4:]) != 1 {
		t.Errorf("неверный ICONDIR: % x", ico[:6])
	}
	if ico[6] != 32 || ico[7] != 16 {
		t.Errorf("размер %dx%d, want 32x16", ico[6], ico[7])
	}
	if got := le.Uint32(ico[14:]); got != uint32(len(payload)) {
		t.Errorf("размер данных %d, want %d", got, len(payload))
	}
	if got := le.Uint32(ico[18:]); got != 22 {
		t.Errorf("смещение данных %d, want 22", got)
	}
	if !bytes.Equal(ico[22:], payload) {
		t.Error("данные изображения повреждены")
	}
}

func TestRenderTrayIcon(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(trayIconData[22:]))
	if err != nil {
		t.Fatalf("PNG внутри иконки не декодируется: %v", err)
	}
	if b := img.Bounds(); b.Dx() != 32 || b.Dy() != 32 {
		t.Errorf("размер %v, want 32x32", b)
	}
	// Центр — цвет круга, угол — фон.
	if r, g, b, _ := img.At(16, 16).RGBA(); r>>8 != 90 || g>>8 != 200 || b>>8 != 250 {
		t.Errorf("центр (%d,%d,%d), want (90,200,250)", r>>8, g>>8, b>>8)
	}
	if r, g, b, _ := img.At(0, 0).RGBA(); r>>8 != 30 || g>>8 != 30 || b>>8 != 35 {
		t.Errorf("угол (%d,%d,%d), want (30,30,35)", r>>8, g>>8, b>>8)
	}
}

func TestLoadDotEnv(t *testing.T) {
	const (
		keyPlain    = "VS_TEST_PLAIN"
		keyQuoted   = "VS_TEST_QUOTED"
		keySingle   = "VS_TEST_SINGLE"
		keyEquals   = "VS_TEST_EQUALS"
		keyExisting = "VS_TEST_EXISTING"
		keyComment  = "VS_TEST_COMMENTED"
	)
	for _, k := range []string{keyPlain, keyQuoted, keySingle, keyEquals, keyComment} {
		if _, ok := os.LookupEnv(k); ok {
			t.Fatalf("%s уже задан в окружении", k)
		}
		t.Cleanup(func() { _ = os.Unsetenv(k) })
	}
	t.Setenv(keyExisting, "outer")

	content := strings.Join([]string{
		"# комментарий",
		"#" + keyComment + "=nope",
		"",
		"  " + keyPlain + " = value  ",
		keyQuoted + `="C:\Program Files\x.exe"`,
		keySingle + "='single'",
		keyEquals + "=a=b",
		"=пустой ключ",
		keyExisting + "=inner",
		"строка без знака равенства",
	}, "\r\n")
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	loadDotEnv(path)

	want := map[string]string{
		keyPlain:    "value",
		keyQuoted:   `C:\Program Files\x.exe`,
		keySingle:   "single",
		keyEquals:   "a=b",
		keyExisting: "outer",
	}
	for k, v := range want {
		if got := os.Getenv(k); got != v {
			t.Errorf("%s = %q, want %q", k, got, v)
		}
	}
	if _, ok := os.LookupEnv(keyComment); ok {
		t.Errorf("%s из закомментированной строки не должен задаваться", keyComment)
	}
}

func TestLoadDotEnvMissingFile(t *testing.T) {
	// Отсутствующий файл — штатная ситуация: просто ничего не загружается.
	loadDotEnv(filepath.Join(t.TempDir(), "nope.env"))
}

func TestMPCExePathFromEnv(t *testing.T) {
	const exe = `D:\Tools\mpc-hc64.exe`
	t.Setenv("MPC_EXE", exe)
	if got := mpcExePath(); got != exe {
		t.Errorf("mpcExePath() = %q, want %q", got, exe)
	}
}

func TestHookProc(t *testing.T) {
	called := make(chan uint32, 1)
	prev := hotkeys
	hotkeys = map[uint32]func(){
		vkF5: func() { called <- vkF5 },
		vkF8: func() { called <- vkF8 },
	}
	t.Cleanup(func() { hotkeys = prev })

	for _, vk := range []uint32{vkF5, vkF8} {
		if r := hookProc(0, wmKeyDown, &kbdllHookStruct{VkCode: vk}); r != 1 {
			t.Errorf("vk=%#x: hookProc = %d, want 1 (клавиша съедена)", vk, r)
		}
		select {
		case got := <-called:
			if got != vk {
				t.Errorf("вызван обработчик %#x, want %#x", got, vk)
			}
		case <-time.After(time.Second):
			t.Fatalf("обработчик %#x не вызван", vk)
		}
	}

	// Чужая клавиша, отпускание нашей клавиши и nCode < 0 уходят дальше по
	// цепочке хуков (CallNextHookEx без хуков возвращает 0) и обработчики не
	// вызывают.
	for _, tc := range []struct {
		name   string
		nCode  int
		wParam uintptr
		vk     uint32
	}{
		{"чужая клавиша", 0, wmKeyDown, 0x41},
		{"отпускание F5", 0, 0x0101, vkF5}, // WM_KEYUP
		{"nCode < 0", -1, wmKeyDown, vkF5},
	} {
		if r := hookProc(tc.nCode, tc.wParam, &kbdllHookStruct{VkCode: tc.vk}); r != 0 {
			t.Errorf("%s: hookProc = %d, want 0", tc.name, r)
		}
	}
	select {
	case got := <-called:
		t.Errorf("лишний вызов обработчика %#x", got)
	case <-time.After(50 * time.Millisecond):
	}
}

func TestVolumeChanged(t *testing.T) {
	base := volumeReading{Level: 0.50, DeviceID: "dev1"}
	tests := []struct {
		name      string
		prev, cur volumeReading
		want      bool
	}{
		{"первый замер", volumeReading{}, base, false},
		{"без изменений", base, base, false},
		{"меньше процента", base, volumeReading{Level: 0.502, DeviceID: "dev1"}, false},
		{"громкость изменилась", base, volumeReading{Level: 0.60, DeviceID: "dev1"}, true},
		{"mute", base, volumeReading{Level: 0.50, Muted: true, DeviceID: "dev1"}, true},
		{"сменилось устройство", base, volumeReading{Level: 0.80, DeviceID: "dev2"}, false},
	}
	for _, tt := range tests {
		if got := volumeChanged(tt.prev, tt.cur); got != tt.want {
			t.Errorf("%s: got %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestAutostartCommand(t *testing.T) {
	cmd, err := autostartCommand()
	if err != nil {
		t.Fatal(err)
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if want := `"` + exe + `"`; cmd != want {
		t.Errorf("got %q, want %q", cmd, want)
	}
}

// ---------------------------------------------------------------------------
// Хелперы для тестов, работающих с системой. Все они либо подменяют точки
// входа во внешний мир (запуск процессов, ключи реестра, заголовки окон),
// либо создают собственные временные объекты и удаляют их после теста —
// настройки пользователя и окна запущенной копии программы не трогаются.
// ---------------------------------------------------------------------------

// setVar подменяет значение переменной на время теста.
func setVar[T any](t *testing.T, p *T, v T) {
	t.Helper()
	old := *p
	*p = v
	t.Cleanup(func() { *p = old })
}

// fakeStart подменяет startDetached: команды не запускаются, а
// записываются; запуск завершается ошибкой err.
func fakeStart(t *testing.T, err error) *[]*exec.Cmd {
	t.Helper()
	var started []*exec.Cmd
	setVar(t, &startDetached, func(cmd *exec.Cmd) error {
		started = append(started, cmd)
		return err
	})
	return &started
}

// tempRegistryKey создаёт пустой временный ключ в HKCU и удаляет его после
// теста.
func tempRegistryKey(t *testing.T) string {
	t.Helper()
	path := fmt.Sprintf(`Software\volume_switch_test_%d_%s`, os.Getpid(), t.Name())
	k, _, err := registry.CreateKey(registry.CURRENT_USER, path, registry.ALL_ACCESS)
	if err != nil {
		t.Fatalf("создание %s: %v", path, err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.DeleteKey(registry.CURRENT_USER, path); err != nil {
			t.Errorf("удаление %s: %v", path, err)
		}
	})
	return path
}

func setRegistryString(t *testing.T, path, name, value string) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetStringValue(name, value); err != nil {
		t.Fatal(err)
	}
}

var (
	procCreateWindowExW = user32.NewProc("CreateWindowExW")
	procDestroyWindow   = user32.NewProc("DestroyWindow")
)

// newNativeWindow создаёт скрытое окно стандартного класса STATIC с заданным
// заголовком и расширенным стилем (класс STATIC не нужно регистрировать) и
// уничтожает его после теста. Окно принадлежит потоку теста, поэтому поток
// закрепляется. Окна GLFW создаются с WS_EX_APPWINDOW (wsExAppWindow) —
// fixOverlayWindowChrome должна его снять.
func newNativeWindow(t *testing.T, title string, ex uintptr) uintptr {
	t.Helper()
	runtime.LockOSThread()
	t.Cleanup(runtime.UnlockOSThread)

	const wsPopup = 0x80000000
	cls, _ := windows.UTF16PtrFromString("STATIC")
	ttl, _ := windows.UTF16PtrFromString(title)
	hwnd, _, errno := procCreateWindowExW.Call(ex, uintptr(unsafe.Pointer(cls)), uintptr(unsafe.Pointer(ttl)),
		wsPopup, 0, 0, 10, 10, 0, 0, 0, 0)
	if hwnd == 0 {
		t.Fatalf("CreateWindowExW: %v", errno)
	}
	t.Cleanup(func() { procDestroyWindow.Call(hwnd) })
	return hwnd
}

// wsExAppWindow — WS_EX_APPWINDOW, с которым GLFW создаёт свои окна.
const wsExAppWindow = 0x00040000

func exStyle(hwnd uintptr) uintptr {
	var gwlExStyle int32 = -20
	s, _, _ := procGetWindowLongPtrW.Call(hwnd, uintptr(gwlExStyle))
	return s
}

// uniqueTitle — заголовок окна, которого точно нет у запущенной копии
// программы.
func uniqueTitle(t *testing.T, base string) string {
	return fmt.Sprintf("%s [test %d %s]", base, os.Getpid(), t.Name())
}

// setupUI собирает оба окна на тестовом драйвере Fyne (без настоящих окон) с
// уникальными заголовками.
func setupUI(t *testing.T, mpcURL string) {
	t.Helper()
	setVar(t, &fyneApp, test.NewTempApp(t))
	setVar(t, &windowTitle, uniqueTitle(t, windowTitle))
	setVar(t, &volumeWindowTitle, uniqueTitle(t, volumeWindowTitle))
	setVar(t, &winVisible, false)
	setVar(t, &currentFilePath, "")
	buildUI(mpcURL)
}

// ---------------------------------------------------------------------------
// Win32-хелперы
// ---------------------------------------------------------------------------

func TestWin32Error(t *testing.T) {
	if got := win32Error("F", syscall.Errno(0)).Error(); got != "F: неизвестная ошибка" {
		t.Errorf("errno 0: %q", got)
	}
	err := win32Error("F", windows.ERROR_ACCESS_DENIED)
	if !errors.Is(err, windows.ERROR_ACCESS_DENIED) || !strings.HasPrefix(err.Error(), "F: ") {
		t.Errorf("errno 5: %v", err)
	}
}

func TestScreenLayout(t *testing.T) {
	work, monitor, err := screenLayout()
	if err != nil {
		// Без интерактивного рабочего стола (CI) курсора может не быть.
		t.Skipf("screenLayout: %v", err)
	}
	if work.Right <= work.Left || monitor.Right <= monitor.Left {
		t.Errorf("пустые границы: work=%+v monitor=%+v", work, monitor)
	}
	if work.Left < monitor.Left || work.Right > monitor.Right {
		t.Errorf("рабочая область %+v выходит за монитор %+v", work, monitor)
	}
}

func TestTitleBarHeight(t *testing.T) {
	if h := titleBarHeight(); h <= 0 || h > 200 {
		t.Errorf("titleBarHeight() = %d", h)
	}
}

func TestFixOverlayWindowChrome(t *testing.T) {
	const (
		wsExToolWindow = 0x80
		wsExTopmost    = 0x8
	)
	title := uniqueTitle(t, "chrome")
	hwnd := newNativeWindow(t, title, wsExAppWindow)

	if err := fixOverlayWindowChrome(title, false); err != nil {
		t.Fatalf("topmost=false: %v", err)
	}
	if s := exStyle(hwnd); s&wsExToolWindow == 0 || s&(wsExTopmost|wsExAppWindow) != 0 {
		t.Errorf("topmost=false: exStyle=%#x, want TOOLWINDOW без TOPMOST и APPWINDOW", s)
	}

	if err := fixOverlayWindowChrome(title, true); err != nil {
		t.Fatalf("topmost=true: %v", err)
	}
	if s := exStyle(hwnd); s&wsExTopmost == 0 {
		t.Errorf("topmost=true: exStyle=%#x, want TOPMOST", s)
	}
}

func TestFixOverlayWindowChromeErrors(t *testing.T) {
	if err := fixOverlayWindowChrome(uniqueTitle(t, "нет такого окна"), false); err == nil {
		t.Error("для несуществующего окна ожидалась ошибка")
	}
	if err := fixOverlayWindowChrome("bad\x00title", false); err == nil {
		t.Error("для заголовка с NUL ожидалась ошибка")
	}
}

func TestFindOwnTrayWindowAndNotification(t *testing.T) {
	setVar(t, &systrayClassName, uniqueTitle(t, "NoSuchClass"))
	if hwnd, err := findOwnTrayWindow(); err != nil || hwnd != 0 {
		t.Fatalf("без окна трея: hwnd=%v err=%v", hwnd, err)
	}
	showNotification("t", "окна трея нет — только запись в лог")

	// Окно класса STATIC нашего процесса изображает окно systray.
	hwnd := newNativeWindow(t, uniqueTitle(t, "tray"), 0)
	systrayClassName = "Static"
	got, err := findOwnTrayWindow()
	if err != nil || got != windows.Handle(hwnd) {
		t.Fatalf("findOwnTrayWindow = %v, %v; want %#x", got, err, hwnd)
	}
	// Иконки трея у этого окна нет, поэтому Shell_NotifyIcon вернёт ошибку,
	// которая уйдёт в лог; ничего не показывается.
	showNotification("t", "b")
}

func TestAcquireSingleInstance(t *testing.T) {
	setVar(t, &singleInstanceMutex, fmt.Sprintf(`Local\volume_switch_test_%d`, os.Getpid()))

	release, already, err := acquireSingleInstance()
	if err != nil || already {
		t.Fatalf("первый запуск: already=%v err=%v", already, err)
	}
	if _, already, err := acquireSingleInstance(); err != nil || !already {
		t.Errorf("второй запуск: already=%v err=%v, want already", already, err)
	}
	release()
	release2, already, err := acquireSingleInstance()
	if err != nil || already {
		t.Fatalf("после release: already=%v err=%v", already, err)
	}
	release2()
}

func TestAcquireSingleInstanceErrors(t *testing.T) {
	setVar(t, &singleInstanceMutex, "bad\x00name")
	if _, _, err := acquireSingleInstance(); err == nil {
		t.Error("имя с NUL: ожидалась ошибка")
	}
	singleInstanceMutex = `NoSuchNamespace\volume_switch_test`
	if _, _, err := acquireSingleInstance(); err == nil {
		t.Error("несуществующее пространство имён: ожидалась ошибка")
	}
}

// ---------------------------------------------------------------------------
// Реестр: автозапуск и путь к MPC-HC
// ---------------------------------------------------------------------------

func TestAutostart(t *testing.T) {
	setVar(t, &autostartRegistryPath, tempRegistryKey(t))

	if isAutostartEnabled() {
		t.Error("пустой ключ: автозапуск включён")
	}
	if err := setAutostart(true); err != nil {
		t.Fatalf("setAutostart(true): %v", err)
	}
	if !isAutostartEnabled() {
		t.Error("после включения автозапуск выключен")
	}

	setRegistryString(t, autostartRegistryPath, autostartValueName, `"C:\old\volume_switch.exe"`)
	if isAutostartEnabled() {
		t.Error("значение указывает на другой exe, а автозапуск считается включённым")
	}

	if err := setAutostart(false); err != nil {
		t.Fatalf("setAutostart(false): %v", err)
	}
	if isAutostartEnabled() {
		t.Error("после выключения автозапуск включён")
	}
	if err := setAutostart(false); err != nil {
		t.Errorf("повторное выключение: %v", err)
	}
}

func TestAutostartMissingKey(t *testing.T) {
	setVar(t, &autostartRegistryPath, `Software\volume_switch_test_no_such_key`)
	if isAutostartEnabled() {
		t.Error("нет ключа: автозапуск включён")
	}
	if err := setAutostart(true); err == nil {
		t.Error("нет ключа: ожидалась ошибка")
	}
}

func TestMPCExeFromRegistry(t *testing.T) {
	setVar(t, &mpcRegistryPath, `Software\volume_switch_test_no_such_key`)
	if got := mpcExeFromRegistry(); got != "" {
		t.Errorf("нет ключа: %q", got)
	}

	mpcRegistryPath = tempRegistryKey(t)
	if got := mpcExeFromRegistry(); got != "" {
		t.Errorf("нет значения: %q", got)
	}
	setRegistryString(t, mpcRegistryPath, "ExePath", `D:\MPC\mpc-hc64.exe`)
	if got := mpcExeFromRegistry(); got != `D:\MPC\mpc-hc64.exe` {
		t.Errorf("ExePath = %q", got)
	}
}

func TestMPCExePathFallbacks(t *testing.T) {
	t.Setenv("MPC_EXE", "")
	existing := filepath.Join(t.TempDir(), "mpc-hc64.exe")
	if err := os.WriteFile(existing, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	missing := filepath.Join(t.TempDir(), "missing.exe")
	setVar(t, &mpcRegistryPath, tempRegistryKey(t))

	setVar(t, &defaultMPCExe, existing)
	if got := mpcExePath(); got != existing {
		t.Errorf("файл по умолчанию есть: %q", got)
	}

	defaultMPCExe = missing
	if got := mpcExePath(); got != missing {
		t.Errorf("ни файла, ни реестра: %q, want путь по умолчанию", got)
	}

	// Stat падает не с «нет файла» — это логируется, дальше реестр.
	defaultMPCExe = "bad\x00path"
	setRegistryString(t, mpcRegistryPath, "ExePath", `D:\MPC\mpc-hc64.exe`)
	if got := mpcExePath(); got != `D:\MPC\mpc-hc64.exe` {
		t.Errorf("из реестра: %q", got)
	}
}

// ---------------------------------------------------------------------------
// Запуск внешних процессов
// ---------------------------------------------------------------------------

func TestStartDetached(t *testing.T) {
	cmd := exec.Command(filepath.Join(os.Getenv("SystemRoot"), "System32", "cmd.exe"), "/c", "exit", "0")
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: windows.CREATE_NO_WINDOW}
	if err := startDetached(cmd); err != nil {
		t.Errorf("startDetached: %v", err)
	}
	if err := startDetached(exec.Command(filepath.Join(t.TempDir(), "missing.exe"))); err == nil {
		t.Error("несуществующий exe: ожидалась ошибка")
	}
}

func TestLaunchMPC(t *testing.T) {
	const exe = `D:\Tools\MPC\mpc-hc64.exe`
	t.Setenv("MPC_EXE", exe)

	started := fakeStart(t, nil)
	launchMPC()
	if len(*started) != 1 {
		t.Fatalf("запущено %d процессов, want 1", len(*started))
	}
	if cmd := (*started)[0]; cmd.Path != exe || cmd.Dir != `D:\Tools\MPC` {
		t.Errorf("Path=%q Dir=%q", cmd.Path, cmd.Dir)
	}

	fakeStart(t, errors.New("boom"))
	launchMPC() // ошибка только логируется
}

func TestOpenContainingFolder(t *testing.T) {
	started := fakeStart(t, nil)

	openContainingFolder("")
	if len(*started) != 0 {
		t.Fatal("для пустого пути проводник запускаться не должен")
	}

	openContainingFolder(`E:\Фильмы\Some Movie\.\movie.mkv`)
	if len(*started) != 1 {
		t.Fatalf("запущено %d процессов, want 1", len(*started))
	}
	want := `explorer.exe /select,"E:\Фильмы\Some Movie\movie.mkv"`
	if got := (*started)[0].SysProcAttr.CmdLine; got != want {
		t.Errorf("CmdLine = %q, want %q", got, want)
	}

	fakeStart(t, errors.New("boom"))
	openContainingFolder(`C:\x.mkv`) // ошибка только логируется
}

func TestToggleMPCPlaybackFallbacks(t *testing.T) {
	started := fakeStart(t, nil)
	t.Setenv("MPC_EXE", `D:\MPC\mpc-hc64.exe`)

	toggleMPCPlayback("")
	if len(*started) != 0 {
		t.Fatal("без URL ничего запускаться не должно")
	}

	srv := httptest.NewServer(http.NotFoundHandler())
	srv.Close()
	toggleMPCPlayback(srv.URL + "/variables.html")
	if len(*started) != 1 || (*started)[0].Path != `D:\MPC\mpc-hc64.exe` {
		t.Fatalf("MPC-HC недоступен — ожидался запуск MPC-HC, запущено: %v", *started)
	}

	rejected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer rejected.Close()
	toggleMPCPlayback(rejected.URL + "/variables.html")
	if len(*started) != 1 {
		t.Error("отказ web-интерфейса не должен запускать MPC-HC")
	}
}

// ---------------------------------------------------------------------------
// Опрос громкости, меню трея, хук клавиатуры
// ---------------------------------------------------------------------------

func TestPollVolume(t *testing.T) {
	type step struct {
		r   volumeReading
		err error
	}
	steps := []step{
		{r: volumeReading{Level: 0.5, DeviceID: "a"}},              // база
		{err: errors.New("COM")},                                   // пропуск
		{r: volumeReading{Level: 0.5, DeviceID: "a"}},              // без изменений
		{r: volumeReading{Level: 0.7, DeviceID: "a"}},              // изменение
		{r: volumeReading{Level: 0.2, DeviceID: "b"}},              // смена устройства
		{r: volumeReading{Level: 0.2, Muted: true, DeviceID: "b"}}, // mute
	}
	ticks := make(chan time.Time, len(steps))
	for range steps {
		ticks <- time.Now()
	}
	close(ticks)

	i := 0
	read := func() (volumeReading, error) {
		s := steps[i]
		i++
		return s.r, s.err
	}
	type call struct {
		level float64
		muted bool
	}
	var calls []call
	pollVolume(read, func(level float64, muted bool) { calls = append(calls, call{level, muted}) }, ticks)

	want := []call{{0.7, false}, {0.2, true}}
	if fmt.Sprint(calls) != fmt.Sprint(want) {
		t.Errorf("onChange вызван с %v, want %v", calls, want)
	}
}

type fakeCheckbox struct{ checked bool }

func (c *fakeCheckbox) Check()   { c.checked = true }
func (c *fakeCheckbox) Uncheck() { c.checked = false }

func TestSetChecked(t *testing.T) {
	a, b := &fakeCheckbox{checked: true}, &fakeCheckbox{}
	setVar(t, &deviceItems, map[string]checkbox{"a": a, "b": b})

	setChecked("b")
	if a.checked || !b.checked {
		t.Errorf("после выбора b: a=%v b=%v", a.checked, b.checked)
	}
	setChecked("нет такого")
	if a.checked || b.checked {
		t.Errorf("неизвестный ID: a=%v b=%v, want обе сняты", a.checked, b.checked)
	}
}

func TestStartHotkeys(t *testing.T) {
	setVar(t, &hotkeys, nil)
	// Пустая карта: пока хук стоит, все клавиши проходят дальше как обычно.
	stop, err := startHotkeys(map[uint32]func(){})
	if err != nil {
		t.Fatalf("startHotkeys: %v", err)
	}
	if hotkeys == nil {
		t.Error("карта обработчиков не установлена")
	}
	if err := stop(); err != nil {
		t.Errorf("stop: %v", err)
	}
	if err := stop(); err == nil {
		t.Error("повторное снятие уже снятого хука: ожидалась ошибка")
	}
}

// ---------------------------------------------------------------------------
// Окна Fyne (тестовый драйвер, без настоящих окон)
// ---------------------------------------------------------------------------

func TestBuildUI(t *testing.T) {
	setupUI(t, defaultURL)
	if mainWindow == nil || volumeWindow == nil {
		t.Fatal("окна не созданы")
	}
	if mainWindow.Title() != windowTitle || volumeWindow.Title() != volumeWindowTitle {
		t.Errorf("заголовки %q, %q", mainWindow.Title(), volumeWindow.Title())
	}
	if len(volumeTicks) != volumeTickCount+1 || volumeTickLabels[volumeTickCount].Text != "100" {
		t.Errorf("шкала громкости: %d отметок", len(volumeTicks))
	}
	if got := progressBar.TextFormatter(); got != "Ожидание подключения к MPC-HC..." {
		t.Errorf("начальный текст %q", got)
	}
}

func TestToggleMPCWindow(t *testing.T) {
	setupUI(t, defaultURL)
	toggleMPCWindow()
	if !winVisible {
		t.Error("после первого F5 окно должно быть показано")
	}
	toggleMPCWindow()
	if winVisible {
		t.Error("после второго F5 окно должно быть скрыто")
	}

	setVar(t, &mainWindow, nil)
	toggleMPCWindow() // окно ещё не создано — только запись в лог
	if winVisible {
		t.Error("без окна состояние меняться не должно")
	}
}

func TestShowProgress(t *testing.T) {
	setupUI(t, defaultURL)

	showProgress(&MPCState{Position: 61_000, Duration: 3_600_000, PositionStr: "00:01:01",
		StateString: "Пауза", File: "movie.mkv", FilePath: `E:\movie.mkv`})
	if elapsedLabel.Text != "00:01:01" || remainingLabel.Text != "-58:59" {
		t.Errorf("подписи %q / %q", elapsedLabel.Text, remainingLabel.Text)
	}
	if want := "movie.mkv   —   1.7%  Пауза  (из 1:00:00)"; progressText != want {
		t.Errorf("текст %q, want %q", progressText, want)
	}
	if currentFilePath != `E:\movie.mkv` {
		t.Errorf("currentFilePath = %q", currentFilePath)
	}

	// Без positionstring время считается само; позиция за концом обрезается.
	showProgress(&MPCState{Position: 5_000_000, Duration: 3_600_000})
	if elapsedLabel.Text != "1:23:20" || progressBar.Value != 1 {
		t.Errorf("elapsed=%q value=%v", elapsedLabel.Text, progressBar.Value)
	}

	showProgress(&MPCState{})
	if progressText != "Нет активного воспроизведения" || progressBar.Value != 0 ||
		elapsedLabel.Text != "00:00" || remainingLabel.Text != "00:00" {
		t.Errorf("без файла: %q value=%v %q %q", progressText, progressBar.Value, elapsedLabel.Text, remainingLabel.Text)
	}
}

// ticksN возвращает закрытый канал с n тиками: цикл опроса сделает n
// итераций и завершится.
func ticksN(n int) <-chan time.Time {
	ticks := make(chan time.Time, n)
	for range n {
		ticks <- time.Now()
	}
	close(ticks)
	return ticks
}

func TestPollMPC(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) <= 2 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(sampleVariablesHTML))
	}))
	defer srv.Close()
	setupUI(t, srv.URL)

	// Первый ответ — ошибка.
	pollMPC(srv.URL, ticksN(1))
	if !strings.HasPrefix(progressText, "⚠ Нет связи с MPC-HC: HTTP 503") {
		t.Errorf("при ошибке текст %q", progressText)
	}

	// Ещё одна такая же ошибка (в лог не дублируется), затем связь
	// восстанавливается.
	pollMPC(srv.URL, ticksN(2))
	if requests.Load() != 3 {
		t.Fatalf("запросов %d, want 3", requests.Load())
	}
	if !strings.HasPrefix(progressText, "movie.mkv") {
		t.Errorf("после восстановления связи текст %q", progressText)
	}
}

// hideSpy сообщает в канал, когда окно спрятали. В тестовом драйвере Fyne
// fyne.Do выполняется прямо в горутине таймера скрытия, и канал даёт тесту
// синхронизацию с ней (иначе -race видит гонку с следующим тестом).
type hideSpy struct {
	fyne.Window
	hidden chan struct{}
}

func (w *hideSpy) Hide() {
	w.Window.Hide()
	select {
	case w.hidden <- struct{}{}:
	default:
	}
}

func TestShowVolumeOverlay(t *testing.T) {
	setupUI(t, defaultURL)
	hidden := make(chan struct{}, 1)
	setVar(t, &volumeWindow, fyne.Window(&hideSpy{Window: volumeWindow, hidden: hidden}))

	showVolumeOverlay(0.5, false)
	if volumePercent.Text != "50%" || volumeBarFill.FillColor != volumeFillColor {
		t.Errorf("текст %q, цвет %v", volumePercent.Text, volumeBarFill.FillColor)
	}
	if fill, track := volumeBarFill.Size().Width, volumeBarTrack.Size().Width; track <= 0 || fill != track/2 {
		t.Errorf("заливка %v при ширине трека %v, want половину", fill, track)
	}

	// Повторный вызов перезапускает таймер скрытия; mute красит заливку.
	showVolumeOverlay(1.5, true)
	if volumePercent.Text != "MUTE" || volumeBarFill.FillColor != volumeMuteColor {
		t.Errorf("mute: текст %q, цвет %v", volumePercent.Text, volumeBarFill.FillColor)
	}
	if volumeBarFill.Size().Width != volumeBarTrack.Size().Width {
		t.Error("уровень больше 1 должен обрезаться до полной шкалы")
	}

	select {
	case <-hidden:
	case <-time.After(volumeOverlayDuration + 2*time.Second):
		t.Fatal("оверлей не скрылся по таймеру")
	}
}

func TestShowVolumeOverlayWithoutWindow(t *testing.T) {
	setVar(t, &fyneApp, test.NewTempApp(t))
	setVar(t, &volumeWindow, nil)
	showVolumeOverlay(0.5, false) // окна ещё нет — ничего не происходит
	volumeMu.Lock()
	volumeHideTimer.Stop()
	volumeMu.Unlock()
}

type fakeDesktopWindow struct {
	fyne.Window
	x, y int
}

func (w *fakeDesktopWindow) RequestPosition(x, y int)    { w.x, w.y = x, y }
func (w *fakeDesktopWindow) RequestFullScreenSecondary() {}
func (w *fakeDesktopWindow) RequestAlwaysOnTop()         {}

func TestRequestPosition(t *testing.T) {
	w := &fakeDesktopWindow{}
	requestPosition(w, 10, 20)
	if w.x != 10 || w.y != 20 {
		t.Errorf("(%d,%d), want (10,20)", w.x, w.y)
	}
	requestPosition(w, 0, 0)
	if w.x != 1 || w.y != 0 {
		t.Errorf("(%d,%d), want (1,0): Fyne игнорирует (0,0)", w.x, w.y)
	}
	// Окно без desktop.Window (тестовый драйвер) — просто ничего не делается.
	requestPosition(test.NewTempApp(t).NewWindow("x"), 5, 5)
}

func TestClickCatcher(t *testing.T) {
	commands := make(chan string, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		commands <- r.URL.RequestURI()
	}))
	defer srv.Close()
	setupUI(t, srv.URL+"/variables.html")
	mainWindow.Resize(fyne.NewSize(800, 100))
	started := fakeStart(t, nil)

	c := newClickCatcher(srv.URL + "/variables.html")
	if r := c.CreateRenderer(); len(r.Objects()) != 1 {
		t.Errorf("рендерер: %d объектов", len(r.Objects()))
	}
	c.MouseDown(&desktop.MouseEvent{})

	pos := fyne.CurrentApp().Driver().AbsolutePositionForObject(progressBar)
	size := progressBar.Size()
	inside := pos.Add(fyne.NewPos(size.Width/2, size.Height/2))
	outside := fyne.NewPos(pos.X+size.Width/2, pos.Y+size.Height+50)
	if !pointInProgressBar(inside) || pointInProgressBar(outside) {
		t.Fatalf("pointInProgressBar: inside=%v outside=%v (bar %v @ %v)",
			pointInProgressBar(inside), pointInProgressBar(outside), size, pos)
	}

	click := func(b desktop.MouseButton, p fyne.Position) {
		c.MouseUp(&desktop.MouseEvent{PointEvent: fyne.PointEvent{Position: p}, Button: b})
	}

	// Колесо — проводник с текущим файлом.
	currentFilePath = `E:\movie.mkv`
	click(desktop.MouseButtonTertiary, outside)
	if len(*started) != 1 {
		t.Errorf("клик колесом: запущено %d процессов, want 1 (explorer)", len(*started))
	}

	// Левая кнопка мимо полосы и правая кнопка ничего не делают.
	click(desktop.MouseButtonPrimary, outside)
	click(desktop.MouseButtonSecondary, inside)
	select {
	case uri := <-commands:
		t.Fatalf("неожиданная команда %q", uri)
	case <-time.After(100 * time.Millisecond):
	}

	// Левая кнопка по полосе — Play/Pause.
	click(desktop.MouseButtonPrimary, inside)
	select {
	case uri := <-commands:
		if uri != "/command.html?wm_command=889" {
			t.Errorf("команда %q", uri)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("команда Play/Pause не отправлена")
	}
}

func TestPointInProgressBarWithoutBar(t *testing.T) {
	setVar(t, &progressBar, nil)
	if pointInProgressBar(fyne.NewPos(1, 1)) {
		t.Error("без полосы прогресса клик не может в неё попасть")
	}
}

// ---------------------------------------------------------------------------
// Switcher (WASAPI/COM). Тесты только читают состояние; SetDefault и Cycle(0)
// выставляют то устройство, которое и так стоит по умолчанию во всех ролях,
// поэтому реально ничего не переключается. Без аудиоустройств (CI) тесты
// пропускаются.
// ---------------------------------------------------------------------------

func TestSwitcher(t *testing.T) {
	s, err := NewSwitcher()
	if err != nil {
		t.Skipf("COM/аудио недоступно: %v", err)
	}
	defer s.Close()

	devices, err := s.ListActiveDevices()
	if err != nil {
		t.Fatalf("ListActiveDevices: %v", err)
	}
	if len(devices) == 0 {
		t.Skip("нет активных устройств воспроизведения")
	}
	for _, d := range devices {
		if d.ID == "" || d.Name == "" {
			t.Errorf("пустое устройство: %+v", d)
		}
	}

	current, err := s.DefaultDeviceID()
	if err != nil {
		t.Fatalf("DefaultDeviceID: %v", err)
	}
	vol, err := s.CurrentVolume()
	if err != nil {
		t.Fatalf("CurrentVolume: %v", err)
	}
	if vol.DeviceID != current || vol.Level < 0 || vol.Level > 1 {
		t.Errorf("CurrentVolume = %+v, default %q", vol, current)
	}

	roles, err := execValue(s, func() ([]string, error) {
		var ids []string
		for _, role := range []wca.ERole{wca.EConsole, wca.EMultimedia, wca.ECommunications} {
			var dev *wca.IMMDevice
			if err := s.mmde.GetDefaultAudioEndpoint(wca.ERender, uint32(role), &dev); err != nil {
				return nil, err
			}
			id, err := deviceID(dev)
			dev.Release()
			if err != nil {
				return nil, err
			}
			ids = append(ids, id)
		}
		return ids, nil
	})
	if err != nil || roles[1] != current || roles[2] != current {
		t.Skipf("у ролей разные устройства по умолчанию (%v, %v) — переключение не проверяю", roles, err)
	}
	// Если текущего устройства нет среди активных, Cycle(0) переключил бы на
	// первое — такого в тесте допускать нельзя.
	listed := false
	for _, d := range devices {
		listed = listed || d.ID == current
	}
	if !listed {
		t.Skipf("устройство по умолчанию %q не в списке активных — переключение не проверяю", current)
	}

	if err := s.SetDefault(current); err != nil {
		t.Errorf("SetDefault(текущее): %v", err)
	}
	dev, err := s.Cycle(0)
	if err != nil || dev.ID != current {
		t.Errorf("Cycle(0) = %+v, %v; want текущее устройство %q", dev, err, current)
	}
}

// ---------------------------------------------------------------------------
// Логирование и завершение
// ---------------------------------------------------------------------------

func TestSetupLoggingToFile(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Cleanup(func() { log.SetOutput(os.Stderr) })

	// Тестовый бинарник собран с консольной subsystem, поэтому hideConsole
	// здесь действительно отвязывает процесс от консоли — вывод go test идёт
	// через каналы и от этого не страдает.
	closeLog := setupLogging(false)
	log.Printf("тестовая запись")
	closeLog()

	data, err := os.ReadFile(logFileName)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"=== volume_switch starting", "тестовая запись"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("в логе нет %q:\n%s", want, data)
		}
	}
}

func TestSetupLoggingFileError(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	// Каталог с именем лог-файла — открыть его на запись нельзя.
	if err := os.Mkdir(logFileName, 0o700); err != nil {
		t.Fatal(err)
	}
	closeLog := setupLogging(false)
	log.Printf("уходит в никуда")
	closeLog()
}

func TestOnExit(t *testing.T) {
	stopped := 0
	setVar(t, &stopHotkeys, func() error {
		stopped++
		if stopped > 1 {
			return errors.New("уже снят")
		}
		return nil
	})
	setVar(t, &fyneApp, test.NewTempApp(t))
	sw := &Switcher{quit: make(chan struct{})}
	setVar(t, &switcher, deviceSwitcher(sw))

	onExit()
	if stopped != 1 {
		t.Errorf("хук снимался %d раз, want 1", stopped)
	}
	select {
	case <-sw.quit:
	default:
		t.Error("switcher не закрыт")
	}

	// Повторно: ошибка снятия хука только логируется; без switcher и Fyne
	// ничего не падает.
	switcher, fyneApp = nil, nil
	onExit()
}

// ---------------------------------------------------------------------------
// Меню трея и переключение устройств (с подменёнными systray и Switcher)
// ---------------------------------------------------------------------------

// fakeSwitcher — deviceSwitcher без COM: записывает вызовы и возвращает
// заданные результаты.
type fakeSwitcher struct {
	setErr   error
	cycleDev Device
	cycleErr error

	setIDs []string
	cycles []int
	closed bool
}

func (s *fakeSwitcher) SetDefault(id string) error {
	s.setIDs = append(s.setIDs, id)
	return s.setErr
}

func (s *fakeSwitcher) Cycle(direction int) (Device, error) {
	s.cycles = append(s.cycles, direction)
	return s.cycleDev, s.cycleErr
}

func (s *fakeSwitcher) Close() { s.closed = true }

// fakeTooltip подменяет подсказку трея и возвращает канал, в который
// приходит каждая выставленная подсказка. announceSwitch выставляет её
// последней (после неё — только showNotification, которая не трогает
// подменяемые в тестах переменные), поэтому канал годится и для
// синхронизации с горутинами меню.
func fakeTooltip(t *testing.T) <-chan string {
	t.Helper()
	tips := make(chan string, 10)
	setVar(t, &setTrayTooltip, func(s string) { tips <- s })
	return tips
}

func TestSelectDevice(t *testing.T) {
	a, b := &fakeCheckbox{checked: true}, &fakeCheckbox{}
	setVar(t, &deviceItems, map[string]checkbox{"a": a, "b": b})
	sw := &fakeSwitcher{}
	setVar(t, &switcher, deviceSwitcher(sw))
	tips := fakeTooltip(t)

	selectDevice(Device{ID: "b", Name: "Наушники"})
	if fmt.Sprint(sw.setIDs) != "[b]" {
		t.Errorf("SetDefault вызван с %v", sw.setIDs)
	}
	if a.checked || !b.checked {
		t.Errorf("галочки: a=%v b=%v", a.checked, b.checked)
	}
	if tip := <-tips; tip != "Volume Switch: Наушники" {
		t.Errorf("подсказка %q", tip)
	}

	// Ошибка переключения: ни галочки, ни подсказка не меняются.
	sw.setErr = errors.New("COM")
	selectDevice(Device{ID: "a", Name: "Колонки"})
	if a.checked || !b.checked || len(tips) != 0 {
		t.Errorf("после ошибки: a=%v b=%v, подсказок %d", a.checked, b.checked, len(tips))
	}
}

func TestOnCycle(t *testing.T) {
	a, b := &fakeCheckbox{checked: true}, &fakeCheckbox{}
	setVar(t, &deviceItems, map[string]checkbox{"a": a, "b": b})
	sw := &fakeSwitcher{cycleDev: Device{ID: "b", Name: "Наушники"}}
	setVar(t, &switcher, deviceSwitcher(sw))
	tips := fakeTooltip(t)

	onCycle(1)
	if fmt.Sprint(sw.cycles) != "[1]" || a.checked || !b.checked {
		t.Errorf("cycles=%v a=%v b=%v", sw.cycles, a.checked, b.checked)
	}
	if tip := <-tips; tip != "Volume Switch: Наушники" {
		t.Errorf("подсказка %q", tip)
	}

	sw.cycleErr = errors.New("нет устройств")
	onCycle(-1)
	if fmt.Sprint(sw.cycles) != "[1 -1]" || !b.checked || len(tips) != 0 {
		t.Errorf("после ошибки: cycles=%v b=%v, подсказок %d", sw.cycles, b.checked, len(tips))
	}
}

func TestBuildDeviceMenu(t *testing.T) {
	setVar(t, &deviceItems, nil)
	sw := &fakeSwitcher{}
	setVar(t, &switcher, deviceSwitcher(sw))
	tips := fakeTooltip(t)

	type fakeItem struct {
		title, tooltip string
		box            *fakeCheckbox
		clicks         chan struct{}
	}
	var items []*fakeItem
	setVar(t, &addTrayCheckbox, func(title, tooltip string, checked bool) (checkbox, <-chan struct{}) {
		it := &fakeItem{title: title, tooltip: tooltip, box: &fakeCheckbox{checked: checked}, clicks: make(chan struct{})}
		items = append(items, it)
		return it.box, it.clicks
	})

	buildDeviceMenu([]Device{{ID: "a", Name: "Колонки"}, {ID: "b", Name: "Наушники"}}, "a")
	if len(items) != 2 || items[0].title != "Колонки" || items[1].title != "Наушники" {
		t.Fatalf("пункты меню: %+v", items)
	}
	if !items[0].box.checked || items[1].box.checked {
		t.Error("галочка должна стоять на текущем устройстве")
	}

	// Клик по второму пункту делает его устройством по умолчанию.
	items[1].clicks <- struct{}{}
	if tip := <-tips; tip != "Volume Switch: Наушники" {
		t.Errorf("подсказка %q", tip)
	}
	if fmt.Sprint(sw.setIDs) != "[b]" || items[0].box.checked || !items[1].box.checked {
		t.Errorf("после клика: SetDefault=%v, галочки %v/%v", sw.setIDs, items[0].box.checked, items[1].box.checked)
	}
	for _, it := range items {
		close(it.clicks) // завершить горутины пунктов меню
	}
}

// ---------------------------------------------------------------------------
// Ветки ошибок, которые можно вызвать без вреда для системы
// ---------------------------------------------------------------------------

func TestFixOverlayWindowChromeZeroExStyle(t *testing.T) {
	// У окон GLFW расширенный стиль никогда не нулевой, поэтому 0 от
	// GetWindowLongPtrW считается ошибкой.
	title := uniqueTitle(t, "zero")
	newNativeWindow(t, title, 0)
	if err := fixOverlayWindowChrome(title, false); err == nil {
		t.Error("для окна с нулевым расширенным стилем ожидалась ошибка")
	}
}

func TestLogRegistryCloseError(t *testing.T) {
	k, err := registry.OpenKey(registry.CURRENT_USER, `Software`, registry.QUERY_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	if err := k.Close(); err != nil {
		t.Fatal(err)
	}
	logRegistryClose(k, "уже закрытый") // повторное закрытие — ошибка только в лог
}

func setRegistryDWord(t *testing.T, path, name string, value uint32) {
	t.Helper()
	k, err := registry.OpenKey(registry.CURRENT_USER, path, registry.SET_VALUE)
	if err != nil {
		t.Fatal(err)
	}
	defer k.Close()
	if err := k.SetDWordValue(name, value); err != nil {
		t.Fatal(err)
	}
}

func TestRegistryValuesOfWrongType(t *testing.T) {
	// Значения не строкового типа: не «нет значения», а ошибка чтения —
	// логируется, результат как при отсутствии значения.
	path := tempRegistryKey(t)
	setRegistryDWord(t, path, autostartValueName, 1)
	setRegistryDWord(t, path, "ExePath", 1)

	setVar(t, &autostartRegistryPath, path)
	if isAutostartEnabled() {
		t.Error("DWORD вместо строки: автозапуск считается включённым")
	}
	setVar(t, &mpcRegistryPath, path)
	if got := mpcExeFromRegistry(); got != "" {
		t.Errorf("DWORD вместо строки: ExePath = %q", got)
	}
}

// truncatedServer обещает в Content-Length больше, чем отдаёт: сервер
// закрывает соединение, клиент получает ошибку при чтении тела.
func truncatedServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Length", "1000")
		_, _ = w.Write([]byte("<p id="))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestFetchStateTruncatedBody(t *testing.T) {
	srv := truncatedServer(t)
	if _, err := fetchState(srv.URL, srv.Client()); err == nil {
		t.Error("оборванный ответ: ожидалась ошибка")
	}
}

func TestToggleMPCPlaybackTruncatedBody(t *testing.T) {
	started := fakeStart(t, nil)
	srv := truncatedServer(t)
	toggleMPCPlayback(srv.URL + "/variables.html") // ошибка чтения только в лог
	if len(*started) != 0 {
		t.Error("ответ получен — MPC-HC запускать не нужно")
	}
}
