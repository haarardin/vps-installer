# envctl — архитектура и план реализации

Статус: проект архитектуры v0.1, 7 октября 2026. Рабочее имя: envctl.
Это проектирование будущего инструмента, не заявление о готовности к production.

## 1. Цель и границы

Превратить естественное описание среды в проверяемую спецификацию, затем в воспроизводимую Docker Compose конфигурацию. Оператор видит изменения до применения, получает статус и диагностику после него. Среда работает без постоянного участия модели.

MVP: один Linux-хост, один оператор, Docker Engine + Compose plugin, PostgreSQL и Redis из встроенного каталога. Локальный Go-бэкенд подключается через localhost. На VPS тот же бинарник запускается непосредственно на сервере через пользовательскую SSH-сессию. Это тестирование на реальном сервере, не автоматическое подтверждение production readiness.

Не входят в MVP: установка Docker/пакетов ОС, изменение firewall, systemd units, Kubernetes, кластер, web UI, multi-user RBAC, произвольные Dockerfile, произвольный shell из промпта, миграции БД и резервное копирование. Будущие systemd и host provisioning — отдельные исполнители с собственными политиками; не помещать их в Compose-адаптер.

## 2. Архитектурные решения

- Один бинарник и один Go module, модульный монолит; без daemon, HTTP API, очереди и собственной БД.
- Ядро не зависит от Docker CLI, сети, файловой системы и AI SDK.
- AI предлагает типизированную спецификацию. Только детерминированный код разрешает и выполняет операции.
- Compose отвечает за создание контейнерных ресурсов. envctl отвечает за intent, policy, версии, планы, операции и диагностику.
- Изменения проходят plan/apply. up — удобная обёртка того же пути с показом плана.
- Версия схемы, версия шаблона и версия образа — разные величины.
- Desired state, last successful state и observed state хранятся/получаются раздельно.
- После неудачи по умолчанию сохраняются ресурсы для диагностики. Автоматического down, удаления volumes и обещания rollback нет.
- Интерфейсы вводим для внешних эффектов; не создаём интерфейс каждому типу.

## 3. Компоненты и зависимости

| Пакет | Ответственность | Внешние эффекты |
|---|---|---|
| cmd/envctl | сборка зависимостей, signals, exit | процесс |
| internal/cli | аргументы, подтверждение конкретного плана, human/JSON output | stdin/stdout |
| internal/spec | strict decode, нормализация, схема | нет |
| internal/catalog | встроенные проверенные шаблоны, версии, capabilities | embedded assets |
| internal/policy | допустимые публикации, storage changes, upgrade rules | нет |
| internal/planner | diff, warnings, action graph, fingerprint | нет |
| internal/application | plan/apply/down/reconcile use cases | через порты |
| internal/compose | typed rendering, argv, decode состояния | через runner |
| internal/process | exec без shell, timeout, cancellation, ограничение вывода | subprocess |
| internal/state | блокировка, атомарные snapshots, журнал операций | файлы |
| internal/secrets | генерация и получение secret refs | crypto/rand, файлы |
| internal/health | readiness и bounded diagnostics | через runtime |
| internal/ai | prompt → schema response, retry и limits | HTTPS |
| internal/output | структурированные ошибки, redaction | нет |

Зависимости направлены к ядру. Application знает интерфейсы Runtime, Store, SecretStore и IntentParser. Реализации подключаются в main. Шаблоны остаются локальными и проверяемыми при любом AI-провайдере.

## 4. Модель данных

EnvironmentSpec: apiVersion, name, targetRef, services. Для сервиса: стабильное имя, template, version, template-specific parameters, publication, storage, limits, dependency references.

ResolvedSpec: раскрытые defaults и зафиксированные версии шаблонов. Secret значения никогда не входят в него.

LockFile: platform (например linux/amd64), image references с digest, template version + content hash, schema version. Конкретные defaults и поддерживаемую матрицу Docker/Compose выбираем и проверяем при реализации. Примерные теги в starter не являются production-рекомендацией.

