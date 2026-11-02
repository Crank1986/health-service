# Health Service — OTUS Microservices

HTTP-сервис на Go: проверка здоровья и CRUD пользователей с хранением в PostgreSQL. Приложение разворачивается в Kubernetes, PostgreSQL устанавливается через Helm, таблица создаётся отдельным Job.

## Как устроено приложение

```text
Клиент → arch.homework:80 → minikube tunnel → nginx Ingress Controller
       → health-app-service:80 → Pod приложения:8000
       → users-db-postgres:5432 → PostgreSQL → PVC
```

Ingress задаёт правила HTTP-маршрутизации. Service направляет запросы к Pod приложения, Deployment поддерживает две реплики. Отдельный Job запускает `psql` для первоначального создания таблицы.

Конфигурация подключения находится в ConfigMap, учётные данные — в Secret. Kubernetes передаёт их в окружение контейнера; Go читает переменные `DB_HOST`, `DB_PORT`, `DB_NAME`, `DB_USER`, `DB_PASSWORD`.

## API

Основной адрес: `http://arch.homework`.

| Метод | Путь | Результат |
| --- | --- | --- |
| GET | `/health/` | `200`, `{"status":"OK"}` |
| POST | `/users` | Создать пользователя, `201` и JSON с ID |
| GET | `/users/{id}` | Получить пользователя, `200` |
| PATCH | `/users/{id}` | Изменить переданные поля, `200` |
| DELETE | `/users/{id}` | Удалить пользователя, `204` без тела |

Поля пользователя: `id`, `name`, `email`. ID назначает БД через `SERIAL`; имя и email обязательны, email уникален. Пробелы по краям имени и email удаляются.

PATCH сохраняет поля, которые не переданы. Пустые строки, `null`, пустой объект и неизвестные поля в PATCH отклоняются с `400`. Некорректный ID также даёт `400`; отсутствующий пользователь — `404`; занятый email — `409` с текстом `учётная запись с данным email уже зарегистрирована`.

## Файлы

| Файл | Назначение |
| --- | --- |
| `main.go` | HTTP-обработчики, валидация, пул соединений и SQL |
| `Dockerfile` | Сборка Go и образ с запуском от непривилегированного пользователя |
| `helm/postgres/values.yaml` | Настройки Helm-чарта PostgreSQL |
| `K8s/ns.yaml` | Namespace `hs` |
| `K8s/app-configmap.yaml` | Адрес, порт и имя БД |
| `K8s/dp.yaml` | Deployment приложения, Secret и пробы |
| `K8s/svc.yaml` | Service приложения, порт 80 → 8000 |
| `K8s/ingress.yaml` | Маршруты `arch.homework` |
| `migrations/001_create_users.sql` | SQL создания таблицы |
| `K8s/migrations-configmap.yaml` | SQL как файл для контейнера миграции |
| `K8s/migrations-job.yaml` | Однократное выполнение SQL через psql |
| `K8s/postman/users-api.postman_collection.json` | Семь запросов и автоматические проверки CRUD |

## Подготовка

Инструкция рассчитана на macOS с Docker Desktop и Minikube с Docker-драйвером. Нужны Docker, Minikube, kubectl и Helm. Для Newman нужны Node.js LTS и npm; для локальной компиляции — Go версии из `go.mod`. При сборке Docker компилятор берётся из образа.

Версии компонентов проверенного окружения: Helm `4.2.4`, чарт PostgreSQL `0.21.2`, образ миграции `postgres:18.6` и образ приложения `artemprotchenko/myapp:users-update-v1`.

