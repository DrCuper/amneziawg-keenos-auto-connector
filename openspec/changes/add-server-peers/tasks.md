# Tasks

## 1. Выполнение команд

- [x] 1.1 `internal/remote`: `Runner`, `Local` (sudo), `SSH` (known_hosts / insecure, ключ или пароль), `ExitError`, `ShellQuote`

## 2. Сервер

- [x] 2.1 `internal/server`: конфиг `servers[]` с умолчаниями и валидацией
- [x] 2.2 Ключи (curve25519, PSK), параметры обфускации AWG 1.x/2.0
- [x] 2.3 Состояние `servers/<name>.json` (0600, flock), выдача адресов
- [x] 2.4 Рендер серверного и клиентского конфига, QR (PNG/терминал), `vpn://`
- [x] 2.5 Init, пиры (add/remove/disable/enable/rotate), план и деплой (syncconf/перезапуск), статус `awg show dump`, установка, проверка

## 3. Интеграция

- [x] 3.1 `config.servers`, маскирование паролей; `app.Server/PeerOp/DeployServer/InitServer`
- [x] 3.2 Журнал: виды `server-*`/`peer-*`, поля `server`/`peer`; алерт `server_down`, `health.ServerCheck`
- [x] 3.3 CLI `server …`, `peer …`
- [x] 3.4 API `/api/v1/servers/...` (выгрузка конфигов — только полный токен), вкладка «Сервер» в UI

## 4. Проверка и документация

- [x] 4.1 Тесты: фейковый хост (деплой, syncconf/перезапуск, статус), параметры, адреса, `vpn://` и `.conf` разбираются импортом этапа 4, API, журнал, конфиг
- [x] 4.2 Проверка UI в headless Chromium
- [x] 4.3 README, `config.example.yaml`, `openspec/project.md`; `openspec validate --strict`, `go vet`, `go test -race`, `golangci-lint`
