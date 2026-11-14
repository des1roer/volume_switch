# Пульт MPC (Android)

*English version: [below](#english).*

Android-клиент HTTP API пульта [volume_switch](../README.md#пульт-с-телефона): управляет MPC-HC и громкостью ПК с телефона или планшета в локальной сети.

## Возможности

- Большая кнопка «Пауза/Старт»; если MPC-HC не запущен, ПК запускает его.
- Крупно — сколько осталось до конца файла, ниже полоса прогресса с прошедшим временем и длительностью.
- «Предыдущая»/«Следующая» — соседний файл в MPC-HC (серия); кнопки неактивны, пока плеер выключен.
- Громкость ПК и собственная громкость MPC-HC: кнопки −/+ и текущий уровень (`MUTE` при выключенном звуке).
- Кнопки громкости телефона в открытом приложении меняют громкость ПК, а не телефона.
- Медиауведомление (в шторке и на экране блокировки): название файла, сколько осталось, громкость ПК, кнопки пауза/старт, предыдущая/следующая и тише/громче ПК. Им управляет foreground-сервис `RemoteService` с `MediaSession`; громкость сессии объявлена удалённой (`setPlaybackToRemote`) и привязана к громкости ПК.
- Сервис останавливается сам, если MPC-HC 30 минут ничего не воспроизводит; при следующем открытии приложения запускается снова.

## Требования

- Android 13+ (`minSdk 33`). Разрешение на уведомления запрашивается при первом запуске — без него медиауведомления не будет, остальное работает.
- На ПК запущен volume_switch с включённым API (`API_LISTEN`, например `API_LISTEN=:7778` в `%LOCALAPPDATA%\volume_switch\.env`) и открытым портом в брандмауэре — см. [«Пульт с телефона»](../README.md#пульт-с-телефона).
- Телефон и ПК в одной сети. Трафик — обычный HTTP без TLS (поэтому в манифесте `usesCleartextTraffic="true"`), авторизации у API нет.

## Настройка

Адрес ПК вводится в поле вверху экрана и сохраняется (`SharedPreferences`): `хост:порт` (`192.168.3.2:7778`, это же значение по умолчанию) или полный URL — без схемы подставляется `http://`. Если ПК не отвечает, под полем появляется «Нет связи с ПК» с текстом ошибки.

Состояние опрашивается через `GET /status` раз в секунду, пока приложение на экране или идёт воспроизведение, и раз в 5 секунд в остальное время. Команды отвечают тем же JSON, что и `/status`, поэтому экран обновляется сразу после нажатия.

## Сборка и установка

Нужны JDK 17 и Android SDK. Путь к SDK — в `local.properties` (не в git):

```properties
sdk.dir=C\:\\Android\\Sdk
```

```sh
./gradlew testDebugUnitTest assembleDebug
adb devices -l
adb -s <serial> install -r app/build/outputs/apk/debug/app-debug.apk
```

APK — `app/build/outputs/apk/debug/app-debug.apk`. `adb` лежит в `platform-tools` Android SDK.

Стек: AGP 8.7.3, Gradle 8.9 (wrapper), Kotlin 2.0.21, Jetpack Compose (Material 3, динамические цвета), без сторонних HTTP/JSON-библиотек — `HttpURLConnection` и `org.json`.

## Устройство кода

`app/src/main/java/com/des1roer/volumeremote/`:

| Файл | Что внутри |
|------|------------|
| `RemoteClient.kt` | HTTP-клиент API (`RemoteClient`), разбор ответа (`parseStatus` → `Status`), `normalizeServer`, `errorMessage`, `formatTime` |
| `Remote.kt` | Синглтон `Remote`: адрес ПК, `StateFlow<UiState>`, опрос и команды. Опрос идёт, пока есть хотя бы один пользователь (`acquire`/`release`): экран и сервис |
| `RemoteScreen.kt` | Экран на Compose |
| `MainActivity.kt` | Активити: тема, кнопки громкости телефона, запрос разрешения на уведомления, запуск `RemoteService` |
| `RemoteService.kt` | Foreground-сервис с `MediaSession` и медиауведомлением |
| `VolumeRemoteApp.kt` | `Application`: инициализирует `Remote` |

Клиент повторяет формат JSON `apiStatus` из `main.go` вручную: при изменении API обновить `parseStatus` и `RemoteClientTest`.

Тесты (`app/src/test`, JUnit 4) покрывают чистые функции `RemoteClient.kt`: `parseStatus`, `normalizeServer`, `errorMessage`, `formatTime`.

Это отдельный Gradle-проект: Go-сборка и CI репозитория его не собирают и не тестируют.

---

<a id="english"></a>

# MPC Remote (Android)

A client for the [volume_switch](../README.md#phone-remote) phone remote HTTP API: controls MPC-HC and the PC master volume from a phone or tablet on the local network. The UI is in Russian.

## Features

- A large Pause/Play button; if MPC-HC is not running, the PC launches it.
- The time remaining in large type, with a progress bar, elapsed time and duration below.
- Previous/Next: the neighbouring file in MPC-HC (episode); disabled while the player is off.
- The PC master volume and MPC-HC's own volume: −/+ buttons and the current level (`MUTE` when muted).
- While the app is open, the phone's volume keys change the PC volume instead of the phone's.
- A media notification (in the shade and on the lock screen): file name, time remaining, PC volume, play/pause, previous/next and PC volume down/up buttons. It is run by the `RemoteService` foreground service with a `MediaSession`; the session volume is declared remote (`setPlaybackToRemote`) and tied to the PC volume.
- The service stops itself after MPC-HC has been idle for 30 minutes; it starts again the next time the app is opened.

## Requirements

- Android 13+ (`minSdk 33`). The notification permission is requested on first launch; without it there is no media notification, everything else works.
- volume_switch running on the PC with the API enabled (`API_LISTEN`, e.g. `API_LISTEN=:7778` in `%LOCALAPPDATA%\volume_switch\.env`) and the port open in the firewall; see ["Phone remote"](../README.md#phone-remote).
- The phone and the PC on the same network. Traffic is plain HTTP without TLS (hence `usesCleartextTraffic="true"` in the manifest); the API has no authentication.

## Setup

Enter the PC address in the field at the top of the screen; it is saved (`SharedPreferences`): `host:port` (`192.168.3.2:7778`, also the default) or a full URL; `http://` is added when there is no scheme. If the PC doesn't respond, "Нет связи с ПК" ("No connection to the PC") with the error text appears below the field.

The state is polled with `GET /status` every second while the app is on screen or something is playing, and every 5 seconds otherwise. Commands respond with the same JSON as `/status`, so the screen updates right after a tap.

## Build and install

Requires JDK 17 and the Android SDK. The SDK path goes into `local.properties` (not in git):

```properties
sdk.dir=C\:\\Android\\Sdk
```

```sh
./gradlew testDebugUnitTest assembleDebug
adb devices -l
adb -s <serial> install -r app/build/outputs/apk/debug/app-debug.apk
```

The APK is `app/build/outputs/apk/debug/app-debug.apk`. `adb` is in the Android SDK's `platform-tools`.

Stack: AGP 8.7.3, Gradle 8.9 (wrapper), Kotlin 2.0.21, Jetpack Compose (Material 3, dynamic colors), no third-party HTTP/JSON libraries: `HttpURLConnection` and `org.json`.

## Code layout

`app/src/main/java/com/des1roer/volumeremote/`:

| File | Contents |
|------|----------|
| `RemoteClient.kt` | The API HTTP client (`RemoteClient`), response parsing (`parseStatus` → `Status`), `normalizeServer`, `errorMessage`, `formatTime` |
| `Remote.kt` | The `Remote` singleton: PC address, `StateFlow<UiState>`, polling and commands. Polling runs while there is at least one user (`acquire`/`release`): the screen and the service |
| `RemoteScreen.kt` | The Compose screen |
| `MainActivity.kt` | The activity: theme, phone volume keys, notification permission request, starting `RemoteService` |
| `RemoteService.kt` | The foreground service with the `MediaSession` and the media notification |
| `VolumeRemoteApp.kt` | The `Application`: initializes `Remote` |

The client mirrors the `apiStatus` JSON format from `main.go` by hand: when the API changes, update `parseStatus` and `RemoteClientTest`.

The tests (`app/src/test`, JUnit 4) cover the pure functions of `RemoteClient.kt`: `parseStatus`, `normalizeServer`, `errorMessage`, `formatTime`.

This is a separate Gradle project: the repository's Go build and CI don't build or test it.
