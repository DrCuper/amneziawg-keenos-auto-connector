# amneziawg-keenos-auto-connector

Сервис на Go для управления доменной маршрутизацией (`object-group fqdn` +
`dns-proxy route`) и AmneziaWG‑подключениями на роутерах Keenetic (KeeneticOS 5.x).

Проект ведётся по [OpenSpec](https://github.com/Fission-AI/OpenSpec): описание
сервиса и план этапов — в [`openspec/project.md`](openspec/project.md),
спецификации — в `openspec/specs/`, предложения — в `openspec/changes/`.

**Статус:** этап 1 — клиент роутера и dry‑run (`openspec/changes/add-router-client`).

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

## Команды

```sh
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

## Как это работает

Оба транспорта исполняют **одни и те же CLI‑строки** KeeneticOS
(RCI — через `POST /rci/ [{"parse": "..."}]`, SSH — через exec), а состояние
читается из running‑config. JSON‑запись в ветки RCI не используется намеренно:
неверное поле там — тихий no‑op. После применения состояние перечитывается;
если расхождение осталось, возвращается ошибка верификации и конфиг не сохраняется.

```
cmd/awgkeen/          CLI
internal/app/         сборка конфиг → транспорт → клиент
internal/config/      YAML + env
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

См. таблицу этапов в [`openspec/project.md`](openspec/project.md): группы доменов
из источников + cron → журнал и уведомления → импорт AWG‑конфига → веб‑интерфейс →
управление сервером и пирами.
