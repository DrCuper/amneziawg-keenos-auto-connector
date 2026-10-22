# Proposal

## Why

Сейчас ключ AmneziaWG берётся только из приложения AmneziaVPN: оно само ставит
сервер на VPS и выдаёт `vpn://`‑ключи, а `awgkeen` (этап 4) лишь импортирует
готовый `.conf` на роутер. Чтобы поднимать свой сервер и выпускать к нему
ключи без приложения Amnezia — для роутера, телефонов и ноутбуков — нужен
этап 6 плана из `openspec/project.md`.

## What Changes

- **Серверы** (`servers:` в конфиге): VPS по SSH (ключ или пароль, проверка
  host key) или машина, на которой запущен `awgkeen` (`local: true`), с
  `sudo` для не‑root пользователя.
- **Инициализация**: ключи сервера, адрес в подсети, случайные параметры
  обфускации Jc/Jmin/Jmax/S1/S2/H1–H4 (опционально AWG 2.0: S3/S4 и диапазоны H).
  Состояние — `state_dir/servers/<name>.json` с правами 0600.
- **Установка** amneziawg на хост (Ubuntu — PPA Amnezia; другие ОС —
  инструкция).
- **Развёртывание**: рендер `awg-quick`‑конфига с NAT, план «что изменится»,
  запись через временный файл, `ip_forward`, подъём через
  `systemctl enable --now awg-quick@<if>`; изменения пиров применяются без
  разрыва (`awg syncconf`), смена ключей/параметров сервера — перезапуском.
- **Клиенты (пиры)**: выдача (следующий свободный адрес, ключи, PSK),
  отключение/включение, ротация ключей, удаление; после изменения — авто‑деплой.
  Клиентский конфиг: `.conf`, QR‑код (терминал и PNG) и `vpn://`‑ключ для
  AmneziaVPN. Выданный конфиг импортируется на роутер этапом 4.
- **Состояние сервера**: интерфейс, пиры с handshake и трафиком
  (`awg show dump`); проверка `server_down:<name>` в `serve`.
- **Журнал**: `server-init`, `server-deploy`, `peer-add/remove/rotate/disable/enable`.
- **CLI** `awgkeen server list|init|install|plan|deploy|status` и
  `awgkeen peer add|list|show|remove|disable|enable|rotate`.
- **API и UI**: `/api/v1/servers/...` (список, состояние, пиры, деплой;
  выгрузка конфигов только с полным токеном) и вкладка «Сервер».

Вне scope: IPv6 внутри туннеля, несколько интерфейсов на одном сервере в
одной записи, установка на не‑Ubuntu системы, лимиты трафика и сроки действия
ключей, автоматическое создание VPS у хостинг‑провайдера.

## Capabilities

### New Capabilities
- `awg-server`: управление сервером AmneziaWG и его клиентами.

### Modified Capabilities
(нет — API и UI добавляют разделы поверх существующих требований; журнал
получает новые виды записей, уведомления — новый алерт `server_down`)

## Impact

- Новые пакеты `internal/remote` (выполнение скриптов локально и по SSH) и
  `internal/server` (конфиг, ключи, состояние, рендер, деплой, статус).
- `internal/app`: `Server`, `PeerOp`, `DeployServer`, `InitServer`, проверка
  `health.ServerCheck`; `internal/api/servers.go`; вкладка в `web/index.html`.
- `journal.Entry` получает поля `server`, `peer`; `notify.AlertServerDown`.
- Конфиг: секция `servers`. Новая зависимость `github.com/skip2/go-qrcode`.
