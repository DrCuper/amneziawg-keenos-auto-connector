# Proposal

## Why

Данил просит две вещи: импортировать ключи администратора Amnezia («полный
доступ» в AmneziaVPN), чтобы выдавать ключи на уже установленном приложением
сервере без самого приложения, и чтобы `awgkeen awg import` по умолчанию
только добавлял ключ в `awgkeen`, не меняя роутер.

## What Changes

- **`awg import` не трогает роутер по умолчанию.** Ключ клиента сохраняется
  как туннель; на роутер — флагом `--apply` или позже `awg apply NAME`. То же
  в API (`apply: false` по умолчанию) и в веб‑интерфейсе (галочка).
- **Ключ администратора.** `awg import NAME vpn://…` (или `server import`)
  распознаёт ключ с SSH‑доступом (`hostName`, `userName`, `password`, `port`)
  и регистрирует сервер `NAME` в `state_dir/servers/NAME.server.yaml` (0600;
  приватный SSH‑ключ — в `NAME.key`). Host key запоминается при первом
  подключении (TOFU) в `NAME.known_hosts`; не‑root пользователь — через `sudo`.
- **Сервер Amnezia** (`amnezia: true`, доступно и в `servers:` конфига):
  `server init` читает контейнер `amnezia-awg` (Docker): ключи сервера,
  адрес, порт и параметры обфускации из `/opt/amnezia/awg/wg0.conf`. Клиенты
  `awgkeen` добавляются в этот файл с пометкой `# awgkeen: <имя>`; секция
  `[Interface]` и клиенты приложения не меняются, адреса не пересекаются.
  Применение — `wg syncconf` в контейнере, без перезапуска. Если приложение
  переустановило сервер (ключи сменились), деплой останавливается.
- `peer add -s NAME --as-tunnel` выдаёт клиента и сразу добавляет его
  туннелем для своего роутера (без применения).
- `server remove NAME` (и `DELETE /api/v1/servers/{s}`, кнопка в UI) забывает
  импортированный сервер; выданные ключи продолжают работать.

Вне scope: синхронизация `clientsTable` приложения (клиенты `awgkeen` в
списке AmneziaVPN не видны по имени), установка протоколов через приложение,
не‑AWG контейнеры.

## Capabilities

### New Capabilities
(нет)

### Modified Capabilities
- `awg-connection`: импорт ключа не применяет его на роутере без запроса.
- `awg-server`: серверы, установленные приложением AmneziaVPN, по ключу администратора.

## Impact

- `internal/server/amnezia.go`: разбор ключа администратора, поиск контейнера,
  чтение и слияние конфига, деплой через `docker exec`; `Params.I`;
  `State.Amnezia`; `Config.Amnezia/Container/TrustOnFirstUse`.
- `internal/remote`: TOFU для host key.
- `internal/app/admin.go`: реестр импортированных серверов, `ImportKey`,
  `ForgetServer`; `ServerNames/ServerConfig` объединяют конфиг и реестр.
- `internal/awg.DecodeVPNDocument`; API `KeyImport`, `DELETE /servers/{s}`; UI.