Все команды ниже выполняются из корня репозитория, кроме явно отмеченных действий. Символ `\` переносит одну команду на следующую строку: после него не должно быть пробелов.

```bash
git clone https://github.com/Crank1986/health-service.git
cd health-service
```

Если репозиторий уже скачан, перейдите в его каталог. Запустите Docker Desktop, затем:

```bash
minikube start --driver=docker
kubectl config current-context
kubectl apply -f K8s/ns.yaml
kubectl get storageclass
```

Контекст должен указывать на нужный Minikube. В `values.yaml` указан StorageClass `standard` и том на `1Gi`; такой класс должен существовать.

## 1. Secret для PostgreSQL

Если `postgres-credentials` уже создан для существующей БД, используйте его. Изменение Secret само по себе не меняет пароль роли в уже инициализированной БД.

Для первого запуска создайте приватный файл **вне репозитория**:

```bash
mkdir -p "$HOME/.config/health-service"
chmod 700 "$HOME/.config/health-service"
```

Сохраните туда файл `postgres-credentials.yaml`, заменив оба значения паролей собственными разными паролями:

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: postgres-credentials
  namespace: hs
type: Opaque
stringData:
  postgres-password: "REPLACE_WITH_ADMIN_PASSWORD"
  CUSTOM_USER: "user_app"
  CUSTOM_PASSWORD: "REPLACE_WITH_APP_PASSWORD"
```

```bash
chmod 600 "$HOME/.config/health-service/postgres-credentials.yaml"
kubectl apply -f "$HOME/.config/health-service/postgres-credentials.yaml"
kubectl describe secret postgres-credentials -n hs
```

`describe` показывает имена ключей и размеры, не значения. `postgres-password` нужен администратору, `CUSTOM_PASSWORD` — приложению и миграции. Не добавляйте файл с паролями в Git. Secret отделяет учётные данные от обычных настроек, но сам по себе не гарантирует шифрование данных в хранилище Kubernetes.

## 2. PostgreSQL через Helm

Для первой установки:

```bash
helm install users-db \
  oci://registry-1.docker.io/cloudpirates/postgres \
  --version 0.21.2 \
  --namespace hs \
  --values helm/postgres/values.yaml \
  --wait \
  --timeout 5m

kubectl get pods,svc,pvc -n hs -l app.kubernetes.io/instance=users-db
```

Ожидаются Pod `users-db-postgres-0` в состоянии `Running`, Service `users-db-postgres` на порту `5432` и PVC `data-users-db-postgres-0` в состоянии `Bound`.

Helm создаёт роль `user_app` и БД `users_db` при первоначальной инициализации. Если release уже установлен, не выполняйте `helm install` повторно; проверьте `helm list -n hs` и состояние ресурсов.

## 3. Создание таблицы через Job

```bash
kubectl apply -f K8s/migrations-configmap.yaml
kubectl apply -f K8s/migrations-job.yaml
kubectl wait -n hs --for=condition=complete job/users-migration-001 --timeout=120s
kubectl logs -n hs job/users-migration-001
```

Ожидаемый лог: `CREATE TABLE`. При неудаче изучите `kubectl describe job users-migration-001 -n hs` и логи Job.

ConfigMap превращает SQL в файл `/migrations/001_create_users.sql` внутри контейнера. `psql -f` исполняет этот файл; локальный каталог проекта контейнеру не требуется.

Миграция рассчитана на пустую БД: повторный SQL `CREATE TABLE users` завершится ошибкой, если таблица уже существует. Повторный `apply` завершённого неизменённого Job не запускает его снова. Не удаляйте и не пересоздавайте Job для обычного перезапуска приложения.

SQL хранится и в `migrations/`, и в ConfigMap. Если исходный SQL меняется до первого запуска, обновите ConfigMap:

```bash
kubectl create configmap users-migrations -n hs \
  --from-file=001_create_users.sql=migrations/001_create_users.sql \
  --dry-run=client -o yaml > K8s/migrations-configmap.yaml
```

Эта команда только генерирует YAML. Для изменения уже существующей схемы нужна следующая миграция.

## 4. Приложение

Deployment использует опубликованный образ `artemprotchenko/myapp:users-update-v1`.

