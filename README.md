# volume_switch

*English version: [below](#english).*

Утилита для системного трея Windows в духе [Volume²](https://github.com/irzyxa/Volume2): **F7/F8** переключают устройство воспроизведения звука по умолчанию, **F3/F4** убавляют/прибавляют общую громкость, а **F5** показывает/скрывает встроенное окно-оверлей с прогрессом MPC-HC.

Написана для себя как замена Volume² на Windows 11: там его оверлей и хоткеи стали подлагивать, а в логе периодически появляется падение в `ShowOSDWindow` ([irzyxa/Volume2#426](https://github.com/irzyxa/Volume2/issues/426)). Сюда вошло только то, чем я пользовался в Volume², — переключение устройства и громкость с оверлеем — плюс оверлей прогресса MPC-HC.

## Возможности

- Глобальные горячие клавиши **F7/F8** переключают устройство воспроизведения Windows по умолчанию (роли console, multimedia и communications переключаются вместе с ним).
- **F3/F4** убавляют/прибавляют общую громкость текущего устройства на 2% (как мультимедийные клавиши Windows) и, как и они, снимают mute; при этом показывается оверлей громкости.
- Горячие клавиши срабатывают только без модификаторов: сочетания с Alt/Ctrl/Shift/Win (Alt+F4, Ctrl+F4, Shift+F3, Ctrl+F5 и т.п.) передаются программам как обычно.
- Меню в трее содержит список всех активных устройств воспроизведения с галочкой на текущем устройстве по умолчанию; клик по любой записи переключает на неё напрямую.
- Пункт меню «Запускать при входе в Windows» (с галочкой) включает/выключает автозапуск через `HKCU\...\Run`.
- Всплывающее уведомление при каждом переключении с именем нового устройства.
- Оверлей-ползунок уровня глобальной громкости в четверть экрана (в левом нижнем углу монитора под курсором), с большим процентом/`MUTE` и шкалой-градацией 0—100 через каждые 10, при любом изменении громкости — клавишами, колесом мыши в системном микшере, аппаратными кнопками или сторонними программами. Изменение детектируется поллингом текущего уровня каждые 120 мс (`Switcher.CurrentVolume`, `pollVolume`), а не перехватом конкретных клавиш, поэтому ловится независимо от источника. Переключение default-устройства (F7/F8, трей, панель задач Windows) оверлей НЕ показывает, даже если у нового устройства другой сохранённый уровень громкости — поллер сравнивает уровень только в рамках одного и того же устройства (по ID). Гаснет сам примерно через 1.2 секунды после последнего изменения.
- **F5** показывает/скрывает окно-оверлей поверх всех окон (не забирая фокус), которое опрашивает веб-интерфейс MPC-HC (`variables.html`) и отображает название/прогресс/прошедшее/оставшееся время.
- Клик средней кнопкой мыши (колесом) по оверлею открывает в проводнике папку с текущим файлом (с выделением самого файла).
- Клик левой кнопкой по полосе прогресса ставит воспроизведение на паузу/возобновляет его; если MPC-HC не запущен — запускает его (путь берётся из `MPC_EXE`, по умолчанию `C:\Program Files (x86)\K-Lite Codec Pack\MPC-HC64\mpc-hc64.exe`, а если такого файла нет — из `HKCU\Software\MPC-HC\MPC-HC\ExePath`, который MPC-HC пишет сам).
- Необязательный файл `.env` для конфигурации; при отсутствии используются переменные окружения, затем флаги.

## Скачать

Готовый `volume_switch.exe` лежит в [релизах](https://github.com/des1roer/volume_switch/releases/latest) — собирать самому не обязательно.

Новый релиз публикуется автоматически (`.github/workflows/release.yml`) при пуше тега вида `vX.Y.Z`:

```sh
git tag v1.2.3
git push origin v1.2.3
```

## Требования

- Go 1.27.1+ (как в `go.mod`)
- Windows (использует `syscall`/`golang.org/x/sys/windows` и вызовы WinAPI напрямую; не соберётся и не запустится на других платформах)
- Опционально: MPC-HC с включённым веб-интерфейсом, отдающий `http://localhost:7777/variables.html` (нужен только для оверлея по F5)

## Сборка

```sh
go build -ldflags "-H=windowsgui"     # основной способ сборки — без консоли вообще
```

Это основной и единственный рекомендуемый способ собрать `volume_switch.exe`.
Без `-H=windowsgui` (`go build` без флагов) получится exe с консольной
subsystem в PE-заголовке, и загрузчик Windows будет подключать для него
консольное окно при каждом запуске ещё до выполнения кода — независимо от
флага `-debug`. `-H=windowsgui` ставит в PE-заголовке GUI subsystem, и
загрузчик не создаёт консоль вообще ни при каких условиях; `-debug=true`
по-прежнему может показать консоль явно через `ensureConsole()`/`AllocConsole()`.

Проверено вживую (сборка → запуск → перебор всех окон процесса через
Win32 `EnumWindows`): с `-debug=false` (по умолчанию) после `-H=windowsgui`
сборки у процесса нет ни одного видимого окна, кроме самого трея — только
скрытые служебные окна GLFW/systray/IME, лог пишется в `volume_switch.log`.
Раньше это ломалось из-за отдельного бага: `onReady()` безусловно вызывал
`ensureConsole()` (оставшийся от прежней версии, где консоль была всегда) —
из-за этого консоль появлялась заново сразу после инициализации трея, даже
при `-debug=false`. Теперь вызов обёрнут проверкой `if debug` (флаг передаётся в `onReady`).

Для разработки без флага удобнее `go run .` / `go build` — тогда видна
консоль с логами независимо от `-debug` (так как subsystem по умолчанию
консольный), либо явно передавайте `-debug=true`, если собрали с
`-H=windowsgui`.

Проверить subsystem собранного `.exe` (2 = GUI, 3 = Console):

```powershell
$b = [IO.File]::ReadAllBytes("volume_switch.exe")
$off = [BitConverter]::ToInt32($b, 0x3C) + 0x5C
[BitConverter]::ToInt16($b, $off)
```

## Конфигурация

| Флаг        | Переменная окр. | По умолчанию                           | Значение                            |
|-------------|-----------------|----------------------------------------|-------------------------------------|
| `-debug`    | —               | `false`                                | Показывать консоль с логами отладки |
| `-url`      | `MPC_URL`       | `http://localhost:7777/variables.html` | URL `variables.html` MPC-HC         |
| `-interval` | `MPC_INTERVAL`  | `500ms`                                | Интервал опроса для оверлея         |
| —           | `MPC_EXE`       | `C:\Program Files (x86)\K-Lite Codec Pack\MPC-HC64\mpc-hc64.exe` | Путь к MPC-HC для запуска по клику  |

Флаги переопределяют переменные окружения, те переопределяют файл `.env`, который переопределяет жёстко заданные значения по умолчанию. Файл `.env` (`KEY=VALUE` по строке, комментарии через `#` разрешены) в рабочем каталоге загружается при запуске, но никогда не переопределяет переменную, уже заданную в реальном окружении.

## Логирование

- `-debug=false` (по умолчанию): логи пишутся в `volume_switch.log` в рабочем каталоге.
- `-debug=true`: логи выводятся в подключённую/выделенную консоль.

## Для ИИ-ассистентов (обзор архитектуры)

Это намеренно **однофайловая программа на Go** (`main.go`, `package main`, без пакетов в `internal/`), чтобы всё целиком можно было вставить в контекст другой модели одним блоком.

**Структура файла** (по порядку, каждая часть помечена заголовком секции `// ---...---` в `main.go`):

1. Логирование (`ensureConsole`, `setupLogging`)
2. Загрузка `.env` (`loadDotEnv`)
3. Парсинг `variables.html` MPC-HC (на регулярках, `fetchState`/`MPCState`)
4. Хелперы Win32: монитор под курсором и высота заголовка окна (`screenLayout`, `titleBarHeight`)
5. Переключение аудиоустройств и шаг громкости через WASAPI/COM (`Device`, `Switcher`, `Switcher.StepVolume`)
6. Монитор глобальной громкости поллингом (`Switcher.CurrentVolume`, `pollVolume`)
7. Всплывающие уведомления (`showNotification` — находит скрытое окно systray и вызывает `Shell_NotifyIconW` напрямую; публичного API systray для этого нет)
8. Генерация иконки трея во время выполнения (`trayIconData`, PNG, обёрнутый в минимальный контейнер `.ico` — без внешнего файла ресурсов)
9. Встроенное окно-оверлей Fyne (`runFyne`, `toggleMPCWindow`); `clickCatcher` ловит клик средней кнопкой мыши и открывает папку с файлом (`openContainingFolder`, `explorer /select,`); второй оверлей — ползунок громкости с градацией (`volumeWindow`, `showVolumeOverlay`, `layoutVolumeOverlay`)
10. Подключение systray + меню устройств (`main`, `onReady`, `onExit`, `buildDeviceMenu`, `selectDevice`, `onCycle`)
11. Глобальный низкоуровневый хук клавиатуры для F3/F4/F5/F7/F8 (`startHotkeys`, `hookProc`, `WH_KEYBOARD_LL`)

**Ключевые типы / инварианты:**

- `Switcher` владеет COM-объектами (`IMMDeviceEnumerator`, `IPolicyConfigVista`). Все вызовы COM сериализуются в **одну выделенную горутину, закреплённую через `runtime.LockOSThread`**, через канал (`reqCh`/`exec`), потому что COM-объекты живут в квартире этого потока (`CoInitializeEx(COINIT_APARTMENTTHREADED)`). Никогда не вызывайте методы COM из другой горутины.
- `startHotkeys` устанавливает хук `WH_KEYBOARD_LL` в своём собственном закреплённом OS-потоке со своим циклом сообщений Win32; обработчики берутся из карты `hotkeys` (virtual-key → функция, заполняется до установки хука и дальше не меняется) и запускаются через `go handler()`, чтобы процедура хука возвращалась быстро.
- Громкость отслеживается поллингом (`pollVolume`), а не push-уведомлениями WASAPI. Изначально были реализованы push-уведомления через самодельный `IAudioEndpointVolumeCallback` (`github.com/diegosz/go-wca` объявляет `IAudioEndpointVolume` с правильными смещениями vtable, но `Register/UnregisterControlChangeNotify` у неё — заглушки (`E_NOTIMPL`), а сам колбэк-интерфейс в библиотеке не реализован вообще, так что оба куска пришлось бы делать вручную по образцу `IMMNotificationClient`) — регистрация проходила успешно, но `OnNotify` ни разу не сработал даже с доработанным циклом сообщений (`TranslateMessage`/`DispatchMessageW`). От push-модели отказались в пользу простого поллинга `Switcher.CurrentVolume()` каждые 120 мс — надёжно работает независимо от драйвера/окружения и не требует отдельного потока/COM-квартиры.
- Fyne 2.8 требует модели потоков `fyne.Do`/`fyne.DoAndWait`: любое изменение UI из горутины, отличной от той, что вызвала `fyneApp.Run()`, должно быть обёрнуто в `fyne.Do(func() { ... })` (см. цикл опроса MPC и `toggleMPCWindow`).
- **Важный инвариант потоков:** пакет `github.com/getlantern/systray` в своём `init()` безусловно вызывает `runtime.LockOSThread()` — а `init()` выполняется на горутине, которая затем становится горутиной `main()`, то есть эта горутина навсегда закрепляется за главным потоком процесса ещё до начала `main()`. Fyne 2.8+ требует, чтобы `app.New()`/`NewWindow()`/`Run()` вызывались именно на этой горутине (см. https://docs.fyne.io/started/goroutines) — если вызвать их из другой горутины (как было до исправления), Fyne лишь печатает предупреждение о неверном потоке и затем **зависает насовсем** внутри `NewWindow()`, так что окно никогда не создаётся и F5 ничего не делает. Поэтому `main()` вызывает `runFyne(...)` напрямую (блокируясь на горутине `main`), а `systray.Run(onReady, onExit)` запускается в отдельной горутине со своим `runtime.LockOSThread()`.

**Зависимости:** `fyne.io/fyne/v2` v2.8.1 (UI оверлея), `github.com/getlantern/systray` (иконка/меню трея), `github.com/diegosz/go-wca` + `github.com/go-ole/go-ole` (переключение устройств WASAPI/COM), `golang.org/x/sys` (сисколлы Win32).

**Напоминание о флагах сборки:** основной способ сборки — `go build -ldflags "-H=windowsgui"` (GUI subsystem в PE-заголовке, загрузчик Windows не создаёт консоль). `-debug` по умолчанию `false` — логи в `volume_switch.log`, окон кроме трея нет (проверено перебором окон процесса). `onReady()` вызывает `ensureConsole()` только при `-debug=true` — раньше вызов был безусловным и пересоздавал консоль сразу после инициализации трея даже при `-debug=false`.

---

<a id="english"></a>

# volume_switch (English)

A Windows system tray utility in the spirit of [Volume²](https://github.com/irzyxa/Volume2): **F7/F8** switch the default audio playback device, **F3/F4** lower/raise the master volume, and **F5** shows/hides a built-in overlay window with MPC-HC playback progress.

Written for my own use as a Volume² replacement on Windows 11: its overlay and hotkeys started to lag there, and its log periodically shows a crash in `ShowOSDWindow` ([irzyxa/Volume2#426](https://github.com/irzyxa/Volume2/issues/426)). It includes only what I actually used in Volume² — device switching and volume with an overlay — plus an MPC-HC progress overlay.

## Features

- Global hotkeys **F7/F8** switch the Windows default playback device (the console, multimedia and communications roles are switched together).
- **F3/F4** lower/raise the master volume of the current device by 2% (like the Windows multimedia keys) and, like them, unmute; the volume overlay is shown.
- Hotkeys fire only without modifiers: combinations with Alt/Ctrl/Shift/Win (Alt+F4, Ctrl+F4, Shift+F3, Ctrl+F5, etc.) are passed to applications as usual.
- The tray menu lists all active playback devices with a checkmark on the current default; clicking any entry switches to it directly.
- The "Запускать при входе в Windows" ("Run at Windows sign-in") menu item, with a checkmark, enables/disables autostart via `HKCU\...\Run`.
- A balloon notification with the new device name on every switch.
- A quarter-screen master volume slider overlay (in the bottom-left corner of the monitor under the cursor) with a large percentage/`MUTE` and a 0—100 scale marked every 10, shown on any volume change — by hotkeys, the mouse wheel in the system mixer, hardware buttons or third-party programs. Changes are detected by polling the current level every 120 ms (`Switcher.CurrentVolume`, `pollVolume`) rather than by intercepting specific keys, so they are caught regardless of the source. Switching the default device (F7/F8, tray, Windows taskbar) does NOT show the overlay, even if the new device has a different saved volume level — the poller compares levels only within the same device (by ID). It fades out by itself about 1.2 seconds after the last change.
- **F5** shows/hides an always-on-top overlay window (without stealing focus) that polls the MPC-HC web interface (`variables.html`) and displays the title/progress/elapsed/remaining time.
- A middle-click (wheel click) on the overlay opens the folder of the current file in Explorer, with the file itself selected.
- A left click on the progress bar pauses/resumes playback; if MPC-HC is not running, it launches it (the path comes from `MPC_EXE`, by default `C:\Program Files (x86)\K-Lite Codec Pack\MPC-HC64\mpc-hc64.exe`, and if that file doesn't exist — from `HKCU\Software\MPC-HC\MPC-HC\ExePath`, which MPC-HC writes itself).
- An optional `.env` file for configuration; environment variables and then flags take precedence over it.

## Download

A prebuilt `volume_switch.exe` is available in [releases](https://github.com/des1roer/volume_switch/releases/latest) — no need to build it yourself.

A new release is published automatically (`.github/workflows/release.yml`) when a `vX.Y.Z` tag is pushed:

```sh
git tag v1.2.3
git push origin v1.2.3
```

## Requirements

- Go 1.27.1+ (as in `go.mod`)
- Windows (uses `syscall`/`golang.org/x/sys/windows` and WinAPI calls directly; won't build or run on other platforms)
- Optional: MPC-HC with the web interface enabled, serving `http://localhost:7777/variables.html` (needed only for the F5 overlay)

## Building

```sh
go build -ldflags "-H=windowsgui"     # the main way to build — no console at all
```

This is the main and the only recommended way to build `volume_switch.exe`.
Without `-H=windowsgui` (plain `go build`) you get an exe with the console
subsystem in its PE header, and the Windows loader attaches a console window
on every launch before any code runs — regardless of the `-debug` flag.
`-H=windowsgui` sets the GUI subsystem in the PE header, and the loader never
creates a console; `-debug=true` can still show a console explicitly via
`ensureConsole()`/`AllocConsole()`.

Verified live (build → run → enumerate all process windows via Win32
`EnumWindows`): with `-debug=false` (the default) an `-H=windowsgui` build has
no visible windows except the tray itself — only hidden GLFW/systray/IME
service windows, and the log goes to `volume_switch.log`. This used to break
because of a separate bug: `onReady()` unconditionally called `ensureConsole()`
(left over from an earlier version where the console was always present), so
the console reappeared right after the tray initialized, even with
`-debug=false`. Now the call is guarded by `if debug` (the flag is passed to `onReady`).

For development, plain `go run .` / `go build` is more convenient — the console
with logs is visible regardless of `-debug` (since the default subsystem is
console), or pass `-debug=true` explicitly if you built with `-H=windowsgui`.

Check the subsystem of the built `.exe` (2 = GUI, 3 = Console):

```powershell
$b = [IO.File]::ReadAllBytes("volume_switch.exe")
$off = [BitConverter]::ToInt32($b, 0x3C) + 0x5C
[BitConverter]::ToInt16($b, $off)
```

## Configuration

| Flag        | Env variable    | Default                                | Meaning                             |
|-------------|-----------------|----------------------------------------|-------------------------------------|
| `-debug`    | —               | `false`                                | Show a console with debug logs      |
| `-url`      | `MPC_URL`       | `http://localhost:7777/variables.html` | MPC-HC `variables.html` URL         |
| `-interval` | `MPC_INTERVAL`  | `500ms`                                | Polling interval for the overlay    |
| —           | `MPC_EXE`       | `C:\Program Files (x86)\K-Lite Codec Pack\MPC-HC64\mpc-hc64.exe` | Path to MPC-HC to launch on click   |

Flags override environment variables, which override the `.env` file, which overrides the hard-coded defaults. The `.env` file (`KEY=VALUE` per line, `#` comments allowed) in the working directory is loaded at startup but never overrides a variable already set in the real environment.

## Logging

- `-debug=false` (default): logs go to `volume_switch.log` in the working directory.
- `-debug=true`: logs go to an attached/allocated console.

## For AI assistants (architecture overview)

This is intentionally a **single-file Go program** (`main.go`, `package main`, no packages in `internal/`), so the whole thing can be pasted into another model's context in one block.

**File layout** (in order, each part marked with a `// ---...---` section header in `main.go`):

1. Logging (`ensureConsole`, `setupLogging`)
2. `.env` loading (`loadDotEnv`)
3. Parsing MPC-HC `variables.html` (regex-based, `fetchState`/`MPCState`)
4. Win32 helpers: monitor under the cursor and window title bar height (`screenLayout`, `titleBarHeight`)
5. Audio device switching and volume stepping via WASAPI/COM (`Device`, `Switcher`, `Switcher.StepVolume`)
6. Master volume monitoring by polling (`Switcher.CurrentVolume`, `pollVolume`)
7. Balloon notifications (`showNotification` finds the hidden systray window and calls `Shell_NotifyIconW` directly; systray has no public API for this)
8. Tray icon generation at runtime (`trayIconData`, a PNG wrapped in a minimal `.ico` container, no external resource file)
9. Built-in Fyne overlay window (`runFyne`, `toggleMPCWindow`); `clickCatcher` catches the middle click and opens the file's folder (`openContainingFolder`, `explorer /select,`); the second overlay is the volume slider with a scale (`volumeWindow`, `showVolumeOverlay`, `layoutVolumeOverlay`)
10. Systray wiring + device menu (`main`, `onReady`, `onExit`, `buildDeviceMenu`, `selectDevice`, `onCycle`)
11. Global low-level keyboard hook for F3/F4/F5/F7/F8 (`startHotkeys`, `hookProc`, `WH_KEYBOARD_LL`)

**Key types / invariants:**

- `Switcher` owns the COM objects (`IMMDeviceEnumerator`, `IPolicyConfigVista`). All COM calls are serialized onto **one dedicated goroutine pinned with `runtime.LockOSThread`** through a channel (`reqCh`/`exec`), because the COM objects live in that thread's apartment (`CoInitializeEx(COINIT_APARTMENTTHREADED)`). Never call COM methods from another goroutine.
- `startHotkeys` installs the `WH_KEYBOARD_LL` hook on its own pinned OS thread with its own Win32 message loop; handlers come from the `hotkeys` map (virtual-key → function, filled before the hook is installed and never changed afterwards) and are run via `go handler()` so the hook procedure returns quickly.
- Volume is tracked by polling (`pollVolume`), not by WASAPI push notifications. Push notifications were implemented first via a hand-made `IAudioEndpointVolumeCallback` (`github.com/diegosz/go-wca` declares `IAudioEndpointVolume` with correct vtable offsets, but its `Register/UnregisterControlChangeNotify` are stubs (`E_NOTIMPL`), and the callback interface isn't implemented in the library at all, so both pieces had to be written by hand following the `IMMNotificationClient` pattern). Registration succeeded, but `OnNotify` never fired even with an improved message loop (`TranslateMessage`/`DispatchMessageW`). The push model was dropped in favor of simply polling `Switcher.CurrentVolume()` every 120 ms: it works reliably regardless of driver/environment and needs no separate thread/COM apartment.
- Fyne 2.8 requires the `fyne.Do`/`fyne.DoAndWait` threading model: any UI change from a goroutine other than the one that called `fyneApp.Run()` must be wrapped in `fyne.Do(func() { ... })` (see the MPC polling loop and `toggleMPCWindow`).
- **Important threading invariant:** the `github.com/getlantern/systray` package unconditionally calls `runtime.LockOSThread()` in its `init()`, and `init()` runs on the goroutine that then becomes the `main()` goroutine, so that goroutine is pinned to the process main thread forever before `main()` even starts. Fyne 2.8+ requires `app.New()`/`NewWindow()`/`Run()` to be called on exactly this goroutine (see https://docs.fyne.io/started/goroutines). If they are called from another goroutine (as before the fix), Fyne only prints a wrong-thread warning and then **hangs forever** inside `NewWindow()`, so the window is never created and F5 does nothing. That's why `main()` calls `runFyne(...)` directly (blocking on the `main` goroutine), while `systray.Run(onReady, onExit)` runs in a separate goroutine with its own `runtime.LockOSThread()`.

**Dependencies:** `fyne.io/fyne/v2` v2.8.1 (overlay UI), `github.com/getlantern/systray` (tray icon/menu), `github.com/diegosz/go-wca` + `github.com/go-ole/go-ole` (WASAPI/COM device switching), `golang.org/x/sys` (Win32 syscalls).

**Build flags reminder:** the main way to build is `go build -ldflags "-H=windowsgui"` (GUI subsystem in the PE header, the Windows loader creates no console). `-debug` defaults to `false`: logs go to `volume_switch.log`, no windows besides the tray (verified by enumerating process windows). `onReady()` calls `ensureConsole()` only with `-debug=true`; the call used to be unconditional and recreated the console right after the tray initialized even with `-debug=false`.
