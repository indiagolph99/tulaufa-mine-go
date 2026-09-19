# tulaufa-mine-go

Управляющий демон для `minecraft.service` на сервере tulaufa.ru. Отдаёт статус,
старт/стоп/рестарт и живой лог в `/api/mc/*`; фронтенд живёт в репозитории
[tulaufa](https://github.com/indiagolph99/tulaufa) на маршруте `/minecraft-admin`.

Слушает только `127.0.0.1:8787` — наружу его пускает nginx.

## Зачем отдельный сервис

Сайт статический (`adapter-static`), серверной части у него нет. Демон на Go
занимает ~15 МБ RSS, а на сервере свободно около 485 МБ — Node-рантайм туда уже
не помещается комфортно.

## Устройство

```
cmd/tulaufa-mine/     точка входа, конфиг, подкоманда hash
internal/auth/        PBKDF2-хеш пароля, сессии, ограничение попыток входа
internal/mc/          вызов mc-ctl, разбор статуса, раздача логов подписчикам
internal/httpapi/     маршруты и middleware
deploy/               mc-ctl, unit, сниппет nginx, setup.sh
testdata/mc-ctl-stub  заглушка, чтобы всё работало без systemd
```

Зависимостей нет — только стандартная библиотека (`crypto/pbkdf2` появился в Go 1.24).

## Безопасность

- Демон не умеет ничего, кроме как запустить `/usr/local/bin/mc-ctl` через sudo.
  Скрипт принадлежит root, принимает ровно `status|start|stop|restart|logs-follow`
  и подставляет имя юнита сам — аргумент вызывающего никогда не доходит до
  `systemctl` или `journalctl`.
- Пароль хранится как PBKDF2-SHA256 (600 000 итераций) в `/etc/tulaufa-mine/env`.
- Сессия — случайные 32 байта в памяти; cookie `HttpOnly`, `Secure`, `SameSite=Strict`.
- Вход ограничен: 5 попыток в минуту с адреса.
- Запросы, меняющие состояние, требуют совпадения заголовка `Origin`.
- Действия дебаунсятся (10 с), чтобы двойной клик не дёргал JVM.
- Все входы и действия пишутся в journal вместе с IP.

## Разработка

Systemd не нужен — есть заглушка:

```sh
export MC_CTL="$PWD/testdata/mc-ctl-stub"
export ADMIN_PASSWORD_HASH="$(go run ./cmd/tulaufa-mine hash)"   # спросит пароль
export ALLOWED_ORIGIN=http://localhost:5173
export SECURE_COOKIE=false
go run ./cmd/tulaufa-mine
```

Тесты: `go test ./...`. Проверка: `go vet ./...`.

## Конфигурация

| Переменная | По умолчанию | Назначение |
|---|---|---|
| `ADMIN_PASSWORD_HASH` | — (обязательна) | результат `tulaufa-mine hash` |
| `ALLOWED_ORIGIN` | `https://tulaufa.ru` | допустимый `Origin` |
| `LISTEN_ADDR` | `127.0.0.1:8787` | адрес прослушивания |
| `SESSION_TTL` | `12h` | время жизни сессии |
| `SECURE_COOKIE` | `true` | выключать только для localhost без TLS |
| `MC_CTL` | `sudo -n /usr/local/bin/mc-ctl` | команда-обёртка |
| `ACTION_DEBOUNCE` | `10s` | минимальный интервал между действиями |
| `MAX_LOG_STREAMS` | `4` | одновременных SSE-потоков |

## Установка на сервер

Одной командой — она отправляет на сервер всю папку `deploy/` и запускает оба
скрипта по порядку:

```sh
./deploy/install-remote.sh root@<host> -p <port>
```

Отправлять нужно именно папку: `setup.sh` ставит `mc-ctl` и unit-файл, которые
лежат рядом с ним. Если скормить ему только сам скрипт через `bash -s`, файлов
на сервере не окажется — скрипт это заметит и объяснит, что делать.

Оба скрипта идемпотентны, запускать повторно безопасно. По отдельности:
`--base-only` или `--nginx-only`.

**1. Пользователь, обёртка, sudoers, unit** (`setup.sh`):

Создаёт пользователя `tulaufa-mine`, ставит `mc-ctl`, проверяет sudoers через
`visudo -c`, ставит и включает unit, а затем сам проверяет, что обёртка
работает и что мусорный аргумент отвергается.

**2. nginx** (`setup-nginx.sh`):

В исходном виде у vhost'а tulaufa.ru **нет ни одного блока `location`**, поэтому
руками его править не надо — оба блока создаёт скрипт:

- `location /` с `try_files $uri $uri.html ...` — без него `/minecraft-admin`
  отдаёт 404, потому что adapter-static кладёт файл как `minecraft-admin.html`;
- `location /api/mc/` — проксирование на демон с отключённой буферизацией для SSE.

Скрипт делает резервную копию, проверяет конфиг через `nginx -t` и откатывается,
если проверка не прошла. Если путь к сертификату отличается от ожидаемого, он
отказывается перезаписывать конфиг.

**3. Пароль.**

Хеш генерируется на вашей машине — открытый пароль не должен попадать на сервер
ни в каком виде:

```sh
go run ./cmd/tulaufa-mine hash
```

Команда спросит пароль (ввод не отображается) и переспросит для подтверждения,
затем напечатает строку вида `pbkdf2-sha256$600000$…`. Пробелы разрешены —
парольная фраза считывается целиком.

Дальше эту строку нужно положить в `/etc/tulaufa-mine/env`. **Не вставляйте её в
двойных кавычках и не подставляйте в командную строку без одинарных** — в хеше
есть `$`, и оболочка съест часть строки. Надёжнее передать файл по ssh, минуя
разбор командной строки:

```sh
{ printf 'ADMIN_PASSWORD_HASH='; go run ./cmd/tulaufa-mine hash; \
  printf 'ALLOWED_ORIGIN=https://tulaufa.ru\n'; } \
| ssh -p <port> root@<host> \
    'cat > /etc/tulaufa-mine/env
     chown root:tulaufa-mine /etc/tulaufa-mine/env
     chmod 0640 /etc/tulaufa-mine/env
     systemctl restart tulaufa-mine.service || true'
```

Права `0640 root:tulaufa-mine` обязательны: сервис читает файл, а больше никто.
`systemd` не раскрывает переменные в `EnvironmentFile`, поэтому `$` в хеше
безопасен — но только если строка дошла до файла целиком.

Проверить, что демон принял хеш:

```sh
ssh -p <port> root@<host> 'systemctl is-active tulaufa-mine.service'
```

При неверном или повреждённом хеше сервис не стартует и пишет в journal
`ADMIN_PASSWORD_HASH is unusable` — это сделано намеренно, чтобы поломка
обнаружилась сразу, а не при первой попытке входа.

## Выкатка

Вручную: **Actions → deploy → Run workflow** с ветки `main`, затем подтвердить
гейт окружения `production`. Workflow собирает статический бинарник, копирует
его в `/opt/tulaufa-mine/`, перезапускает сервис, проверяет здоровье и ставит
тег `v<дата>`.

## Если забыт пароль

Сгенерировать новый хеш локально (`tulaufa-mine hash`), заменить строку в
`/etc/tulaufa-mine/env`, затем `systemctl restart tulaufa-mine.service`.