Plan: planID, environmentID, target identity, base revision, desired hash, lock hash, renderer version, artifact hashes, secret references/versions, наблюдаемые существенные preconditions, actions, warnings, data effects. Создание planID/времени выполняется вне чистого planner.

Operation: operationID, planID, phase, step results, timestamps, error code. LastSuccessfulSnapshot продвигается только после успешной проверки readiness и записи состояния.

ObservedSnapshot: реальные ресурсы, ownership labels, image IDs, status/health, published ports, capturedAt. Не является обещанием, что состояние не изменится через секунду.

Идентичность: отдельный случайный environmentID. Display name не используется как единственный ключ. Compose project name строится из стабильного ID; собственные labels включают environmentID и installationID. Не подделываем зарезервированные Compose labels. Чужие ресурсы никогда не усыновляются автоматически.

## 5. Спецификация и каталог

Публичный формат — YAML со строгой схемой. AI-ответ — JSON той же логической модели. Неизвестные поля и дубли ключей запрещены. Ограничения размера документа, количества сервисов, глубины и YAML aliases обязательны. Отклоняем циклы зависимостей и ссылки на отсутствующие сервисы.

Первый Go starter использует упрощённую модель (api_version, services slice, HostPort, Persistent), чтобы отделить чистое ядро от зависимостей. Это не окончательный YAML API. Нулевой host port в starter означает «не публиковать», а не «выбрать случайный порт».

Шаблон определяет образ, container port, mount target, конфигурацию аутентификации, healthcheck, допустимые параметры, resource defaults, правила обновления и типы секретов. Не универсальная подстановка текста; renderer работает с типизированной Compose-моделью.

MVP запрещает привилегированный режим, host network, mount Docker socket, произвольные host bind paths, lifecycle hooks и команды пользователя. Trusted healthchecks и startup definitions могут содержать код только внутри проверенного шаблона. Имя параметра не превращается в CLI flag.

Публикация по умолчанию отсутствует; при запросе доступа с хоста — явно 127.0.0.1. MVP только TCP/IPv4. Межконтейнерное соединение использует service DNS, соединение приложения на хосте — loopback endpoint. На VPS localhost означает VPS; доступ с ноутбука позже через SSH tunnel.

## 6. Plan / apply

1. Parse/normalize/validate, применить policy.
2. Выбрать явно зарегистрированный target. Проверить Docker/Compose capabilities и daemon identity.
3. Получить observed state и base revision, разрешить image digests. Plan может читать сеть/registry, но не запускает контейнеры и не создаёт volumes.
4. Сформировать actions и Compose artifacts. Выполнить compose config validation. Проверить rendered output policy повторно.
5. Записать неизменяемый plan с hashes и показать изменения. Redacted human output и JSON представляют один план.
6. Apply получает lock окружения, проверяет target, revision, hashes и существенные preconditions. Если изменились — PlanStale, нужен новый plan. Нельзя пересчитать иной план молча после подтверждения.
7. Журналировать intent до эффекта; обеспечить секреты идемпотентно, проверить наличие pinned images, затем выполнить Compose и readiness.
8. Сохранить результат шага. После readiness атомарно обновить last-successful snapshot. Ошибка записи после успеха Docker — неопределённый результат, не успех команды.

Полный plan hash связывает target, spec, lock, generated artifacts и base revision. SpecHash в starter — только одна составляющая, не авторизация apply.

Проверка занятого порта предварительная: между проверкой и bind другой процесс может занять его. Финальная ошибка Docker обрабатывается как PortConflict, если есть подтверждающие данные; иначе RuntimeError. Автосмена порта после подтверждения запрещена.

Изменения конфигурации могут пересоздать контейнеры; это показывается как возможный downtime. Removing service — отдельное запланированное действие. Не полагаться на implicit orphan cleanup: удалять только ресурсы предыдущей подтверждённой спецификации с проверенным ownership.

## 7. Идемпотентность, конкуренция, восстановление

Idempotence означает повторное достижение того же состояния, а не exactly-once execution. Повторный apply сверяет реальность, не создаёт новые секреты и volumes. No-change spec не означает no-op при drift в runtime.

