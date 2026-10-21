# amneziawg-keenos-auto-connector

Сервис на Go для управления доменной маршрутизацией (`object-group fqdn` +
`dns-proxy route`) и AmneziaWG‑подключениями на роутерах Keenetic (KeeneticOS 5.x).

Проект ведётся по [OpenSpec](https://github.com/Fission-AI/OpenSpec): описание
сервиса и план этапов — в [`openspec/project.md`](openspec/project.md),
спецификации — в `openspec/specs/`, предложения — в `openspec/changes/`.

**Статус:** этап 2 — группы доменов, дифф‑синхронизация и cron
(`openspec/changes/add-domain-groups-sync`); этап 1 (клиент роутера) заархивирован в `openspec/specs/router-client`.

## Что умеет сейчас

* Транспорты **RCI** (HTTP‑API KeeneticOS, challenge‑response авторизация) и **SSH**.
* Чтение running‑config и разбор групп `object-group fqdn` и маршрутов `dns-proxy route`.
* План изменений как **дифф** «роутер → желаемое состояние»: только недостающие /
  лишние `include`/`exclude`/маршруты, без полной перезаписи групп.
* **Dry‑run**: `router plan` и `router apply --dry-run` ничего не отправляют.
* Применение с проверкой (состояние перечитывается после записи) и
  `system configuration save`.
* Бэкап running‑config перед каждым изменением, таймауты, ретраи транспортных
  ошибок, блокировка от параллельных запусков (в процессе + flock).
* **Группы доменов** из источников: URL‑списки community‑репозиториев и локальные
  файлы в форматах plain / hosts / dnsmasq / v2fly / Clash (автоопределение
  построчно), ручные `include` / `remove` / `exclude`, кэш загрузок с условным
  GET и откатом к последней удачной копии.
* **Подключения**: группа ссылается на `connection` (AWG, Reality …), маршруты
  `dns-proxy route` описаны один раз на подключение.
* **Нормализация**: регистр, IDN → punycode, проверка формата, дедупликация,
  схлопывание поддоменов, лимит записей (`error` или `truncate`).
* **Версии и откат**: каждая изменившаяся сборка — новая версия в `state_dir`;
  `groups rollback` закрепляет группу на прошлой версии и применяет дифф.
* **Защиты**: упавший источник не опустошает группу; резкое сокращение
  (`max_shrink`) блокируется без `--force`; dry‑run не пишет ни на роутер, ни в состояние.
* **Расписание**: `awgkeen serve` синхронизирует по cron, `awgkeen sync` — вручную.

## Установка

```sh
go install github.com/DrCuper/amneziawg-keenos-auto-connector/cmd/awgkeen@latest
# или
make build && ./bin/awgkeen --help
```

## Конфигурация

Скопируйте [`config.example.yaml`](config.example.yaml) в `config.yaml`.
Секреты можно не писать в файл:

```sh
export AWGKEEN_ROUTER_PASSWORD='...'      # пароль admin (rci и ssh)
export AWGKEEN_ROUTER_KEY_FILE=~/.ssh/kn  # ключ для ssh
```

Желаемое состояние групп — отдельный YAML, см. [`examples/groups.yaml`](examples/groups.yaml):

```yaml
groups:
  - name: awg-main
    include: [example.com, api.example.com]
    exclude: [static.example.com]
    routes:
      - interface: Wireguard0
```

Описание групп, подключений и источников — `domains.yaml`,
см. [`examples/domains.yaml`](examples/domains.yaml):

```yaml
connections:
  awg:
    routes: [{interface: Wireguard0}]
groups:
  - name: awg-main
    connection: awg
    sources:
      - url: https://raw.githubusercontent.com/.../domains.lst
      - file: lists/extra.txt
    include: [example.com]
    remove:  [tracker.example.com]
```

## Команды

```sh
awgkeen sync                              # собрать группы из источников и применить дифф
awgkeen sync --dry-run                    # показать сводку и план, ничего не менять
awgkeen sync --group awg-main --force     # одна группа, без защиты от сокращения
awgkeen serve                             # демон: синхронизация по sync.schedule

awgkeen groups list                       # версии, применённые версии, закрепления
awgkeen groups show awg-main [--version 3]
awgkeen groups history awg-main
awgkeen groups add awg-main site.example  # ручная запись без правки YAML (--apply — сразу на роутер)
awgkeen groups remove awg-main bad.example
awgkeen groups clear awg-main
awgkeen groups rollback awg-main [--to 3] # закрепить на прошлой версии и применить
awgkeen groups unpin awg-main             # снова собирать из источников

awgkeen router show                       # группы и маршруты на роутере
awgkeen router show --raw                 # весь running-config
awgkeen router plan  -d groups.yaml       # что изменится (роутер не трогается)
awgkeen router apply -d groups.yaml --dry-run
awgkeen router apply -d groups.yaml       # применить, проверить, сохранить
awgkeen router apply -d groups.yaml --no-save
awgkeen router save                       # system configuration save
```

Пример вывода `plan`:

```
group awg-main:
  + include api.example.com
  - include old.example.com
  + route Wireguard0 auto

3 command(s):
  object-group fqdn awg-main include api.example.com
  no object-group fqdn awg-main include old.example.com
  dns-proxy route object-group awg-main Wireguard0 auto
```

Пример вывода `sync`:

```
group awg-main -> awg: version 7 (new), 1843 entries, 2 exclude, +12/-3 vs applied v6, collapsed 41
  source community: 1790 valid of 1802 lines, 12 skipped
  source local: 55 valid of 55 lines
group reality -> reality: version 2, 120 entries, +0/-0 vs applied v2

group awg-main:
  + include new.example.com
  ...
15 command(s):
  ...
applied 15 command(s), saved=true, backup=/var/lib/awgkeen/backups/running-config-20261021-120000.txt
```

## Как это работает

Оба транспорта исполняют **одни и те же CLI‑строки** KeeneticOS
(RCI — через `POST /rci/ [{"parse": "..."}]`, SSH — через exec), а состояние
читается из running‑config. JSON‑запись в ветки RCI не используется намеренно:
неверное поле там — тихий no‑op. После применения состояние перечитывается;
если расхождение осталось, возвращается ошибка верификации и конфиг не сохраняется.

Синхронизация (`sync`/`serve`): загрузить `domains.yaml` → для каждой группы
скачать источники (или взять закреплённую версию) → разобрать, нормализовать,
применить `remove`/лимит → сравнить хэш с последней версией и при изменении
сохранить новую → собрать `Desired` из всех групп и маршрутов их подключений →
`Client.Apply` (lock, бэкап, дифф, проверка, save) → запомнить применённые версии.

```
cmd/awgkeen/          CLI
internal/app/         сборка конфиг → транспорт → клиент → store → syncer
internal/config/      YAML + env
internal/domains/     domains.yaml, нормализация, сборка группы
internal/source/      загрузка списков (HTTP с кэшем, файлы) и парсер форматов
internal/store/       файловое состояние: версии, applied/pinned, ручные правки, кэш
internal/syncer/      оркестратор синхронизации, откат, планировщик cron
internal/router/      модель, план (дифф), Client: lock → read → backup → plan → apply → verify → save
internal/router/cli/  парсер running-config и генерация команд
internal/router/rci/  транспорт RCI
internal/router/sshx/ транспорт SSH
internal/router/fake/ фейковый роутер для тестов
internal/lock/        flock
```

## Разработка

```sh
make test   # go test -race ./...
make lint   # golangci-lint
openspec validate --strict
```

Тесты не требуют сети и реального роутера.

## Дорожная карта

См. таблицу этапов в [`openspec/project.md`](openspec/project.md): журнал и
уведомления → импорт AWG‑конфига → веб‑интерфейс → управление сервером и пирами.
