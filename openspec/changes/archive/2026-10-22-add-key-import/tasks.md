# Tasks

- [x] 1.1 `awg.KeyStore` (проверка ключа, 0600, атомарная запись), `Tunnel.Imported`, `Manager.List/Put/Drop`
- [x] 1.2 `app.ImportTunnel/ForgetTunnel` с записью в журнал; импортированные ключи подхватываются при запуске
- [x] 1.3 CLI `awg import`, `awg remove` для импортированных, `awg list` показывает источник
- [x] 1.4 API `POST /tunnels/import`, `DELETE /tunnels/{name}`; форма во вкладке «Туннели»
- [x] 1.5 `health.Monitor.ChecksFunc` — проверки новых туннелей без перезапуска
- [x] 1.6 Тесты: два ключа от разных серверов → два интерфейса и маршруты, замена, удаление, конфликт имён, API; проверка UI; README
