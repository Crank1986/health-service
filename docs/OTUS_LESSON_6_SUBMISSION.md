# ДЗ №6 - централизованное логирование

Go CRUD-сервис пишет JSON-логи в stdout. Fluent Bit читает логи контейнеров из namespace `hs`, разбирает JSON, добавляет метаданные Kubernetes и отправляет записи в Elasticsearch. В Kibana настроены поиск, фильтры и гистограмма по уровням логирования.

```text
health-service / миграционная Job (hs)
  → файлы логов на ноде
  → Fluent Bit DaemonSet (logging)
  → Elasticsearch StatefulSet + PVC (logging)
  → Kibana (logging)
```

## Окружение и файлы

Проверено в Minikube с Docker-драйвером на macOS; runtime внутри ноды - Docker. Команды выполняются из корня репозитория. Нужны Docker Desktop, Minikube, kubectl, Helm, а для Newman - Node.js/npm.

| Компонент | Версия / размещение |
| --- | --- |
| Приложение | `artemprotchenko/myapp:logging-v2`, две реплики в `hs` |
| PostgreSQL | существующий Helm release `users-db`; первая установка описана ниже |
| Миграционная Job | `postgres:18.6`, namespace `hs` |
| Elasticsearch / Kibana | `8.19.25`, namespace `logging` |
| Fluent Bit | `4.2.5`, DaemonSet в `logging` |
| Данные Elasticsearch | PVC `data-elasticsearch-0`, `2Gi`, StorageClass `standard` |

Манифесты EFK: [K8s/logging](../K8s/logging). Приложение: [Deployment](../K8s/dp.yaml), [ConfigMap](../K8s/app-configmap.yaml), [Service](../K8s/svc.yaml). Миграции: [SQL ConfigMap](../K8s/migrations-configmap.yaml), [основная Job](../K8s/migrations-job.yaml), [проверочная Job](../K8s/migrations-check-job.yaml).

Лимит памяти ноды - 4 GiB. Перед установкой EFK компоненты мониторинга из ДЗ №5 приостановлены, PVC сохранены. Одновременная работа обоих стеков и полная установка на чистый кластер не проверялись.

## 1. Подготовка namespace и приложения

```bash
minikube start --driver=docker
kubectl config current-context
kubectl apply -f K8s/ns.yaml
kubectl apply -f K8s/logging/namespace.yaml
kubectl get storageclass standard
```

`hs` содержит приложение, PostgreSQL и миграции; `logging` - EFK. Namespace создаются до остальных объектов. Не выполняйте рекурсивный apply всей папки `K8s`: там есть независимые ресурсы мониторинга и проверочная миграция.

### Новая установка на пустую БД