```bash
kubectl apply -f K8s/app-configmap.yaml
kubectl apply -f K8s/svc.yaml
kubectl apply -f K8s/dp.yaml
kubectl rollout status deployment/health-service -n hs --timeout=120s
kubectl logs -n hs deployment/health-service --tail=30
```

Ожидаемые сообщения: `Соединение с PostgreSQL проверено` и `Сервер запущен на :8000`. Приложение проверяет соединение с БД при старте; без обязательных переменных окружения оно не запустится.

Для публикации собственного образа замените `YOUR_DOCKERHUB_USER` на свой логин:

```bash
docker build -t YOUR_DOCKERHUB_USER/myapp:users-update-v1 .
docker login
docker push YOUR_DOCKERHUB_USER/myapp:users-update-v1
```

Затем укажите тот же образ в `K8s/dp.yaml` и повторите `apply` и `rollout status`. Для следующих изменений используйте новый тег.

## 5. Ingress и arch.homework

Проверьте наличие Helm-release `nginx` и Service контроллера в namespace `hs`:

```bash
helm list -n hs
kubectl get service nginx-ingress-nginx-controller -n hs
```

Если контроллер отсутствует, установите его:

```bash
helm repo add ingress-nginx https://kubernetes.github.io/ingress-nginx/
helm repo update
helm install nginx ingress-nginx/ingress-nginx \
  --version 4.15.1 \
  --namespace hs \
  --set controller.service.type=LoadBalancer \
  --wait --timeout 5m
```

Конфигурация предназначена для локального Minikube. Если контроллер с классом `nginx` уже работает, повторная установка не требуется.

```bash
kubectl apply -f K8s/ingress.yaml
```

На Mac добавьте в `/etc/hosts` строку, если её ещё нет:

```text
127.0.0.1 arch.homework
```

Файл можно открыть через `sudo nano /etc/hosts`. В nano: `Ctrl+O`, Enter — сохранить; `Ctrl+X` — выйти. Для sudo нужен пароль macOS. Проверьте разрешение имени:

```bash
dscacheutil -q host -a name arch.homework
```

В отдельном терминале запустите и оставьте работать:

```bash
minikube tunnel
```

В другом терминале:

```bash
curl -i --max-time 10 http://arch.homework/health/
curl -i --max-time 10 http://arch.homework/otusapp/artem/health/
```

Оба маршрута должны возвращать `200` и `{"status":"OK"}`. Первый Ingress направляет `/health/` и `/users` без изменения пути. Второй удаляет префикс `/otusapp/artem` перед отправкой запроса приложению.

Для диагностики приложения в обход Ingress настройте перенаправление локального порта:

```bash
kubectl port-forward -n hs service/health-app-service 8080:80
```

Пока команда работает, приложение доступно через `http://localhost:8080`. Для `arch.homework` этот port-forward не нужен.

## 6. Автоматические проверки Postman / Newman

Коллекция: [users-api.postman_collection.json](K8s/postman/users-api.postman_collection.json). Её можно импортировать в Postman и выполнить через Collection Runner. `base_url` должен быть `http://arch.homework`.

Порядок запросов: Health → создание → чтение → PATCH имени → чтение обновления → удаление → проверка отсутствия. Email генерируется заново при каждом прогоне, ID сохраняется из ответа POST. Сценарий создаёт и удаляет собственного пользователя.

Для запуска из терминала нужны Node.js LTS и npm:

```bash
node --version
npm --version
```

При первом запуске `npx` предложит скачать Newman. Глобальная установка не требуется:

```bash
NO_PROXY=arch.homework,localhost,127.0.0.1 \
no_proxy=arch.homework,localhost,127.0.0.1 \
npx newman run K8s/postman/users-api.postman_collection.json \
  --env-var "base_url=http://arch.homework"
```

`NO_PROXY` / `no_proxy` направляют локальные запросы напрямую; значения перед командой действуют только на этот запуск. Эти настройки позволяют исключить прокси как причину `ECONNRESET` при обращении к локальному сервису. `--env-var` задаёт переменную Postman с приоритетом над одноимённой переменной коллекции.

