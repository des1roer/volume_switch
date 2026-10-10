package main

import (
	"bytes"
	"encoding/binary"
	"image/png"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/windows"
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