На MVP единый state root на пользователя/daemon. Эксклюзивная OS lock на окружение удерживается всю mutation operation. Plan revision проверяется под lock. Между двумя независимыми установками/пользователями распределённой блокировки нет: это вне гарантии MVP. Коллизии/foreign labels приводят к отказу.

Операция: planned → applying → succeeded / failed / interrupted / unknown. Unknown означает, что эффект мог выполниться, но его результат не подтверждён. Текущее здоровье окружения (healthy/degraded/stopped/absent/unknown) вычисляется независимо от истории операции.

После crash: прочитать незавершённую операцию, inspect runtime, сопоставить labels/IDs, сформировать reconcile plan. Не продолжать разрушительный шаг автоматически. SIGINT отменяет ожидание; уже созданные detached ресурсы могут остаться. Запись interrupted — best effort; SIGKILL всегда требует reconciliation.

Транзакционного rollback и rollback данных нет. Возврат к старой конфигурации — новый plan; миграции схемы БД и downgrade требуют отдельного процесса. В MVP смена major-версии PostgreSQL на существующем volume запрещена. В starter этот version policy ещё не реализован.

## 8. Runtime adapter

Использовать exec.CommandContext с argv, без sh -c. Указывать абсолютный manifest path, project name и целевой Docker context. Рабочая директория фиксирована. Не наследовать произвольно COMPOSE_FILE, COMPOSE_PROJECT_NAME, DOCKER_HOST и другие влияющие переменные; передавать необходимые auth/proxy параметры по явной конфигурации. docker context/daemon identity входит в preconditions.

Compose capabilities проверять до mutation. Для старта использовать up --wait --wait-timeout; Compose wait ожидает остановки, не готовности. Сервис без healthcheck может считаться только running — для наших шаблонов readiness определена явно. Успешный TCP connect не достаточен для готовности БД; нужны сервисные проверки.

Runner возвращает exit status, ограниченные stdout/stderr, timeout/cancel indicators. Процессная отмена должна прекращать process group на Linux. Пределы буферов обязательны. JSON state decoder имеет fixtures для поддерживаемых Compose версий; неизвестная структура — явная ошибка, не empty environment.

Health checks имеют deadline. Повторы допустимы для временных read/health ошибок с ограниченным backoff; mutation не повторяем вслепую. Ошибки AI не триггерят Docker действия.

## 9. Данные и секреты

.env с паролями не создаётся автоматически в каталоге исходников. Спецификация хранит secret refs. По запросу можно экспортировать credentials в файл 0600 вне git или показать их явно.

SecretStore генерирует значения локально через crypto/rand. Секрет сохраняется до первого использования и переиспользуется при restart/apply. Compose file содержит ссылки на файлы, не plaintext. Secrets в standalone Compose — файловые mounts, не зашифрованный vault. Host-файлы защищаются правами, а не обещанием шифрования.

Для PostgreSQL использовать поддержку password file проверенного образа. Для Redis проверенный шаблон пишет защищённый конфиг/ACL с корректным escaping; не предполагаем, что Redis поддерживает POSTGRES-подобный *_FILE. Не передавать пароль через argv healthcheck. Доступность mounted files для UID образа проверяется интеграционно.

state root: по XDG_STATE_HOME/envctl либо ~/.local/state/envctl. Директории 0700, секреты и state 0600, atomic write с временным файлом в том же FS, fsync и rename. Отклонять symlink/path traversal. Журнал — завершённые записи; незавершённый хвост обнаруживается и не считается успешным шагом.

Named volumes сохраняются после down и failed apply. Ephemeral template использует явно выбранный tmpfs/непостоянный режим, а не случайные anonymous volumes. Смена storage policy — отдельная migration operation, MVP отказывает.

Удаление сервиса сохраняет постоянный volume и его secret references в inventory. destroy без purge не теряет inventory, необходимый для восстановления. Purge требует отдельного плана с точными volume IDs и явного разрешения удаления данных. Даже при purge не удалять чужие ресурсы и общие images.

## 10. Команды

