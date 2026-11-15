# Пульт MPC (Android)

*English version: [below](#english).*

Android-клиент HTTP API пульта [volume_switch](../README.md#пульт-с-телефона): управляет MPC-HC и громкостью ПК с телефона или планшета в локальной сети.

## Возможности

- Большая кнопка «Пауза/Старт»; если MPC-HC не запущен, ПК запускает его.
- Крупно — сколько осталось до конца файла, ниже ползунок перемотки с прошедшим временем и длительностью: пока его тянешь, время показывает, куда перемотается, а MPC-HC перематывается, когда ползунок отпущен (`POST /seek?position_ms=`). Кнопки «−1 мин», «−10 с», «+10 с», «+1 мин» перематывают от текущей позиции MPC-HC (`POST /seek?offset_ms=`). Новая позиция показывается сразу, не дожидаясь ответа ПК; если команда не прошла, поток `/events` переподключается и возвращает настоящую.
- «Предыдущая»/«Следующая» — соседний файл в MPC-HC (серия); кнопки неактивны, пока плеер выключен.
- Громкость ПК и собственная громкость MPC-HC: кнопки −/+ и текущий уровень (`MUTE` при выключенном звуке).
- Настройка «Кнопки громкости управляют ПК» (на телефоне по умолчанию включена, на планшете — выключена, `smallestScreenWidthDp ≥ 600`). Включена — кнопки громкости в приложении, в шторке и на экране блокировки меняют громкость ПК (медиасессия объявляет удалённую громкость, `setPlaybackToRemote`). Выключена — они меняют громкость самого устройства (`setPlaybackToLocal`). Кнопки «тише/громче (ПК)» в уведомлении управляют ПК всегда.
- Медиауведомление (в шторке и на экране блокировки): название файла, сколько осталось (в минутах, обновляется раз в минуту и только при включённом экране), громкость ПК, кнопки пауза/старт, предыдущая/следующая и тише/громче ПК, полоса прогресса, которую можно перетаскивать для перемотки (`ACTION_SEEK_TO`). Им управляет foreground-сервис `RemoteService` с `MediaSession`; громкость сессии объявлена удалённой (`setPlaybackToRemote`) и привязана к громкости ПК.
- Сервис останавливается сам, если MPC-HC 30 минут ничего не воспроизводит; при следующем открытии приложения запускается снова.
- «Смотреть на этом устройстве» (`PlayerActivity`, Media3/ExoPlayer): тот же файл, что открыт в MPC-HC, проигрывается на планшете или телефоне синхронно с ПК — файл берётся с ПК по `GET /media` без перекодирования, позиция/пауза/серия — из потока `/events`. Расхождение больше 1,5 с исправляется перемоткой, меньшее — скоростью 0,95–1,05× (`syncAction`); после события от ПК (в том числе перемотки в MPC-HC) плеер догоняет сразу, иначе перематывается не чаще раза в 5 с. Ползунок и кнопки перемотки на экране плеера перематывают MPC-HC, а планшет следует за ним. Звук на устройстве по умолчанию выключен (кнопка «Звук на планшете»), экран не гаснет, ориентация — альбомная.

## Требования

- Android 13+ (`minSdk 33`). Разрешение на уведомления запрашивается при первом запуске — без него медиауведомления не будет, остальное работает.
- На ПК запущен volume_switch с включённым API (`API_LISTEN`, например `API_LISTEN=:7778` в `%LOCALAPPDATA%\volume_switch\.env`) и открытым портом в брандмауэре — см. [«Пульт с телефона»](../README.md#пульт-с-телефона).
- Телефон и ПК в одной сети. Трафик — обычный HTTP без TLS (поэтому в манифесте `usesCleartextTraffic="true"`), авторизации у API нет.

## Настройка

Адрес ПК вводится в поле вверху экрана и сохраняется (`SharedPreferences`): `хост:порт` (`192.168.3.2:7778`, это же значение по умолчанию) или полный URL — без схемы подставляется `http://`. Если ПК не отвечает, под полем появляется «Нет связи с ПК» с текстом ошибки.

Опроса нет: приложение держит поток `GET /events` (SSE), и ПК присылает состояние сам — сразу при подключении и затем только при значимых изменениях (пауза, перемотка, другой файл, громкость). Оставшееся время телефон досчитывает сам от момента получения события; на экране оно тикает раз в секунду, пока идёт воспроизведение и приложение открыто. При обрыве поток переподключается с паузой 1 → 2 → 5 → 10 с; при открытии приложения и включении экрана — сразу (после Doze соединение могло тихо умереть). Мёртвое соединение без пингов обнаруживается за 60 с (`readTimeout`). Сервер без `/events` (старый volume_switch) не поддерживается — приложение пишет, что его нужно обновить. Команды отвечают тем же JSON, что и `/status`, поэтому экран обновляется сразу после нажатия.

## Сборка и установка

Нужны JDK 17 и Android SDK. Путь к SDK — в `local.properties` (не в git):

```properties
sdk.dir=C\:\\Android\\Sdk
```

Для повседневного использования — релизная сборка: R8 (минификация и удаление неиспользуемого кода и ресурсов), не отлаживаемая, только `arm64-v8a`, с базовыми профилями Compose. Она подписывается ключом из `~/.gradle/gradle.properties` (вне репозитория); без этих свойств — debug-ключом:

```properties
volumeRemoteKeystore=C:/Users/<user>/.android/volumeremote-release.jks
volumeRemoteStorePassword=…
volumeRemoteKeyAlias=volumeremote
volumeRemoteKeyPassword=…
```

```sh
./gradlew testReleaseUnitTest assembleRelease
adb devices -l
adb -s <serial> install -r app/build/outputs/apk/release/app-release.apk
adb -s <serial> shell cmd package compile -m speed -f com.des1roer.volumeremote   # сразу AOT-компиляция под процессор устройства
```

Debug и release подписаны разными ключами: чтобы сменить одну на другую, приложение сначала удаляют (`adb uninstall com.des1roer.volumeremote`). Потеря релизного ключа означает то же самое для следующих обновлений. Debug-сборка — `./gradlew assembleDebug`, APK в `app/build/outputs/apk/debug/`. `adb` лежит в `platform-tools` Android SDK.

Стек: AGP 8.7.3, Gradle 8.9 (wrapper), Kotlin 2.0.21, Jetpack Compose (Material 3, динамические цвета), Media3/ExoPlayer 1.5.0, без сторонних HTTP/JSON-библиотек — `HttpURLConnection` и `org.json`.

**Звук AC3/E-AC3 и лицензия.** У многих Android-устройств (например, POCO Pad) нет декодера Dolby Digital/Digital Plus, а в WEB-DL-релизах звук почти всегда такой. Поэтому плеер подключает программный декодер FFmpeg — готовую сборку `org.jellyfin.media3:media3-ffmpeg-decoder` (используется, только если системного декодера нет, `EXTENSION_RENDERER_MODE_ON`). Она распространяется под **GPLv3**, поэтому собранный APK тоже распространяется на условиях GPLv3; исходный код в этом репозитории остаётся под MIT. Версия декодера должна совпадать с версией Media3.

На экране плеера кнопки громкости меняют громкость самого устройства (а не ПК, как на главном экране); если звук в плеере выключен, «громче» его включает.

## Устройство кода

`app/src/main/java/com/des1roer/volumeremote/`:

| Файл | Что внутри |
|------|------------|
| `RemoteClient.kt` | HTTP-клиент API (`RemoteClient`), разбор ответа (`parseStatus` → `Status`), `normalizeServer`, `errorMessage`, `formatTime` |
| `Remote.kt` | Синглтон `Remote`: адрес ПК, `StateFlow<UiState>`, поток `/events` и команды. Поток открыт, пока есть хотя бы один пользователь (`acquire`/`release`): экран и сервис |
| `RemoteScreen.kt` | Экран на Compose |
| `MainActivity.kt` | Активити: тема, кнопки громкости телефона, запрос разрешения на уведомления, запуск `RemoteService` |
| `RemoteService.kt` | Foreground-сервис с `MediaSession` и медиауведомлением |
| `VolumeRemoteApp.kt` | `Application`: инициализирует `Remote` |
| `PlayerActivity.kt` | Просмотр фильма на устройстве: ExoPlayer, синхронизация с MPC-HC |
| `VideoSync.kt` | Чистые функции синхронизации: `syncAction`, `mediaUrl` |

Клиент повторяет формат JSON `apiStatus` из `main.go` вручную: при изменении API обновить `parseStatus` и `RemoteClientTest`.

Тесты (`app/src/test`, JUnit 4) покрывают чистые функции `RemoteClient.kt`: `parseStatus`, `SseParser`, `Status.positionAt`/`remainingAt`, `normalizeServer`, `errorMessage`, `formatTime`, `formatRemainingMinutes`.

Это отдельный Gradle-проект: Go-сборка и CI репозитория его не собирают и не тестируют.

---

<a id="english"></a>

# MPC Remote (Android)

A client for the [volume_switch](../README.md#phone-remote) phone remote HTTP API: controls MPC-HC and the PC master volume from a phone or tablet on the local network. The UI is in Russian.

## Features

- A large Pause/Play button; if MPC-HC is not running, the PC launches it.
- The time remaining in large type, with a seek slider, elapsed time and duration below: while dragging, the time shows where it will seek to, and MPC-HC seeks when the slider is released (`POST /seek?position_ms=`). The −1 min, −10 s, +10 s, +1 min buttons seek from MPC-HC's current position (`POST /seek?offset_ms=`). The new position is shown right away without waiting for the PC; if the command fails, the `/events` stream reconnects and brings back the real one.
- Previous/Next: the neighbouring file in MPC-HC (episode); disabled while the player is off.
- The PC master volume and MPC-HC's own volume: −/+ buttons and the current level (`MUTE` when muted).
- The "Кнопки громкости управляют ПК" ("Volume keys control the PC") setting: on by default on phones, off on tablets (`smallestScreenWidthDp ≥ 600`). When on, the volume keys in the app, the shade and on the lock screen change the PC volume (the media session declares remote volume, `setPlaybackToRemote`); when off, they change the device's own volume (`setPlaybackToLocal`). The PC volume down/up buttons in the notification always control the PC.
- A media notification (in the shade and on the lock screen): file name, time remaining (in minutes, updated once a minute and only while the screen is on), PC volume, play/pause, previous/next and PC volume down/up buttons, and a progress bar you can drag to seek (`ACTION_SEEK_TO`). It is run by the `RemoteService` foreground service with a `MediaSession`; the session volume is declared remote (`setPlaybackToRemote`) and tied to the PC volume.
- The service stops itself after MPC-HC has been idle for 30 minutes; it starts again the next time the app is opened.
- "Смотреть на этом устройстве" ("Watch on this device", `PlayerActivity`, Media3/ExoPlayer): the file open in MPC-HC plays on the tablet or phone in sync with the PC. The file comes from the PC via `GET /media` without transcoding; position, pause and episode come from the `/events` stream. Drift over 1.5 s is fixed by seeking, smaller drift by playing at 0.95–1.05× (`syncAction`); after an event from the PC (including a seek in MPC-HC) the player catches up right away, otherwise it seeks at most once every 5 s. The seek slider and buttons on the player screen seek MPC-HC, and the tablet follows it. Sound on the device is off by default (the "Звук на планшете" button), the screen stays on, landscape orientation.

## Requirements

- Android 13+ (`minSdk 33`). The notification permission is requested on first launch; without it there is no media notification, everything else works.
- volume_switch running on the PC with the API enabled (`API_LISTEN`, e.g. `API_LISTEN=:7778` in `%LOCALAPPDATA%\volume_switch\.env`) and the port open in the firewall; see ["Phone remote"](../README.md#phone-remote).
- The phone and the PC on the same network. Traffic is plain HTTP without TLS (hence `usesCleartextTraffic="true"` in the manifest); the API has no authentication.

## Setup

Enter the PC address in the field at the top of the screen; it is saved (`SharedPreferences`): `host:port` (`192.168.3.2:7778`, also the default) or a full URL; `http://` is added when there is no scheme. If the PC doesn't respond, "Нет связи с ПК" ("No connection to the PC") with the error text appears below the field.

There is no polling: the app keeps a `GET /events` (SSE) stream open and the PC pushes the state itself, right after connecting and then only on significant changes (pause, seek, another file, volume). The phone extrapolates the time remaining from the moment the event arrived; on screen it ticks every second while playing and the app is open. A dropped stream reconnects after 1 → 2 → 5 → 10 s; when the app opens or the screen turns on, right away (the connection may have died silently in Doze). A dead connection without pings is detected within 60 s (`readTimeout`). A server without `/events` (an old volume_switch) is not supported: the app says it needs updating. Commands respond with the same JSON as `/status`, so the screen updates right after a tap.

## Build and install

Requires JDK 17 and the Android SDK. The SDK path goes into `local.properties` (not in git):

```properties
sdk.dir=C\:\\Android\\Sdk
```

For everyday use, build the release: R8 (minification, unused code and resource removal), not debuggable, `arm64-v8a` only, with the Compose baseline profiles. It is signed with a key from `~/.gradle/gradle.properties` (outside the repository); without these properties, with the debug key:

```properties
volumeRemoteKeystore=C:/Users/<user>/.android/volumeremote-release.jks
volumeRemoteStorePassword=…
volumeRemoteKeyAlias=volumeremote
volumeRemoteKeyPassword=…
```

```sh
./gradlew testReleaseUnitTest assembleRelease
adb devices -l
adb -s <serial> install -r app/build/outputs/apk/release/app-release.apk
adb -s <serial> shell cmd package compile -m speed -f com.des1roer.volumeremote   # full AOT compilation for the device CPU right away
```

The debug and release builds are signed with different keys: to switch between them, uninstall the app first (`adb uninstall com.des1roer.volumeremote`). Losing the release key means the same for future updates. Debug build: `./gradlew assembleDebug`, APK in `app/build/outputs/apk/debug/`. `adb` is in the Android SDK's `platform-tools`.

Stack: AGP 8.7.3, Gradle 8.9 (wrapper), Kotlin 2.0.21, Jetpack Compose (Material 3, dynamic colors), Media3/ExoPlayer 1.5.0, no third-party HTTP/JSON libraries: `HttpURLConnection` and `org.json`.

**AC3/E-AC3 audio and licensing.** Many Android devices (e.g. the POCO Pad) have no Dolby Digital/Digital Plus decoder, while WEB-DL releases almost always use it. So the player adds a software FFmpeg decoder, the prebuilt `org.jellyfin.media3:media3-ffmpeg-decoder` (used only when there is no system decoder, `EXTENSION_RENDERER_MODE_ON`). It is licensed under **GPLv3**, so the built APK is distributed under GPLv3 terms as well; the source code in this repository stays MIT. The decoder version must match the Media3 version.

On the player screen the volume keys change the device's own volume (not the PC's, as on the main screen); if the player's sound is off, volume up turns it on.

## Code layout

`app/src/main/java/com/des1roer/volumeremote/`:

| File | Contents |
|------|----------|
| `RemoteClient.kt` | The API HTTP client (`RemoteClient`), response parsing (`parseStatus` → `Status`), `normalizeServer`, `errorMessage`, `formatTime` |
| `Remote.kt` | The `Remote` singleton: PC address, `StateFlow<UiState>`, the `/events` stream and commands. The stream is open while there is at least one user (`acquire`/`release`): the screen and the service |
| `RemoteScreen.kt` | The Compose screen |
| `MainActivity.kt` | The activity: theme, phone volume keys, notification permission request, starting `RemoteService` |
| `RemoteService.kt` | The foreground service with the `MediaSession` and the media notification |
| `VolumeRemoteApp.kt` | The `Application`: initializes `Remote` |
| `PlayerActivity.kt` | Watching the movie on the device: ExoPlayer, sync with MPC-HC |
| `VideoSync.kt` | Pure sync functions: `syncAction`, `mediaUrl` |

The client mirrors the `apiStatus` JSON format from `main.go` by hand: when the API changes, update `parseStatus` and `RemoteClientTest`.

The tests (`app/src/test`, JUnit 4) cover the pure functions of `RemoteClient.kt`: `parseStatus`, `SseParser`, `Status.positionAt`/`remainingAt`, `normalizeServer`, `errorMessage`, `formatTime`, `formatRemainingMinutes`.

This is a separate Gradle project: the repository's Go build and CI don't build or test it.