Создайте Secret `postgres-credentials` в `hs` по [разделу 1 README](../README.md#1-secret-для-postgresql). Он должен содержать `postgres-password`, `CUSTOM_USER=user_app` и `CUSTOM_PASSWORD`. Значения паролей хранятся вне репозитория. Затем установите PostgreSQL:

```bash
helm install users-db \
  oci://registry-1.docker.io/cloudpirates/postgres \
  --version 0.21.2 --namespace hs \
  --values helm/postgres/values.yaml --wait --timeout 5m
kubectl apply -f K8s/migrations-configmap.yaml
kubectl apply -f K8s/migrations-job.yaml
kubectl wait -n hs --for=condition=complete job/users-migration-001 --timeout=120s
kubectl logs -n hs job/users-migration-001
```

Ожидаются JSON-события `migration_started` и `migration_completed` и строка psql `CREATE TABLE`. При ошибке SQL Job пишет `migration_failed` уровня ERROR и возвращает исходный код ошибки. Не удаляйте завершённую Job до проверки сбора её логов.

### Уже существующая БД

Если release, Secret и таблица `public.users` уже существуют, пропустите их создание. Не пересоздавайте основную Job: SQL не идемпотентен. Изменение шаблона существующей Job через apply также может вызвать ошибку immutable field. Для проверки нового логирования миграции на существующем стенде используется отдельная схема, описанная в разделе 4.

### Развёртывание приложения - для обоих сценариев

```bash
kubectl apply -f K8s/app-configmap.yaml
kubectl apply -f K8s/svc.yaml
kubectl apply -f K8s/dp.yaml
kubectl rollout status deployment/health-service -n hs --timeout=120s
kubectl get pods -n hs -l env=health-service-env
```

Доступ через `arch.homework` требует nginx Ingress Controller, [K8s/ingress.yaml](../K8s/ingress.yaml), записи `127.0.0.1 arch.homework` на Mac и работающего `minikube tunnel`. Первая установка контроллера и настройка адреса: [раздел 5 README](../README.md#5-ingress-и-archhomework).

Для проверки API без ingress можно в отдельном терминале запустить:

```bash
kubectl port-forward -n hs service/health-app-service 8080:80
```

API будет доступен по адресу `http://localhost:8080`. Подставьте его вместо `http://arch.homework` в командах проверки. Порт Service - 80, порт контейнера - 8000.

## 2. Установка EFK

Для Elasticsearch на Linux-ноде задайте и проверьте `vm.max_map_count`. После перезапуска ноды значение может потребоваться восстановить:

```bash
minikube ssh -- "printf '1048576\n' | sudo tee /proc/sys/vm/max_map_count"
minikube ssh -- "cat /proc/sys/vm/max_map_count"
kubectl apply -f K8s/logging/elasticsearch-services.yaml
kubectl apply -f K8s/logging/elasticsearch-configmap.yaml
kubectl apply -f K8s/logging/elasticsearch-statefulset.yaml
kubectl rollout status statefulset/elasticsearch -n logging --timeout=300s
kubectl get pods,pvc -n logging
kubectl apply -f K8s/logging/kibana-deployment.yaml -f K8s/logging/kibana-service.yaml
kubectl rollout status deployment/kibana -n logging --timeout=300s
kubectl apply -f K8s/logging/fluent-bit-rbac.yaml
kubectl apply -f K8s/logging/fluent-bit-configmap.yaml
kubectl apply -f K8s/logging/fluent-bit-daemonset.yaml
kubectl rollout status daemonset/fluent-bit -n logging --timeout=300s
kubectl get pods -n logging
kubectl logs -n logging daemonset/fluent-bit --tail=100
```

Ожидаются три готовых Pod и PVC Elasticsearch в состоянии Bound. Настройки Elasticsearch: single-node, heap 512 MiB, лимит контейнера 1 GiB, отключены ML и аутентификация. Это локальный учебный стенд: Elasticsearch оставлен за ClusterIP, без публичного ingress.

Fluent Bit читает `/var/log/containers/*_hs_*.log`. На Docker-ноде это ссылки через `/var/log/pods` на `/var/lib/docker/containers`, поэтому оба каталога подключены read-only. Контейнер запущен с UID 0 для чтения защищённых файлов; Docker socket не подключается. Позиции чтения сохраняются в hostPath `/var/lib/fluent-bit/tail.db`. На другом runtime пути подключения нужно проверить отдельно.

`Read_from_Head On` включает чтение сохранившихся файлов с начала, в том числе логов завершённой Job. Удалённые файлы не восстанавливаются. Для Elasticsearch 8 задан `Suppress_Type_Name On`: эта версия больше не принимает устаревший `Type _doc`.

## 3. Проверка доставки и доступ к Kibana

В отдельном терминале:

```bash
kubectl port-forward -n logging service/elasticsearch 9200:9200
```

В другом терминале:

```bash
curl --noproxy '*' --max-time 10 \
  'http://localhost:9200/_cat/indices/fluent-bit-logs-*?v&h=health,index,docs.count,store.size&s=index'
minikube service kibana -n logging
```

Ожидаются ежедневные индексы с документами. Yellow при одной ноде означает отсутствие размещённых резервных копий шардов, а не потерю доступа к основным шардам. `minikube service` открывает интерфейс NodePort Kibana; если команда поддерживает туннель, терминал оставляют работающим. Альтернатива - `kubectl port-forward -n logging service/kibana 5601:5601`, затем `http://localhost:5601`.

В Kibana: **Stack Management → Data Views → Create data view**. Название `OTUS logs`, маска `fluent-bit-logs-*`, поле времени `@timestamp`. Для старых событий установите нужный абсолютный диапазон или Last 7 days, для свежих - Last 1 hour. События становятся видимыми с задержкой чтения, отправки, индексации и обновления интерфейса.

## 4. Тесты и запросы для анализа

### CRUD и валидация

```bash
NO_PROXY=arch.homework,localhost,127.0.0.1 \
no_proxy=arch.homework,localhost,127.0.0.1 \
npx --yes newman run K8s/postman/users-api.postman_collection.json \
  --env-var "base_url=http://arch.homework" \
  --timeout-request 10000 --timeout 60000 --reporters cli
curl --noproxy '*' -i --max-time 10 'http://arch.homework/users/abc'
```

Newman создаёт и удаляет тестового пользователя. Ожидаются 7 запросов и 18 успешных проверок. В прогоне 17 ноября пользователь получил ID 5331; при повторении подставьте новый ID из ответа API. GET после удаления возвращает 404. Запрос с ID `abc` возвращает 400 и создаёт WARN `validation_failed`.

### Проверка миграции на существующей БД

Если проверочная схема и Job ещё не существуют, подключитесь к PostgreSQL:

```bash
kubectl exec -it users-db-postgres-0 -n hs -- \
  psql -h users-db-postgres -U user_app -d users_db -W
```

В psql выполните `CREATE SCHEMA migration_check AUTHORIZATION user_app;`, затем `\q`. Пароль - существующий пароль роли приложения. Далее:

```bash
kubectl apply -f K8s/migrations-configmap.yaml
kubectl apply -f K8s/migrations-check-job.yaml
kubectl wait -n hs --for=condition=complete job/users-migration-check-001 --timeout=120s
kubectl logs -n hs job/users-migration-check-001
```

PGOPTIONS задаёт search_path=migration_check, поэтому рабочая таблица public.users не меняется. Если тест уже выполнен, используйте существующие логи: не пересоздавайте Job поверх существующей таблицы. При первой установке на пустую БД достаточно основной миграции из раздела 1; проверочная схема необязательна.

### Запуск и корректная остановка

Сначала убедитесь, что обе реплики готовы. Для получения свежих событий удалите только один выбранный Pod обычным delete (без force): Deployment создаст замену. Команда с POD_NAME ниже - шаблон, замените его именем из списка.

```bash
kubectl get pods -n hs -l env=health-service-env
kubectl delete pod POD_NAME -n hs
kubectl rollout status deployment/health-service -n hs --timeout=120s
kubectl get pods -n hs -l env=health-service-env
```

Ожидаются `application_stopping`, `http_server_stopped` старого экземпляра и `application_starting` нового. Проверка 17 ноября: остановка началась в 15:49:48.779 MSK, HTTP-сервер остановлен в 15:49:48.836, новый экземпляр начал запуск в 15:49:51.141; все события видны в Kibana. ERROR подключения получены ранее при старте ноды, специально останавливать рабочую БД для повторения не требуется.

### KQL в Discover

| Проверка | Запрос |
| --- | --- |
| Миграция | `service: "users-migration"` |
| WARN / ERROR | `service: "health-service" AND level: (WARN OR ERROR)` |
| CRUD одного пользователя | `service: "health-service" AND user_id: 5331` |
| HTTP-запрос | `service: "health-service" AND event: "http_request_received" AND path: "/users/abc"` |
| Запуск / остановка | `service: "health-service" AND event: (application_starting OR application_stopping OR http_server_stopped)` |
| Все логи приложения | `service: "health-service"` |

Для CRUD добавлены столбцы @timestamp, level, event, operation, user_id, kubernetes.pod_name и сортировка времени по возрастанию. Для HTTP - method, path, peer_ip; peer_ip отражает непосредственного отправителя (за ingress это прокси). Для миграции - service, event, migration, schema и Pod. Поля могут отсутствовать у событий другого типа: например, у ошибки подключения при запуске нет CRUD-поля operation.

Для гистограммы в Discover задайте запрос `service: "health-service"`, период Last 1 hour и Breakdown by level.keyword. На скриншоте видны INFO и WARN. Ошибки ERROR произошли раньше и показаны в просмотре за 7 дней. Запросы и настройки таблиц сохранены отдельными сессиями Discover.

## 5. Скриншоты и результат

Скриншоты проверок за 15-17 ноября 2026 находятся в [logging_screenshots](screenshots/logging_screenshots). При повторном запуске тестов даты и ID будут другими.

![Kibana OTUS - CRUD пользователя 5331](<screenshots/logging_screenshots/Kibana OTUS - CRUD пользователя 5331.png>)

![Kibana OTUS - HTTP-запросы](<screenshots/logging_screenshots/Kibana OTUS - HTTP-запросы.png>)

![Kibana OTUS - WARN и ERROR](<screenshots/logging_screenshots/Kibana OTUS - WARN и ERROR.png>)

![Kibana OTUS - запуск и остановка](<screenshots/logging_screenshots/Kibana OTUS - запуск и остановка.png>)

![Kibana OTUS - логи миграции](<screenshots/logging_screenshots/Kibana OTUS - логи миграции.png>)

![Kibana OTUS - логи по уровням](<screenshots/logging_screenshots/Kibana OTUS - логи по уровням.png>)

![Kibana настройки Data View](<screenshots/logging_screenshots/Kibana настройки Data View.png>)

Проверены INFO (HTTP, CRUD, запуск/остановка и миграция), WARN (валидация), ERROR (подключение к PostgreSQL), фильтрация, поиск по ID и метаданные Pod. Newman: 7 запросов, 18 проверок, 0 ошибок.

Ограничения: миграции пока без Flyway/Liquibase; SQL не идемпотентен. Буфер Fluent Bit в памяти: tail.db хранит позиции, но не гарантирует сохранность неотправленных сообщений при аварии. Skip_Long_Lines пропускает слишком длинные строки. Нет политики автоматического удаления старых индексов, PVC всего 2Gi. Команды и проверки рассчитаны на учебный одноузловой Minikube, не на production.