Результат прогона 2 ноября 2026 года: **7 запросов, 18 проверок, 0 ошибок** через `arch.homework`. Последний `404` ожидаем: пользователь удалён. Это функциональный сценарий, а не нагрузочный тест. Проверки пустых полей, null и конфликта email проводились отдельно вручную и пока не включены в эту коллекцию.

## Полезные команды и флаги

Подключение к PostgreSQL:

```bash
kubectl exec -it users-db-postgres-0 -n hs -- \
  psql -h users-db-postgres -U user_app -d users_db -W
```

Введите пароль `CUSTOM_PASSWORD`. В psql: `\dt` — таблицы, `\d users` — структура таблицы, `\q` — выход.

| Элемент | Значение |
| --- | --- |
| `kubectl exec` | Выполнить программу в контейнере Pod |
| `-i`, `-t` | Interactive stdin и terminal: ввод с клавиатуры и терминальная сессия |
| `-n` | Namespace: пространство имён |
| `--` | Конец параметров kubectl, далее команда внутри контейнера |
| `psql -h` | Host: адрес PostgreSQL, здесь имя Service |
| `psql -U` | Username: роль БД |
| `psql -d` | Database: имя базы данных |
| `psql -W` | Запросить пароль; не определяет права чтения/записи |
| `psql -w` | Не спрашивать пароль интерактивно; Job использует PGPASSWORD |
| `psql -f` | File: выполнить SQL из файла |
| `psql -v ON_ERROR_STOP=1` | Задать переменную psql: остановиться при SQL-ошибке |
| `kubectl apply -f` | Применить описание ресурсов из файла |
| `helm --values` | Файл настроек чарта |
| `--wait`, `--timeout` | Дождаться результата с ограничением времени |
| `curl -i` | Include: показать заголовки ответа |
| `curl -X` | Задать HTTP-метод |
| `curl -H` | Header: передать заголовок |
| `curl -d` | Data: тело запроса |
| `docker build -t` | Tag: имя и тег собираемого образа |
| `docker build ... .` | Текущий каталог — контекст сборки |

## Диагностика и остановка окружения

| Симптом | Что проверить |
| --- | --- |
| `405 Method Not Allowed` | Метод и путь: POST `/users`, PATCH `/users/{id}` |
| Ошибка положительного ID | В URL подставилось число, а не буквальное `{{user_id}}`; сначала выполнен POST |
| Newman `ECONNRESET`, но curl работает | Запуск с NO_PROXY/no_proxy из раздела выше |
| npm `EACCES` при глобальной установке | Используйте npx, как в инструкции |
| Нет соединения с `arch.homework:80` | Docker Desktop, Minikube, `/etc/hosts`, tunnel и Service контроллера |
| Приложение перезапускается | Логи Pod, Secret, ConfigMap и доступность PostgreSQL |

Для освобождения памяти завершите tunnel и port-forward через `Ctrl+C`, затем:

```bash
minikube stop
```

После этого можно завершить Docker Desktop. Для продолжения снова запустите Docker Desktop, `minikube start` и `minikube tunnel`. Не используйте `minikube delete` или удаление PVC как обычный способ остановки: это может уничтожить данные БД.

## Текущие ограничения

- `/health/` проверяет HTTP-приложение; после запуска доступность БД через этот endpoint не проверяется. Liveness и readiness используют один маршрут.
- Проверки формата email, длины строк и ограничения размера HTTP-тела пока не реализованы. POST и PATCH имеют разную строгость разбора JSON.
- SQL-миграция одна, автоматического учёта версий миграций пока нет.
- `docker-compose.yaml` не содержит PostgreSQL и переменных DB_*. Для текущей версии используйте описанный Kubernetes-сценарий.
- Компоненты проверены в существующем кластере. Полное развёртывание с нуля по этой инструкции отдельно не проверялось.
