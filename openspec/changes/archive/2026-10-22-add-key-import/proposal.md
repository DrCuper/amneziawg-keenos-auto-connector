# Proposal

## Why

Туннели этапа 4 задаются только в `config.yaml` (`awg.tunnels`): чтобы
подключить роутер к уже работающему серверу Amnezia (ключ из AmneziaVPN, от
друга или провайдера), нужно положить файл и править конфиг. Данил просит
импортировать «свой ключ» напрямую и подключать так несколько серверов.

## What Changes

- `awgkeen awg import NAME FILE|vpn://…|-`: ключ (`.conf` или `vpn://`)
  проверяется, сохраняется в `state_dir/tunnels/NAME.conf` (0600) и сразу
  импортируется на роутер (`--no-apply` — только сохранить, `--replace` —
  заменить ключ, например после перевыпуска на сервере).
- Импортированные ключи — обычные туннели: свой `Wireguard<N>`, проверка
  `awg_down:<name>`, маршруты групп `tunnel: NAME`, импорт при старте `serve`.
  Несколько ключей = несколько серверов; имена из конфига заняты.
- `awg remove NAME` для импортированного туннеля удаляет интерфейс и ключ.
- API: `POST /api/v1/tunnels/import`, `DELETE /api/v1/tunnels/{name}`; в
  списке туннелей признак `imported`. Во вкладке «Туннели» — форма «Добавить
  ключ» и удаление.
- Проверки здоровья в `serve` пересобираются каждый раунд, чтобы туннели,
  добавленные во время работы, тоже проверялись.

## Capabilities

### New Capabilities
(нет)

### Modified Capabilities
- `awg-connection`: туннели можно добавлять ключом во время работы.

## Impact

- `internal/awg`: `KeyStore`, `Tunnel.Imported`, `Manager.List/Put/Drop`
  (список туннелей под мьютексом).
- `internal/app`: `Keys`, `ImportTunnel`, `ForgetTunnel`; синхронизация и API
  всегда получают менеджер туннелей.
- `health.Monitor.ChecksFunc`; `internal/api` — два эндпоинта; UI.
