# Пульт MPC: взаимодействие телефона и ПК

Как Android-приложение «Пульт MPC» (`android/`) работает с volume_switch на ПК. Те же схемы в PlantUML — в [`plantuml/`](plantuml/).

Сопряжения в смысле обмена ключами нет: API без авторизации (решение владельца, см. `CLAUDE.md`). «Сопряжение» — это адрес ПК, введённый в приложении (`SharedPreferences`, по умолчанию `192.168.3.2:7778`), и открытый порт API на ПК (`API_LISTEN=:7778`, правило брандмауэра для частной сети).

## 1. Компоненты

```mermaid
flowchart LR
    subgraph phone["Телефон (Android 13+)"]
        screen["MainActivity<br/>RemoteScreen"]
        service["RemoteService<br/>foreground, mediaPlayback"]
        session["MediaSession<br/>шторка и экран блокировки"]
        remote["Remote<br/>синглтон: StateFlow, поток /events, команды"]
        client["RemoteClient<br/>HttpURLConnection + SseParser"]
        prefs[("SharedPreferences<br/>адрес ПК")]
    end

    subgraph pc["ПК (Windows)"]
        subgraph vs["volume_switch"]
            api["remoteAPI.handler<br/>HTTP :7778, API_LISTEN"]
            hub["statusHub<br/>общий опросчик 500 мс<br/>+ statusChanged"]
            mpc["mpcClient"]
            sw["Switcher<br/>WASAPI/COM"]
            overlay["pollVolume<br/>оверлей громкости"]
        end
        mpchc["MPC-HC<br/>web-интерфейс :7777"]
        audio["Windows Audio"]
    end

    screen -- "acquire(ui) / release, команды" --> remote
    service -- "acquire / release, reconnect при включении экрана" --> remote
    remote -- StateFlow --> screen
    remote -- StateFlow --> service
    service -- "метаданные, PlaybackState" --> session
    session -- "кнопки: пред., пауза/старт, след., тише/громче" --> service
    remote --> prefs
    remote --> client
    client -. "GET /events (SSE), POST команды<br/>Wi-Fi, HTTP без TLS" .-> api

    api -- subscribe --> hub
    hub -- "события apiStatus" --> api
    api --> mpc
    api --> sw
    hub -- "State()" --> mpc
    hub -- "CurrentVolume()" --> sw
    mpc -- "status.json, command.html?wm_command=N" --> mpchc
    sw --> audio
    overlay -- "CurrentVolume() каждые 120 мс" --> sw
```

## 2. Подключение и поток `/events` (SSE)

Телефон не опрашивает ПК: держит один поток `GET /events`, и ПК присылает состояние сам — сразу при подключении и затем только при значимых изменениях. Ход воспроизведения событий не порождает: оставшееся время телефон досчитывает сам от момента получения события.

```mermaid
sequenceDiagram
    autonumber
    participant ui as MainActivity / RemoteService
    participant remote as Remote
    participant client as RemoteClient
    participant api as volume_switch remoteAPI.events
    participant hub as statusHub
    participant mpc as MPC-HC :7777
    participant sw as Switcher (WASAPI)

    ui->>remote: acquire(ui = true)
    remote->>remote: reconnect(): отменить старый поток, запустить stream()
    remote->>client: events()
    client->>api: GET /events (Accept: text/event-stream)
    api->>mpc: GET status.json
    api->>sw: CurrentVolume()
    api->>hub: subscribe(текущее состояние)
    opt первый подписчик
        hub->>hub: запустить опросчик (тик 500 мс), базовое состояние = текущее
    end
    api-->>client: data: apiStatus (сразу после подключения)
    client-->>remote: Status
    remote->>remote: receivedAt = elapsedRealtime(), error = null
    remote-->>ui: StateFlow UiState

    loop каждые 500 мс, пока есть подписчики
        hub->>mpc: GET status.json
        hub->>sw: CurrentVolume()
        hub->>hub: statusChanged(last, new, elapsed)
        alt значимое изменение: пауза/старт, файл, перемотка больше 2 с, громкость, MPC-HC запущен/закрыт
            hub-->>api: apiStatus
            api-->>client: data: apiStatus
            client-->>remote: Status
            remote-->>ui: StateFlow UiState
        else обычный ход воспроизведения
            hub->>hub: ничего не рассылать
        end
    end

    loop каждые 25 с
        api-->>client: комментарий ": ping"
    end

    Note over ui: Экран тикает раз в секунду локально:<br/>remaining = duration − (position + now − receivedAt).<br/>Уведомление: «Осталось N мин» раз в минуту, только при включённом экране.
```

## 3. Команда с телефона

