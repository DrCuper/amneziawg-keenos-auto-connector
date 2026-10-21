# Proposal

## Why

Issue #3: при импорте существующих групп Keenetic в `domains.yaml` нормализация
отбрасывает односоставные имена без точки (`include youtube`), хотя роутер их
принимает (`router.ValidateEntry` их пропускает). В результате
`sync --dry-run` планирует `no object-group fqdn domain-list0 include youtube` —
удаление существующего правила, которое оператор явно перечислил в `include`.

## What Changes

- Нормализация различает происхождение записей:
  - **источники** (URL‑списки, файлы) — как раньше: имена без точки
    (TLD, имена из hosts‑файлов) отбрасываются;
  - **записи оператора** (`include`, `remove`, `exclude` в `domains.yaml`,
    `awgkeen groups add|remove`) — односоставные имена сохраняются, кроме
    зарезервированных локальных/служебных (`localhost`, `local`,
    `localdomain`, `broadcasthost`, `ip6-*`, `*`).
- Регрессионные тесты: сборка группы с `include: [youtube]` и dry‑run
  синхронизации против роутера с `include youtube` без команд удаления.

## Impact

- Спека `domain-groups`: изменено требование «Нормализация».
- Код: `internal/domains` (`NormalizeManual`, `Build`), `internal/syncer`
  (ручные правки через CLI).
