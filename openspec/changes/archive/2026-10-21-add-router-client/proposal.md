# Proposal

## Why

Всё остальное в сервисе (синхронизация доменных групп, импорт AWG, веб‑интерфейс)
упирается в один компонент — надёжный клиент роутера Keenetic. Без него нельзя
ни прочитать текущее состояние, ни безопасно (dry‑run, бэкап, блокировка)
применить изменения. Это этап 1 плана из `openspec/project.md`.

## What Changes

- Каркас репозитория: Go‑модуль, `cmd/awgkeen`, `internal/`, YAML‑конфиг,
  Makefile, golangci‑lint, GitHub Actions.
- Новый пакет `internal/router` — клиент роутера:
  - транспорты **RCI** (HTTP JSON‑API с challenge‑response авторизацией) и **SSH**;
    учётные данные из конфига или переменных окружения;
  - чтение `running-config` и разбор секций `object-group fqdn` и `dns-proxy route`;
  - операции: создание/обновление/удаление `object-group fqdn` (include/exclude,
    description), привязка/отвязка `dns-proxy route object-group`, `system configuration save`;
  - **dry‑run**: план изменений (команды + дифф) без применения;
  - защита: таймауты на каждую операцию, ретраи транспортных ошибок с backoff,
    блокировка от параллельных изменений (внутрипроцессная + файловая),
    бэкап running‑config перед применением;
  - проверка после применения: состояние перечитывается и сравнивается с желаемым.
- CLI‑подкоманды `awgkeen router show`, `awgkeen router plan`, `awgkeen router apply`
  (с `--dry-run`), `awgkeen router save`.
- Юнит‑тесты с фейковым транспортом; тесты не требуют сети.

Вне scope: источники доменов, нормализация списков, cron, журнал, уведомления,
импорт AWG, веб‑интерфейс. Они приходят отдельными change'ами, но API клиента
проектируется так, чтобы они на него легли без переделки.

## Capabilities

### New Capabilities
- `router-client`: чтение и изменение конфигурации Keenetic (object-group fqdn,
  dns-proxy route, сохранение) через SSH/RCI с dry‑run, бэкапом, ретраями и блокировкой.

### Modified Capabilities
(нет)

## Impact

- Новые пакеты: `internal/router`, `internal/router/{rci,sshx,cli,fake}`,
  `internal/config`, `internal/lock`, `cmd/awgkeen`.
- Зависимости: `golang.org/x/crypto/ssh`, `gopkg.in/yaml.v3`, `github.com/spf13/cobra`.
- Внешние системы: роутер Keenetic (KeeneticOS ≥ 5.0, где есть `dns-proxy route object-group`).