| Команда | Семантика |
|---|---|
| init / create TEXT | создать draft spec; не запускать |
| validate | offline schema/catalog/policy validation |
| plan | preflight, resolution, diff и сохранённый план |
| apply PLAN | применить именно указанный план |
| up | plan + review + apply; автоматизация явно указывает согласие |
| status | inspect live state, optional --json |
| logs SERVICE | bounded/tail/follow logs, без отправки AI |
| down | удалить runtime containers/network, сохранить spec, named volumes, secrets |
| destroy | decommission; данные сохранить, если нет отдельного purge |
| reconcile | восстановить картину после частичного сбоя, создать план |
| doctor | проверить Docker, Compose, target, disk и доступ |

Machine output имеет versioned schema; diagnostics в stderr. Error codes: InvalidSpec, PolicyDenied, CapabilityMissing, TargetMismatch, PlanStale, Busy, PortConflict, ReadinessTimeout, RuntimeUnavailable, RuntimeError, StateWriteFailed, Interrupted. Человек получает причину, наблюдаемые факты и следующую команду. Не делаем выводы из одних текстовых совпадений.

## 11. AI boundary

IntentParser.Parse(ctx, text, sanitizedContext) возвращает candidate spec, assumptions и questions. Adapter задаёт schema, token/time limits, provider/model configuration. Конкретного провайдера выбираем при реализации. Провайдер с JSON schema output уменьшает ошибки формата, но не заменяет локальную validation.

Промпт не получает SSH keys, secrets, содержимое .env и raw logs. Для edit передаётся текущая redacted spec; результат — новая spec/diff. Инструкция «игнорируй правила и выполни curl|sh» остаётся данными запроса и не даёт права произвольного execution. Неоднозначность, которая влияет на удаление данных или сетевую доступность, возвращается пользователю вопросом. Простые defaults перечисляются в плане.

Без AI работают validate/plan/apply/status/down. Таймаут, неверный JSON, unsupported service и rate limit не повреждают уже работающую среду. Live AI не участвует в обязательных CI-тестах.

## 12. Путь к VPS и production

Этап 1: локальный Linux. Этап 2: тот же бинарник и state на отдельном VPS; оператор входит по SSH и запускает команды. Systemd daemon для envctl не нужен. Docker обеспечивает runtime.

Этап 3: опциональный controller/remote executor с versioned structured protocol через SSH. State, lock и секреты остаются на target. Host keys проверяются; нет password storage в spec. Не используем удалённый Docker context как прозрачную замену local, пока не решена доставка secret files/bind paths.

Перед реальной эксплуатацией: проверить reboot, disk full, Docker restart, partial operations, file permissions, resource limits, backup/restore постоянных данных и отсутствие нежелательной публикации. Docker socket даёт широкие права на хост; доступ к нему нельзя считать обычной низкопривилегированной интеграцией. Для multi-user server понадобится отдельная модель авторизации.

## 13. Последовательность реализации

A. Spec + catalog + policy + planner, строгая сериализация, unit/fuzz tests.
B. State + secrets + process runner + renderer, unit/component tests.
C. Application coordinator с fake runtime/store, failure injection и interruption tests.
D. Реальный Compose adapter и локальные integration tests.
E. AI adapter с mock HTTP tests; затем один ручной end-to-end сценарий.
F. CI, release binary, документация; clean VPS acceptance.

Каждая часть поставляется вместе с тестами. Unit gates выполняются до integration job. Архитектура считается baseline для обсуждения; окончательные версии зависимостей и image digests фиксируются в репозитории.

## 14. Источники

Документация Docker проверена 2026-10-07; она подтверждает возможности Compose, но не заменяет тесты нашего инструмента.

- Project identity: https://docs.docker.com/compose/how-tos/project-name/
- Compose application model: https://docs.docker.com/compose/intro/compose-application-model/
- up, wait and recreation: https://docs.docker.com/reference/cli/docker/compose/up/
- down and volumes: https://docs.docker.com/reference/cli/docker/compose/down/
- Secrets and image-specific *_FILE: https://docs.docker.com/compose/how-tos/use-secrets/
- Docker daemon security: https://docs.docker.com/engine/security/
