# Пульт MPC для часов Zepp OS

*English version: [below](#english).*

Мини-приложение Zepp OS — пульт [volume_switch](../README.md#пульт-с-телефона) на часах. Проверено на Amazfit Balance 3 (круглый экран 480×480, Zepp OS 6). В `app.json` указан API 4.0+ и только круглые экраны 480 px.

## Возможности

- Название файла, крупно — сколько осталось, ниже позиция и длительность.
- Кнопки «−10 с», «Пауза/Старт», «+10 с», «Пред.», «F5» (оверлей прогресса на ПК), «След.», громкость ПК «−»/«+».
- Пока приложение открыто, состояние запрашивается раз в 2 с, время на часах тикает раз в секунду.

## Как это работает

Часы сами в сеть не ходят. Страница на часах (`page/index.js`) отправляет команду по Bluetooth в side service (`app-side/index.js`), который работает внутри приложения Zepp на телефоне, а он делает HTTP-запрос к API volume_switch и возвращает часам сжатый статус (`compactStatus`). Без телефона рядом пульт не работает. Общая логика (адреса команд, разбор ответа, формат времени) — в `shared/remote.js`, её тесты — `npm test`.

Адрес ПК по умолчанию — `192.168.3.2:7778`. Меняется в настройках мини-приложения в Zepp (модуль `setting`).

## Сборка и установка

Нужны Node.js и Zeus CLI (`npm i -g @zeppos/zeus-cli`).

```sh
cd zepp
npm install
npm test
zeus login                               # аккаунт Zepp (почта и пароль)
zeus preview -t "Amazfit Balance 3"      # собирает, загружает и печатает QR-код
```

QR-код сканируется в приложении Zepp в режиме разработчика:

- включить режим: Профиль → Настройки → «О приложении» → 7 раз нажать на логотип Zepp;
- сканер: Устройство → «Основные» → «Режим разработчика» → «+» → «Scan».

Обычная камера не подойдёт: ссылка `zpkd1://` открывается только сканером Zepp. QR-код действует неделю.

Ресурсы лежат в `assets/<цель>.<форма экрана>` (`assets/round.r` для цели `round` и `st: "r"`): с другим именем каталога `zeus build` не находит иконку.

---

<a id="english"></a>

# MPC remote for Zepp OS watches

A Zepp OS mini program: the [volume_switch](../README.md#phone-remote) remote on a watch. Tested on the Amazfit Balance 3 (round 480×480 screen, Zepp OS 6). `app.json` targets API 4.0+ and round 480 px screens only.

## Features

- The file name, the time remaining in large type, the position and duration below it.
- −10 s, Pause/Play, +10 s, Prev, F5 (the progress overlay on the PC), Next, and PC volume −/+ buttons.
- While the app is open, the state is requested every 2 s and the time on the watch ticks every second.

## How it works

The watch has no network access of its own. The watch page (`page/index.js`) sends a command over Bluetooth to the side service (`app-side/index.js`), which runs inside the Zepp app on the phone; the side service calls the volume_switch API over HTTP and returns a compact status (`compactStatus`) to the watch. The remote does not work without the phone nearby. Shared logic (command paths, response parsing, time format) is in `shared/remote.js`, tested with `npm test`.

The default PC address is `192.168.3.2:7778`. Change it in the app settings in Zepp (the `setting` module).

## Build and install

Requires Node.js and the Zeus CLI (`npm i -g @zeppos/zeus-cli`).

```sh
cd zepp
npm install
npm test
zeus login                               # Zepp account (email and password)
zeus preview -t "Amazfit Balance 3"      # builds, uploads and prints a QR code
```

Scan the QR code in the Zepp app in developer mode:

- enable it: Profile → Settings → About → tap the Zepp logo 7 times;
- the scanner: Device → General → Developer mode → "+" → Scan.

A regular camera won't do: the `zpkd1://` link only opens in the Zepp scanner. The QR code is valid for a week.

Assets live in `assets/<target>.<screen shape>` (`assets/round.r` for the `round` target with `st: "r"`); with any other directory name `zeus build` can't find the icon.