Кнопка на экране, в шторке или на экране блокировки отправляет `POST`. Ответ — сразу новое `apiStatus`, поэтому экран обновляется без ожидания события; затем то же изменение приходит и по SSE. Так же за ~0,5 с доходят изменения, сделанные на самом ПК (F5-оверлей, клавиши, мышь в MPC-HC).

```mermaid
sequenceDiagram
    autonumber
    actor user as Пользователь
    participant ui as RemoteScreen / MediaSession
    participant remote as Remote
    participant client as RemoteClient
    participant api as volume_switch remoteAPI
    participant mpc as mpcClient
    participant mpchc as MPC-HC :7777
    participant hub as statusHub

    user->>ui: «Пауза» (экран, шторка или экран блокировки)
    ui->>remote: playPause()
    remote->>client: playPause()
    client->>api: POST /playpause
    api->>mpc: TogglePlayback()
    mpc->>mpchc: GET command.html?wm_command=889
    opt MPC-HC не отвечает
        mpc->>mpc: Launch() — запустить mpc-hc64.exe
    end
    api->>mpchc: GET status.json
    api-->>client: 200 apiStatus
    client-->>remote: Status
    remote-->>ui: StateFlow — экран обновился сразу

    hub->>mpchc: очередной тик: GET status.json
    hub->>hub: statusChanged: state изменился
    hub-->>client: data: apiStatus (по SSE)
```

Остальные команды:

| Запрос | Что делает на ПК |
|--------|------------------|
| `POST /next`, `POST /prev` | `wm_command` 920/919 — следующий/предыдущий файл; 503, если MPC-HC не запущен |
| `POST /volume/up`, `POST /volume/down` (`?step=N`) | Системная громкость через `Switcher.StepVolume`; на ПК показывается оверлей громкости. Сюда же — кнопки громкости телефона в открытом приложении и тише/громче в уведомлении |
| `POST /mpc/volume/up`, `POST /mpc/volume/down` | `wm_command` 907/908 — громкость самого MPC-HC |

## 4. Жизненный цикл потока на телефоне

```mermaid
stateDiagram-v2
    [*] --> Idle
    Idle: Нет пользователей
    Connecting: Подключение (GET /events, connectTimeout 2 с)
    Streaming: Поток открыт (readTimeout 60 с, пинг сервера раз в 25 с)
    Backoff: Пауза перед повтором 1 → 2 → 5 → 10 с

    Idle --> Connecting: acquire() — экран или сервис
    Connecting --> Streaming: первое событие
    Connecting --> Backoff: ошибка соединения или 404
    Streaming --> Backoff: обрыв, EOF, таймаут чтения
    Backoff --> Connecting: пауза истекла
    Streaming --> Connecting: reconnect() — открыто приложение, включился экран, сменён адрес
    Backoff --> Connecting: reconnect()
    Connecting --> Idle: release() последнего пользователя
    Streaming --> Idle: release() последнего пользователя
    Backoff --> Idle: release() последнего пользователя
```

Пользователи потока — `MainActivity` (`onStart`/`onStop`) и `RemoteService` (`onCreate`/`onDestroy`). Сервис останавливается сам, если 30 минут ничего не воспроизводится, и запускается снова при открытии приложения. Пауза перед повтором сбрасывается после первого полученного события; 404 означает старый volume_switch без `/events` — приложение просит его обновить.

## 5. Опросчик на ПК и остановка API

```mermaid
sequenceDiagram
    autonumber
    participant client as Клиент /events
    participant handler as remoteAPI.events
    participant hub as statusHub
    participant exit as trayApp.onExit
    participant srv as http.Server
    participant sw as Switcher

    rect rgba(128, 128, 128, 0.1)
        Note over client, hub: Подписчики приходят и уходят
        client->>handler: GET /events
        handler->>hub: subscribe() — первый подписчик запускает опросчик (500 мс)
        client->>handler: отключение
        handler->>hub: unsubscribe()
        hub->>hub: на следующем тике подписчиков нет — опросчик и тикер остановлены
    end

    rect rgba(128, 128, 128, 0.1)
        Note over exit, sw: Выход из программы («Выход» в трее)
        exit->>srv: Shutdown(ctx, 2 с)
        srv->>srv: RegisterOnShutdown → cancel() отменяет BaseContext запросов
        srv-->>handler: r.Context().Done()
        handler->>hub: unsubscribe()
        handler-->>client: поток закрыт
        srv-->>exit: Shutdown завершён без ожидания таймаута
        exit->>sw: Close()
    end

    Note over exit, sw: API останавливается до switcher.Close():<br/>иначе обработчик, обратившийся к закрытому Switcher, повис бы в exec.
```
