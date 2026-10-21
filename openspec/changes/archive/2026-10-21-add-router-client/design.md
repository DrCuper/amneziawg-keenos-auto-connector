# Design

## Context

Репозиторий пустой. Целевая платформа — KeeneticOS 5.x: `object-group fqdn`
появился в 4.3, `dns-proxy route object-group` — в 5.0. Документации RCI у
Keenetic нет; поведение известно из практики сообщества:

* RCI — JSON‑зеркало дерева CLI на `http://<router>/rci/...`. Авторизация:
  `GET /auth` → 401 с `X-NDM-Realm`, `X-NDM-Challenge`, cookie;
  `key = sha256(challenge + md5(login:realm:password))`, `POST /auth {login, password: key}`.
  Сессия — скользящие 300 с; на 401 нужно переавторизоваться.
* `POST /rci/` принимает `[{"parse": "<cli line>"}]` — исполняет команды CLI.
  Ошибки часто приходят с HTTP 200 внутри `status[].status == "error"`.
* Неверное поле в JSON‑записи — тихий no‑op, поэтому пишем **только CLI‑строками
  через parse** и всегда перечитываем состояние после записи.
* `GET /ci/running-config.txt` — текущая конфигурация как CLI‑текст.
* SSH: тот же CLI; каждую команду исполняем в отдельной exec‑сессии
  (`show running-config` для чтения).

Известный синтаксис (из running-config и release notes 5.0.x):

```
object-group fqdn <name>
    description <text>
    include <fqdn|cidr>
    exclude <fqdn|cidr>
dns-proxy
    route object-group <name> <iface|gateway [iface]> [auto] [reject]
```

Однострочные формы: `object-group fqdn <name> include <addr>`,
`no object-group fqdn <name> include <addr>`, `no object-group fqdn <name>`,
`dns-proxy route object-group <name> <iface> auto`,
`no dns-proxy route object-group <name> <iface>`, `system configuration save`.

## Goals / Non-Goals

**Goals:**
- Единый интерфейс `Transport` (исполнить CLI‑строки, вернуть running-config),
  реализуемый RCI, SSH и фейком.
- Чистые функции `Parse(runningConfig) → State` и `Diff(State, Desired) → Plan`,
  тестируемые без роутера.
- `Client` как оркестратор: lock → read → backup → plan → apply → verify → save.
- Расширяемость для следующих этапов: `Desired` описывает группы+маршруты,
  позже сюда же лягут интерфейсы Wireguard.

**Non-Goals:**
- JSON‑запись через RCI (`POST /rci/object-group/...`) — не используем.
- Восстановление конфигурации из бэкапа через клиент (ручная операция).
- Управление Wireguard‑интерфейсами (этап 4).

## Decisions

1. **Команды CLI как единственный язык записи.** Альтернатива — JSON RCI.
   Отвергнута: тихие no‑op при ошибке в поле, разные формы для SSH и RCI,
   невозможность одинаково тестировать. CLI‑строки одинаково идут в оба
   транспорта и видны в dry‑run.
2. **Состояние из running-config, а не из `GET /rci/object-group/fqdn`.**
   Один парсер на оба транспорта; running-config — то, что реально сохранится.
   Парсер устойчив к отступам и неизвестным строкам (их игнорирует).
3. **Diff на уровне записей, не «переписать группу».** Keenetic тратит время на
   каждую запись; полная перезапись больших групп (тысячи доменов) недопустима.
   Удаление группы целиком — только когда её нет в желаемом состоянии и
   `prune=true`.
4. **Верификация после применения.** Из‑за тихих no‑op перечитываем состояние и
   строим diff повторно; непустой diff = ошибка верификации (сохранение не делаем).
5. **Ретраи только транспортных ошибок.** Классификация: `TransportError`
   (сеть, таймаут, HTTP 5xx, 401 после переавторизации) — ретраим;
   `CommandError` (ответ роутера `error`) — нет. Backoff: base×2^n, cap, jitter.
   Повтор пакета команд безопасен, потому что команды идемпотентны
   (`include` уже существующего — no‑op).
6. **Блокировка: `sync.Mutex` + `flock` на файл.** Внутрипроцессная защищает
   cron и HTTP‑хэндлеры в одном бинарнике; файловая — два экземпляра/ручной
   запуск CLI параллельно с сервисом. Ожидание ограничено `lock_timeout`.
7. **Секреты.** Поля `password`/`key_file` в YAML допускаются, но env
   `AWGKEEN_ROUTER_PASSWORD`/`AWGKEEN_ROUTER_KEY_FILE` переопределяют их.
   В `String()`/логах конфиг печатается с замаскированными секретами.
8. **Библиотеки.** `gopkg.in/yaml.v3`, `golang.org/x/crypto/ssh`, `spf13/cobra`.
   HTTP — stdlib с `cookiejar`. Логи — `log/slog`.

## Risks / Trade-offs

- [Синтаксис `exclude`/`route` может отличаться между версиями 5.0.x] →
  команды собираются в одном пакете `cli`; `router show --raw` показывает
  running-config для сверки; верификация ловит расхождение.
- [SSH‑exec может не поддерживаться на некоторых прошивках, нужен интерактивный shell]
  → транспорт SSH изолирован, возможна замена на PTY‑сессию без изменения клиента.
- [Сохранение (`system configuration save`) асинхронное] → после save опрашиваем
  `show last-change` до `unsaved=false` (RCI) или просто ждём paузу (SSH); таймаут настраиваемый.
- [Лимит числа записей в группе] → лимит задаётся в конфиге (`max_entries`,
  по умолчанию 2000); план с превышением отклоняется до применения.

## Open Questions

- Точный лимит записей на группу в KeeneticOS 5.x (уточним на реальном
  роутере; на спеку не влияет — значение конфигурируемое).
