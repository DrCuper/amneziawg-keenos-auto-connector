# Tasks

## 1. Модель роутера

- [x] 1.1 `internal/router`: `WGInterface`, `WGPeer`, канонические префиксы, валидация; `State.Interfaces`, `Desired.Interfaces/RemoveInterfaces`, `RouteSpec.Tunnel`
- [x] 1.2 `Plan`: изменения интерфейсов, write‑only изменения и секреты (`Redact`, `RedactErr`, маскирование в `String`); верификация игнорирует write‑only; лог ретраев без секретов
- [x] 1.3 `internal/router/cli`: разбор `interface Wireguard<N>` (пиры, ASC, адрес/маска), команды и дифф интерфейсов, удаление; тесты
- [x] 1.4 Статус: `WGStatus`, `ParseWGStatus`, `cli.ParseShow` (колонка двоеточия), `InterfaceStatus` в RCI/SSH/фейке, `Client.InterfaceStatus`; тесты

## 2. Импорт AWG

- [x] 2.1 `internal/awg`: разбор `.conf` и `vpn://`, предупреждения, вывод публичного ключа; тесты
- [x] 2.2 `Tunnel` (YAML), валидация, построение желаемого интерфейса
- [x] 2.3 `Manager`: автовыбор/резерв интерфейса, `adopt`, отпечаток ключей, `Apply`, `Remove`, `Status`, `Evaluate`; тесты на фейковом роутере

## 3. Интеграция

- [x] 3.1 `internal/syncer`: `ResolveTunnel` для `tunnel:` маршрутов; `internal/domains`: валидация `tunnel`
- [x] 3.2 `internal/health`: `AWGCheck` (`awg_down:<tunnel>`); `internal/app`: `Tunnels()`, проверки, резолвер
- [x] 3.3 `internal/config`: секция `awg`, конфликт имён с `health.checks`, маскирование inline‑конфига
- [x] 3.4 `internal/journal`: виды `awg-apply`/`awg-remove`, `Change.Kind`; `journal show` выводит интерфейсы
- [x] 3.5 CLI: `awg inspect|list|plan|apply|remove|status`; `serve` импортирует туннели при старте
- [x] 3.6 README, `config.example.yaml`, `examples/`, `openspec/project.md`; `openspec validate --strict`, `go vet`, `go test -race`
