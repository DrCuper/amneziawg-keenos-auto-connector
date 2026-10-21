# Tasks

## 1. Источники

- [x] 1.1 `internal/source`: `Parse(format, text) (entries, stats)` для plain/hosts/dnsmasq/v2fly/clash/URL с автоопределением; табличные тесты по сценариям спеки
- [x] 1.2 `internal/source`: `Fetcher` (HTTP с таймаутом, лимитом размера, ETag/Last-Modified, кэш через `store.Cache`; локальные файлы относительно spec‑файла); тесты на `httptest.Server` (200, 304, 5xx с кэшем и без, превышение размера)

## 2. Группы и нормализация

- [x] 2.1 `internal/domains`: `Spec` (connections, groups), `Load(path)` с валидацией (уникальность, ссылки на подключения, маршруты как в `router.RouteSpec`); тесты
- [x] 2.2 `internal/domains`: `Normalize` (регистр, точка, IDN, фильтр служебных имён, `router.ValidateEntry`, дедуп, схлопывание поддоменов); тесты
- [x] 2.3 `internal/domains`: `Build(group, sources, manual, limits) → Built{Entries, Exclude, Stats, Hash}` с приоритетом и `on_limit`; тесты (лимит error/truncate, remove побеждает, порядок)

## 3. Состояние

- [x] 3.1 `internal/store`: интерфейс `Store` (GroupState, Versions, SaveVersion, SetApplied/SetPinned, Manual add/remove/clear, Cache get/put) и файловая реализация с атомарной записью и ротацией версий; тесты (первый запуск, повреждённый файл одной группы, keep_versions)

## 4. Синхронизация

- [x] 4.1 `internal/syncer`: `Syncer.Run(ctx, Options{DryRun, Force, Groups}) → Result`: сборка всех групп, версии, shrink‑guard, `Desired` из подключений, `Client.Apply`, applied; тесты с `router/fake` по сценариям спеки (успех, ошибка группы, ошибка применения, dry-run, shrink, force, идемпотентность)
- [x] 4.2 `internal/syncer`: `Rollback(ctx, group, to)` / `Unpin`; тесты
- [x] 4.3 `internal/syncer`: `Scheduler` на `robfig/cron/v3` (run_on_start, skip-if-running, graceful stop); тесты на разбор выражения и пропуск пересечения

## 5. Конфиг, CLI, документация

- [x] 5.1 `internal/config`: `state_dir`, секция `sync` (domains_file, schedule, run_on_start, max_shrink, on_limit, keep_versions, source_timeout, source_max_bytes, user_agent); валидация cron при `serve`; тесты
- [x] 5.2 `cmd/awgkeen`: `sync [--dry-run] [--force] [--group]`, `serve`, `groups list|show|history|add|remove|clear|rollback|unpin`; `internal/app` собирает Store/Fetcher/Syncer
- [x] 5.3 README и `config.example.yaml`, `examples/domains.yaml` с подключениями и источниками; `openspec validate --strict`, `make test lint`

## Workflow follow-up

- Архивировать change после ревью PR (`openspec archive add-domain-groups-sync`).
