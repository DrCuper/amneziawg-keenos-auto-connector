# Tasks

## 1. Каркас репозитория

- [x] 1.1 Инициализировать Go‑модуль `github.com/DrCuper/amneziawg-keenos-auto-connector`, `cmd/awgkeen`, `internal/`; проверить `go build ./...`
- [x] 1.2 Добавить Makefile (`build`, `test`, `lint`, `vet`), `.golangci.yml`, GitHub Actions (test+lint); проверить `make test lint`
- [x] 1.3 Добавить `internal/config` (YAML + env‑секреты, значения по умолчанию, валидация) с тестами на приоритет env и ошибки

## 2. Модель и чистая логика

- [x] 2.1 `internal/router`: типы `State`, `FQDNGroup`, `DNSRoute`, `Desired`, `Plan`; ошибки `TransportError`/`CommandError`
- [x] 2.2 `internal/router/cli`: парсер running-config (группы, маршруты, отступы, игнор неизвестного); табличные тесты по сценариям спеки
- [x] 2.3 `internal/router/cli`: генерация команд и `Diff(State, Desired, opts) Plan`; тесты: новая группа, частичное обновление, пустой план, маршрут, prune
- [x] 2.4 Валидация желаемого состояния (имя группы, формат домена/CIDR, лимит записей); тесты

## 3. Транспорты

- [x] 3.1 Интерфейс `Transport` + `internal/router/fake` (скриптуемые ответы, запись команд, инъекция ошибок)
- [x] 3.2 `internal/router/rci`: challenge‑response auth, cookie jar, `POST /rci/ parse`, разбор `status[]`, `GET /ci/running-config.txt`, переавторизация на 401; тесты на `httptest.Server`
- [x] 3.3 `internal/router/sshx`: exec‑сессии через `x/crypto/ssh`, пароль/ключ, known_hosts или insecure‑флаг; тесты сборки конфигурации (без реального сервера)

## 4. Клиент

- [x] 4.1 `internal/lock`: `sync.Mutex` + flock с таймаутом; тест на «занято»
- [x] 4.2 `Client`: `ReadState`, `Plan`, `Apply(dryRun)`, `Save`, бэкап running-config; ретраи с backoff; тесты с фейком по всем сценариям спеки (dry‑run, ошибка роутера, верификация, ретраи, блокировка, бэкап)

## 5. CLI и документация

- [x] 5.1 `awgkeen router show|plan|apply|save` через cobra; `--config`, `--desired`, `--dry-run`, `--no-save`; проверить `go run ./cmd/awgkeen router plan --desired examples/groups.yaml` против фейка/примеров
- [x] 5.2 README: установка, конфиг, примеры команд, env‑секреты; `config.example.yaml`, `examples/groups.yaml`

## Workflow follow-up

- Архивировать change после ревью PR (`openspec archive add-router-client`).
