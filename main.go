package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// httpRequestDuration хранит распределение длительности запросов.
var httpRequestDuration = prometheus.NewHistogramVec(
	prometheus.HistogramOpts{
		// Имя метрики содержит единицу измерения - секунды.
		Name: "http_request_duration_seconds",
		// Описание метрики для строки в # HELP.
		Help: "HTTP request duration in seconds.",
		//Верхние границы корзин в секундах: от 1мс до 10 секунд.
		Buckets: []float64{
			0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
		},
	},
	// Для каждого HTTP-метода и шаблона маршрута будет своя гистограмма.
	[]string{"method", "route"},
)

// httpRequestDurationMax показывает максимальную длительность
// обработки запроса с момента запуска процесса.
var httpRequestDurationMax = prometheus.NewGaugeVec(
	prometheus.GaugeOpts{
		Name: "http_request_duration_max_seconds",
		Help: "Maximum HTTP request duration since process start in seconds.",
	},
	// Для каждого метода и маршрута храим отдельный максимум
	[]string{"method", "route"},
)

// Ключ обозначает конкретную пару: HTTP-метод и шаблон маршрута.
type requestMetricKey struct {
	method string
	route  string
}

// Для каждой пары храним одно число — максимальную длительность.
var requestDurationMaxValues = make(map[requestMetricKey]float64)

// Защищаем совместное чтение и обновление рекордов.
var requestDurationMaxMu sync.Mutex

func observeRequestDurationMax(method, route string, duration float64) {
	key := requestMetricKey{
		method: method,
		route:  route,
	}

	// Только один запрос за раз может проверять и обновлять рекорд.
	requestDurationMaxMu.Lock()

	// Освободим замок при выходе из функции, включая ранний return.
	defer requestDurationMaxMu.Unlock()

	previous, exists := requestDurationMaxValues[key]

	// Если рекорд уже есть и новое время не больше — ничего не меняем.
	if exists && duration <= previous {
		return
	}

	// Запоминаем новый рекорд и показываем его в метрике Prometheus.
	requestDurationMaxValues[key] = duration
	httpRequestDurationMax.WithLabelValues(method, route).Set(duration)
}

// httpRequestsTotal хранит счётчики запросов для разных сочетаний меток.
var httpRequestsTotal = prometheus.NewCounterVec(
	prometheus.CounterOpts{
		// Name — имя метрики; суффикс _total обозначает накопительный счётчик.
		Name: "http_requests_total",
		// Help — описание, которое появится в строке # HELP.
		Help: "Total number of HTTP requests.",
	},
	// Метки разделяют счётчики по HTTP-методу, шаблону маршрута и статусу ответа.
	[]string{"method", "route", "status"},
)

// responseRecorder передаёт ответ клиенту и хранит его HTTP-статус.
type responseRecorder struct {
	// Встраиваем исходный ResponseWriter, через который отправляется ответ.
	http.ResponseWriter

	// status хранит код ответа для последующего учёта в метриках.
	status int
}

// WriteHeader запоминает итоговый статус и передаёт его клиенту.
func (rec *responseRecorder) WriteHeader(code int) {
	// Если итоговый статус уже отправлен, повторный вызов его не изменит.
	// Выходим, чтобы сохранить первый статус.
	if rec.status != 0 {
		return
	}

	// Ответы 1xx — промежуточные: передаём их, но не считаем итоговым статусом.
	if code >= 100 && code < 200 {
		rec.ResponseWriter.WriteHeader(code)
		return
	}

	// Запоминаем итоговый статус для метрик этого запроса.
	rec.status = code

	// Отправляем статус через исходный ResponseWriter.
	rec.ResponseWriter.WriteHeader(code)
}

// Write отправляет тело ответа и учитывает неявный статус 200.
func (rec *responseRecorder) Write(data []byte) (int, error) {
	// Если итогового статуса ещё нет, устанавливаем 200 через наш метод.
	// Он одновременно запомнит статус и передаст его исходному ResponseWriter.
	if rec.status == 0 {
		rec.WriteHeader(http.StatusOK)
	}

	// Передаём тело исходному ResponseWriter.
	// Возвращаем вызывающему коду число записанных байтов и возможную ошибку.
	return rec.ResponseWriter.Write(data)
}

// loggingMiddleware записывает входящий запрос и передаёт его дальше
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// RemoteAddr содержит адрес непосредственного отправителя: IP и порт
		// Отделяем IP; порт тут не нужен
		peerIP, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			peerIP = r.RemoteAddr
		}
		// Записываем факт получения запроса до выполнения обработчика
		slog.Info("Входящий HTTP-запрос",
			"event", "http_request_received",
			"method", r.Method,
			"path", r.URL.Path,
			"peer_ip", peerIP,
		)
		next.ServeHTTP(w, r)
	})
}

// metricsMiddleware оборачивает обработчик и считает завершённые запросы.
// route — шаблон маршрута, next — обработчик, который выполнит работу.
func metricsMiddleware(route string, next http.HandlerFunc) http.HandlerFunc {
	// Возвращаем новую функцию: её сервер будет вызывать для каждого запроса.
	return func(w http.ResponseWriter, r *http.Request) {
		// Создаём отдельную обёртку для этого запроса.
		// Внутрь кладём исходный w; status первоначально равен 0.
		rec := &responseRecorder{
			ResponseWriter: w,
		}

		// Запоминаем момент начала обработки запроса.
		started := time.Now()

		// Выполняем обработчик: проверку данных, работу с БД и запись ответа.
		next(rec, r)

		// Вычисляем прошедшее время и переводим его в секунды.
		duration := time.Since(started).Seconds()

		// Observe добавляет измеренную длительность в её статистику.
		// Записываем длительность в гистограмму нужно операции API.
		httpRequestDuration.WithLabelValues(r.Method, route).Observe(duration)

		// Проверяем, стал ли этот запрос новым рекордом длительности.
		observeRequestDurationMax(r.Method, route, duration)

		// Если обработчик ничего не записал, обычный ответ Go будет иметь статус 200.
		if rec.status == 0 {
			rec.status = http.StatusOK
		}

		// Выбираем счётчик по методу, шаблону маршрута и итоговому статусу.
		// Itoa превращает числовой статус в строку; Inc увеличивает счётчик на 1.
		httpRequestsTotal.WithLabelValues(
			r.Method,
			route,
			strconv.Itoa(rec.status),
		).Inc()
	}
}

// User хранит данные пользователя; JSON-теги задают имена полей HTTP-ответа.
type User struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
}

// CreateUserRequest описывает данные для создания пользователя.
// ID сюда не входит: его назначает база данных.
type CreateUserRequest struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

// RegisterRequest содержит имя, email и пароль для регистрации
type RegisterRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Password string `json:"password"`
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type LoginResponse struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
}

// UpdateUserRequest позволяет определить, какие поля клиент хочет изменить.
// nil означает отсутствие значения, а указатель на "" — явно переданную пустую строку.
type UpdateUserRequest struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
}

// hashPassword создаёт случайную соль и вычисляет хеш пароля.
// Возвращает алгоритм, параметры, соль и хеш в одной строке.
func hashPassword(password string) string {
	// Параметры вычисления: память в Kib, число проходов и параллелизм
	const memory = 19 * 1024
	const iterations = 2
	const parallelism = 1
	const keyLength = 32

	// Создаём 16 байт криптографически случайной соли, соли? Ну да мы же в Санкт - Питербурге:DD
	salt := make([]byte, 16)
	rand.Read(salt)

	// Вычисляем Argon2id-хэш из байтов пароля и соли
	hash := argon2.IDKey(
		[]byte(password), salt,
		iterations, memory, parallelism, keyLength,
	)

	return fmt.Sprintf(
		"$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version, memory, iterations, parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(hash),
	)
}

func verifyPassword(password, encodedHash string) (bool, error) {
	// Разделяем строку на алгоритм, версию, параметры, соль и хэш
	parts := strings.Split(encodedHash, "$")
	if len(parts) != 6 {
		return false, fmt.Errorf("некорректный формат хэша")
	}

	// Принимаем только поддерживаемый алгоритм и параметры
	if parts[0] != "" ||
		parts[1] != "argon2id" ||
		parts[2] != fmt.Sprintf("v=%d", argon2.Version) ||
		parts[3] != "m=19456,t=2,p=1" {
		return false, fmt.Errorf("неподдерживаемые параметры хэша")
	}

	// Восстанавливаем байты соли из Base64
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(salt) != 16 {
		return false, fmt.Errorf("некорректная соль хэша")
	}

	// Восстанавливаем сохранённый результат вычисления
	expectedHash, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(expectedHash) != 32 {
		return false, fmt.Errorf("неккоректная длина или кодировка хэша")
	}
	// Вычисляем хэш введённого пароля с той же солью и параметрами
	actualHash := argon2.IDKey(
		[]byte(password), salt,
		2, 19*1024, 1, 32,
	)

	// Сравниваем байты способом, предназначенным для криптографического кода.
	return subtle.ConstantTimeCompare(actualHash, expectedHash) == 1, nil
}

func createAccessToken(userID int64, signingKey []byte) (string, error) {
	// не выдаём токен для некорректного ID или слишком короткого ключа
	if userID <= 0 || len(signingKey) < 32 {
		return "", fmt.Errorf("некорректный ID или ключ подписи JWT")
	}

	// Одно время используем для момента выдачи и расчёта срока действия
	now := time.Now()

	// Заполняем стандартные поля JWT
	claims := jwt.RegisteredClaims{
		Subject:   strconv.FormatInt(userID, 10),
		Issuer:    "health-service",
		Audience:  jwt.ClaimStrings{"health-service-api"},
		IssuedAt:  jwt.NewNumericDate(now),
		ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
	}

	// Выбираем алгоритм и подписываем токен секретным ключом
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString(signingKey)
}

// parseAccessToken проверяет JWT и возвращает ID его владельца
func parseAccessToken(tokenString string, signingKey []byte) (int64, error) {
	// Не проверяем токены с отсутствующим или слишком коротким ключом
	if len(signingKey) < 32 {
		return 0, fmt.Errorf("некорректный ключ проверки JWT")
	}

	// Подготавливаес структуру для полей токена
	var claims jwt.RegisteredClaims

	token, err := jwt.ParseWithClaims(
		tokenString,
		&claims,
		// Библиотека использует этот ключ для проверки подписи
		func(token *jwt.Token) (any, error) {
			return signingKey, nil
		},
		// Разрешаем только выбранный алгоритм
		jwt.WithValidMethods([]string{"HS256"}),
		// Проверяем издателя, получателя и срок действия
		jwt.WithIssuer("health-service"),
		jwt.WithAudience("health-service-api"),
		jwt.WithExpirationRequired(),
	)
	if err != nil || !token.Valid {
		return 0, fmt.Errorf("Недействительный JWT")
	}

	// Используем sub только после успешной проверки токена
	userID, err := strconv.ParseInt(claims.Subject, 10, 64)
	if err != nil || userID <= 0 {
		return 0, fmt.Errorf("некорректный ID пользователя в JWT")
	}

	return userID, nil
}

// requireOwnerMiddleware разрешает операцию только владельцу профиля
func requireOwnerMiddleware(
	signingKey []byte,
	next http.HandlerFunc,
) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		//Ожидаем заголовок Authorization: Bearer <JWT>
		parts := strings.Fields(r.Header.Get("Authorization"))
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Требуется вход", http.StatusUnauthorized)
			return
		}

		// Устанавливаем личность только по проверенному токену
		userID, err := parseAccessToken(parts[1], signingKey)
		if err != nil {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "Недействительный токен", http.StatusUnauthorized)
			return
		}

		// Получаем ID профиля с которым клиент хочет работать
		profileID, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
		if err != nil || profileID <= 0 {
			http.Error(w, "ID должен быть положительным целым числом",
				http.StatusBadRequest)
			return
		}

		// Вход выполнен, но доступ к чужому профилю запрещён
		if userID != profileID {
			http.Error(w, "Доступ к чужому профилю запрещён",
				http.StatusForbidden)
			return
		}

		// Проверки пройдены, вызываем обработчик профиля
		next(w, r)
	}
}

// decodeLogin читает запрос на вход и проверяет обязательные поля
func decodeLogin(r *http.Request) (LoginRequest, error) {
	//создаем переменную для данных из JSON
	var input LoginRequest

	// Отклонеяем поля, котрых нет в LoginRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("некорректный JSON: %w", err)
	}

	//разрешаем только одно JSON-значение в теле запроса
	if err := decoder.Decode(new(any)); err != io.EOF {
		return input, fmt.Errorf("тело запроса должно содержать один JSON-объект")
	}

	// Email обрабатываем так же, как при регистрации и пароль не меняем
	input.Email = strings.TrimSpace(input.Email)

	if input.Email == "" || input.Password == "" {
		return input, fmt.Errorf("email и password обязательны")
	}

	return input, nil
}

// decodeRegister читает данные регистрации и проверяет обязательные поля
// Пароль сохраняем в исходном виде и пробелы могут быть его частью
func decodeRegister(r *http.Request) (RegisterRequest, error) {
	var input RegisterRequest

	// Разрешаем только поля, объявленные в RegisterRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(&input); err != nil {
		return input, fmt.Errorf("некорректный JSON: %w", err)
	}

	// После первого JSON-значения тело запроса должно закончиться
	if err := decoder.Decode(new(any)); err != io.EOF {
		return input, fmt.Errorf("тело запроса должно содержать один JSON-объект")
	}

	// Убираем внешние проблемы у имени и email
	input.Name = strings.TrimSpace(input.Name)
	input.Email = strings.TrimSpace(input.Email)

	if input.Name == "" || input.Email == "" || input.Password == "" {
		return input, fmt.Errorf("name, email и password обязательны")
	}

	// Проверяем длину имени и email до записи в VARCHAR(200)
	if utf8.RuneCountInString(input.Name) > 200 ||
		utf8.RuneCountInString(input.Email) > 200 {
		return input, fmt.Errorf("name и email должны содержать не более 200 символов")
	}

	// Проверяем формат email и принимаем только сам адрес, без имени
	address, err := mail.ParseAddress(input.Email)
	if err != nil || address.Address != input.Email {
		return input, fmt.Errorf("email должен содержать корректный адрес электронной почты")
	}

	//Считаем кодовые точки Unicode, чтобы учитывать кириллицу
	passwordLength := utf8.RuneCountInString(input.Password)
	if passwordLength < 15 || passwordLength > 128 {
		return input, fmt.Errorf("пароль должен содержать от 15 до 128 символов")
	}

	return input, nil
}

// decodeCreateUser читает JSON-анкету и проверяет обязательные поля.
// В БД эта функция ничего не записывает.
func decodeCreateUser(r *http.Request) (CreateUserRequest, error) {
	var input CreateUserRequest

	if err := json.NewDecoder(r.Body).Decode(&input); err != nil {
		return input, fmt.Errorf("некорректный JSON: %w", err)
	}

	input.Name = strings.TrimSpace(input.Name)
	input.Email = strings.TrimSpace(input.Email)
	if input.Name == "" || input.Email == "" {
		return input, fmt.Errorf("name и email обязательны")
	}

	return input, nil
}

// decodeUpdateUser проверяет бланк изменений.
// Возвращает только те поля, которые клиент хочет обновить.
func decodeUpdateUser(r *http.Request) (UpdateUserRequest, error) {
	var input UpdateUserRequest

	// Словарь сохраняет имена присланных полей.
	// Это позволяет отличить пропущенное поле от явно переданного null.
	var fields map[string]*string
	decoder := json.NewDecoder(r.Body)

	if err := decoder.Decode(&fields); err != nil {
		return input, fmt.Errorf("некорректный JSON: %w", err)
	}

	// После первого JSON должно закончиться тело запроса.
	if err := decoder.Decode(new(any)); err != io.EOF {
		return input, fmt.Errorf("тело запроса должно содержать один JSON-объект")
	}

	if len(fields) == 0 {
		return input, fmt.Errorf("укажите name или email для изменения")
	}

	for key, value := range fields {
		if key != "name" && key != "email" {
			return input, fmt.Errorf("неизвестное поле: %s", key)
		}

		// Поле прислали, но null означает отсутствие значения — такое изменение запрещаем.
		// Проверяем до обращения через *value: чтение через nil вызовет панику.
		if value == nil {
			return input, fmt.Errorf("%s не может быть null", key)
		}

		// *value — сам текст по указателю: убираем у него пробелы по краям.
		// Если после этого строка пуста, клиент пытается стереть обязательное поле.
		*value = strings.TrimSpace(*value)
		if *value == "" {
			return input, fmt.Errorf("%s не может быть пустым", key)
		}

		// По названию поля кладём указатель на проверенный текст в нужную графу input.
		// Поля, которых не было в JSON, останутся nil: их в БД менять не нужно.
		switch key {
		case "name":
			input.Name = value
		case "email":
			input.Email = value
		}
	}

	return input, nil
}

// loginHandler проверяет учётные данные и вадаёт JWT
func loginHandler(
	w http.ResponseWriter,
	r *http.Request,
	pool *pgxpool.Pool,
	signingKey []byte,
) {
	// Читаем email и пароль из запроса
	input, err := decodeLogin(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Ограничиваем время обращения к БД
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// У старых пользователей хэш может отсутствовать
	var userID int64
	var passwordHash *string

	err = pool.QueryRow(
		ctx,
		"SELECT id, password_hash FROM users WHERE email = $1",
		input.Email,
	).Scan(&userID, &passwordHash)

	// Не раскрываем клиенту, существует ли указанный емэйл или нет
	if errors.Is(err, pgx.ErrNoRows) {
		http.Error(w, "Неверный email или пароль", http.StatusUnauthorized)
		return
	}

	// другие ошибки БД не означают неправильный пароль
	if err != nil {
		slog.Error("Ошибка поиска пользователя при входе",
			"event", "database_operation_failed",
			"operation", "login",
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// У старых записей может не быть пароля
	if passwordHash == nil {
		http.Error(w, "Неверный email или пароль", http.StatusUnauthorized)
		return
	}

	// Проверяем пароль по сохранённым соли и хэшу
	matches, err := verifyPassword(input.Password, *passwordHash)
	if err != nil {
		slog.Error("Не удалось проверить сохранённый хэш",
			"event", "password_verification_failed",
			"user_id", userID,
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	if !matches {
		http.Error(w, "Неверный email или пароль", http.StatusUnauthorized)
		return
	}

	// Создаём JWT после успешной проверки пароля
	accessToken, err := createAccessToken(userID, signingKey)
	if err != nil {
		slog.Error("Не удалось создать JWT",
			"event", "token_creation_failed",
			"user_id", userID,
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// Готовим ответ, пароль и его хэш в него не входят
	response := LoginResponse{
		AccessToken: accessToken,
		TokenType:   "Bearer",
	}
	data, err := json.Marshal(response)
	if err != nil {
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// Просим клиентов и посредников не сохранять ответ с токеном в кеше
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

// registerHandler принимает анкету, регистрирует пользователя и возвращает запись с ID.
func registerHandler(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) {
	input, err := decodeRegister(r)
	if err != nil {
		// Данные не прошли проверку, запись в БД не появилась
		slog.Warn("Некорректные данные для создания пользователя",
			"event", "validation_failed",
			"operation", "register_user",
			"reason", err.Error(),
		)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Проверенные данные готовы: вычисляем хэш
	passwordHash := hashPassword(input.Password)

	// Передаём значения отдельно от SQL; ID назначает база данных.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var user User
	err = pool.QueryRow(
		ctx,
		`INSERT INTO users (name, email, password_hash)
		VALUES ($1, $2, $3)
		RETURNING id, name, email`,
		input.Name,
		input.Email,
		passwordHash,
	).Scan(&user.ID, &user.Name, &user.Email)

	// Код 23505 означает нарушение UNIQUE. Отвечаем понятной ошибкой клиенту.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		slog.Warn("Email уже зарегистрирован",
			"event", "user_email_conflict",
			"operation", "register_user",
		)
		http.Error(w, "учётная запись с данным email уже зарегистрирована", http.StatusConflict)
		return
	}
	if err != nil {
		// Операция создания не удалась из за ошибки БД
		slog.Error("Ошибка создания пользователя",
			"event", "database_operation_failed",
			"operation", "register_user",
			"error", err.Error(),
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// INSERT завершился успешно, БД вернула ID созданной записи
	slog.Info("Пользователь зарегистрирован",
		"event", "user_registered",
		"operation", "register_user",
		"user_id", user.ID,
	)

	// Сначала готовим JSON, затем отправляем статус 201 и тело ответа.
	data, err := json.Marshal(user)
	if err != nil {
		slog.Error("Не удалось сформировать JSON-ответ",
			"event", "response_encoding_failed",
			"operation", "register_user",
			"user_id", user.ID,
			"error", err.Error(),
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

// createUserHandler принимает анкету, сохраняет пользователя и возвращает запись с ID.
func createUserHandler(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) {
	input, err := decodeCreateUser(r)
	if err != nil {
		// Данные не прошли проверку, запись в БД не появилась
		slog.Warn("Некорректные данные для создания пользователя",
			"event", "validation_failed",
			"operation", "create_user",
			"reason", err.Error(),
		)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// Передаём значения отдельно от SQL; ID назначает база данных.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var user User
	err = pool.QueryRow(
		ctx,
		"INSERT INTO users (name, email) VALUES ($1, $2) RETURNING id, name, email",
		input.Name,
		input.Email).Scan(&user.ID, &user.Name, &user.Email)
	// Код 23505 означает нарушение UNIQUE. Отвечаем понятной ошибкой клиенту.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		slog.Warn("Email уже зарегистрирован",
			"event", "user_email_conflict",
			"operation", "create_user",
		)
		http.Error(w, "учётная запись с данным email уже зарегистрирована", http.StatusConflict)
		return
	}
	if err != nil {
		// Операция создания не удалась из за ошибки БД
		slog.Error("Ошибка создания пользователя",
			"event", "database_operation_failed",
			"operation", "create_user",
			"error", err.Error(),
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// INSERT завершился успешно, БД вернула ID созданной записи
	slog.Info("Пользователь создан",
		"event", "user_created",
		"operation", "create_user",
		"user_id", user.ID,
	)

	// Сначала готовим JSON, затем отправляем статус 201 и тело ответа.
	data, err := json.Marshal(user)
	if err != nil {
		slog.Error("Не удалось сформировать JSON-ответ",
			"event", "response_encoding_failed",
			"operation", "create_user",
			"user_id", user.ID,
			"error", err.Error(),
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(data)
}

// deleteUserHandler удаляет пользователя по ID из URL.
// Если запись отсутствует, возвращает 404.
func deleteUserHandler(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		slog.Warn("Некорректный ID пользователя",
			"event", "validation_failed",
			"operation", "delete_user",
			"field", "id",
			"reason", "expected_positive_integer",
		)
		http.Error(w, "ID должен быть положительным целым числом", http.StatusBadRequest)
		return
	}
	// Ограничиваем время запроса и учитываем отмену HTTP-запроса.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var deletedID int64
	err = pool.QueryRow(
		ctx,
		"DELETE FROM users u WHERE u.id = $1 RETURNING u.id",
		id).Scan(&deletedID)

	// RETURNING не вернул строку — пользователя с таким ID не было.
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("Пользователь не найден",
			"event", "user_not_found",
			"operation", "delete_user",
			"user_id", id,
		)
		http.Error(w, "Пользователь не найден", http.StatusNotFound)
		return
	}
	if err != nil {
		slog.Error("Ошибка удаления пользователя",
			"event", "database_operation_failed",
			"operation", "delete_user",
			"user_id", id,
			"error", err.Error(),
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}
	// DELETE завершился успешно и вернул корректный ID
	slog.Info("Пользователь удалён",
		"event", "user_deleted",
		"operation", "delete_user",
		"user_id", deletedID,
	)
	// Удаление успешно. Статус 204 отправляется без тела ответа.
	w.WriteHeader(http.StatusNoContent)
}

// updateUserHandler проверяет номер карточки и присланные изменения.
// Обновление записи в БД добавим следующим шагом.
// w нужен для ответа клиенту, r содержит запрос, pool даёт доступ к соединениям с БД.
func updateUserHandler(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) {
	// Объявляем id (номер пользователя) и err (ошибка преобразования).
	// PathValue берёт текст из {id}; ParseInt превращает его в десятичное число int64.
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	// Если текст не удалось преобразовать ИЛИ число неположительное, отклоняем запрос.
	if err != nil || id <= 0 {
		// до обращения к БД отклоняем некорректный ID
		slog.Warn("Некорректный ID пользователя",
			"event", "validation_failed",
			"operation", "update_user",
			"field", "id",
			"reason", "expected_positive_integer",
		)
		// Отправляем клиенту статус 400 и объяснение ошибки.
		http.Error(w, "ID должен быть положительным целым числом", http.StatusBadRequest)
		// Завершаем обработчик, чтобы не продолжать работу с неверным ID.
		return
	}

	// Объявляем input с проверенными изменениями; существующую err используем повторно.
	// decodeUpdateUser читает JSON, проверяет поля и возвращает изменения либо ошибку.
	input, err := decodeUpdateUser(r)
	// Если проверка JSON или его полей не прошла, до базы запрос не допускаем.
	if err != nil {
		// ID корректен, но данные для изменения не прошли проверку
		slog.Warn("Некорректные данные для обновлениия пользователя",
			"event", "validation_failed",
			"operation", "update_user",
			"user_id", id,
			"reason", err.Error(),
		)
		// Отправляем статус 400 с текстом ошибки проверки и завершаем обработчик.
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// ctx ограничивает время работы с БД пятью секундами.
	// Отмена запроса клиентом также передаётся в этот контекст.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)

	// При выходе из обработчика освобождаем ресурсы таймера.
	defer cancel()

	// user — место для обновлённой карточки, которую вернёт PostgreSQL.
	var user User

	// Передаём значения отдельно от SQL: $1 — имя, $2 — email, $3 — ID.
	// Если указатель равен nil, pgx передаст NULL, а COALESCE сохранит старое поле.
	err = pool.QueryRow(
		ctx,
		`UPDATE users
		SET name = COALESCE($1, name),
			email = COALESCE($2, email)
		WHERE id = $3
		RETURNING id, name, email`,
		input.Name,
		input.Email,
		id,
	).Scan(&user.ID, &user.Name, &user.Email)
	// Scan записывает возвращённые столбцы в поля user по порядку.
	// Ошибку выполнения запроса мы также получаем через Scan.

	// Если UPDATE не нашёл строку с таким ID, возвращать карточку нечего.
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("Пользователь не найден",
			"event", "user_not_found",
			"operation", "update_user",
			"user_id", id,
		)
		http.Error(w, "Пользователь не найден", http.StatusNotFound)
		return
	}

	// pgErr нужен, чтобы прочитать код и название ограничения из ошибки PostgreSQL.
	var pgErr *pgconn.PgError

	// 23505 означает нарушение уникальности; users_email_key — ограничение email.
	// Если новый email уже занят, возвращаем 409 и прекращаем обработку.
	if errors.As(err, &pgErr) &&
		pgErr.Code == "23505" &&
		pgErr.ConstraintName == "users_email_key" {
		slog.Warn("Email уже зарегистрирован",
			"event", "user_email_conflict",
			"operation", "update_user",
			"user_id", id,
		)
		http.Error(w, "учётная запись с данным email уже зарегистрирована", http.StatusConflict)
		return
	}

	// Любая оставшаяся ошибка означает, что операцию не удалось выполнить.
	// Подробности пишем в логи, клиенту отправляем общее сообщение.
	if err != nil {
		slog.Error("Ошибка обновления пользователя",
			"event", "database_operation_failed",
			"operation", "update_user",
			"user_id", id,
			"error", err.Error(),
		)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// UPDATE завершился успешно и вернул обновлённую запись
	slog.Info("Пользователь обновлён",
		"event", "user_updated",
		"operation", "update_user",
		"user_id", user.ID,
	)

	// Преобразуем обновлённую карточку в JSON для ответа клиенту.
	data, err := json.Marshal(user)
	// Если сформировать JSON не получилось, возвращаем 500.
	if err != nil {
		slog.Error("Не удалось сформировать JSON-ответ",
			"event", "response_encoding_failed",
			"operation", "update_user",
			"user_id", user.ID,
			"error", err.Error(),
		)
		http.Error(w, "Внутрення ошибка сервера", http.StatusInternalServerError)
		return
	}

	// Сообщаем формат ответа, отправляем статус 200 и сам JSON.
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(data)
}

func handler(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) {
	// Получаем ID из /users/{id} и проверяем его до обращения к базе.
	idText := r.PathValue("id")
	id, err := strconv.ParseInt(idText, 10, 64)
	if err != nil || id <= 0 {
		// Запро сотклонён из за некорректно ID, до обращения к БД
		slog.Warn("Некорректный ID пользователя",
			"event", "validation_failed",
			"operation", "get_user",
			"field", "id",
			"reason", "expected_positive_integer",
		)
		http.Error(w, "ID должен быть положительным, целым числом", http.StatusBadRequest)
		return
	}
	// Ограничиваем запрос к БД пятью секундами; отмена HTTP-запроса также отменяет ожидание.
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	var user User

	// Передаём ID отдельно от SQL через $1. Scan записывает столбцы в поля структуры по порядку.
	// QueryRow использует общий пул, а Scan возвращает результат или ошибку запроса.
	err = pool.QueryRow(
		ctx,
		"SELECT u.id, u.name, u.email FROM users u WHERE u.id = $1",
		id).Scan(&user.ID, &user.Name, &user.Email)

	// Отсутствующая запись — это 404, а не сбой базы данных.
	if errors.Is(err, pgx.ErrNoRows) {
		slog.Info("Пользователь не найден",
			"event", "user_not_found",
			"operation", "get_user",
			"user_id", id,
		)
		http.Error(w, "Пользователь не найден", http.StatusNotFound)
		return
	}

	// Подробности сбоя оставляем в логах, клиенту возвращаем общее сообщение.
	if err != nil {
		slog.Error("Ошибка получения пользователя",
			"event", "database_operation_failed",
			"operation", "get_user",
			"user_id", id,
			"error", err.Error(),
		)
		http.Error(w, "Внутреннеяя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// БД успешно вернула найденного пользователя
	slog.Info("Пользователь получен",
		"event", "user_read",
		"operation", "get_user",
		"user_id", user.ID,
	)

	// Превращаем найденную запись в JSON и задаём тип ответа до записи его тела.
	data, err := json.Marshal(user)
	if err != nil {
		slog.Error("Не удалось сформировать JSON-ответ",
			"event", "response_encoding_failed",
			"operation", "get_user",
			"user_id", user.ID,
			"error", err.Error(),
		)
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	// Проверяем, что HTTP-сервер отвечает; доступность БД этот обработчик не проверяет.
	response := map[string]string{"status": "OK"}
	data, _ := json.Marshal(response)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func main() {
	// Формируем каждую запись лога как JSON и выводим в stdout.
	logHandler := slog.NewJSONHandler(os.Stdout, nil)

	// Добавляем имя сервиса ко всем записям этого логгера
	logger := slog.New(logHandler).With("service", "health-service")

	// Делаем логгер обищим для вызовов slog.info, slog.Warn и skog.Error.
	slog.SetDefault(logger)

	// Отмечаем начало запуска: подклчение к БД ещё предстоит проверить
	slog.Info("Запуск приложения", "event", "application_starting")

	// Подключаем семейство счётчиков к стандартному реестру Prometheus.
	// Его содержимое выдаёт уже добавленный promhttp.Handler().
	// Регистрируем метрики, которые будет отдавать обработчик /metrics.
	prometheus.MustRegister(
		httpRequestsTotal,
		httpRequestDuration,
		httpRequestDurationMax,
	)

	// Kubernetes передаёт настройки из ConfigMap и Secret в окружение контейнера.
	// Go читает обычные переменные окружения, не обращаясь к Kubernetes API.
	dbHost := os.Getenv("DB_HOST")
	dbPort := os.Getenv("DB_PORT")
	dbName := os.Getenv("DB_NAME")
	dbUser := os.Getenv("DB_USER")
	dbPassword := os.Getenv("DB_PASSWORD")
	// Не запускаем сервер без обязательных настроек. Значения паролей не выводим.
	if dbHost == "" || dbPort == "" || dbName == "" ||
		dbUser == "" || dbPassword == "" {
		slog.Error("Не заданы обязательные настройки подключения к БД", "event", "configuration_invalid")
		os.Exit(1)
	}

	// Читаем общий ключ подписи JWT из окружения контейнера
	signingKey := []byte(os.Getenv("JWT_SIGNING_KEY"))

	// Без подходящего ключа приложение не должно выдавать токены
	if len(signingKey) < 32 {
		slog.Error("JWT_SIGNING_KEY должен содержать не менее 32 байт",
			"event", "configuration_invalid",
		)
		os.Exit(1)
	}

	// Собираем URL с корректным кодированием специальных символов в учётных данных.
	// Полный URL содержит пароль, поэтому его нельзя печатать в логах.
	dbUrl := url.URL{
		Scheme: "postgres",
		User:   url.UserPassword(dbUser, dbPassword),
		Host:   net.JoinHostPort(dbHost, dbPort),
		Path:   dbName,
	}
	// Разбор настроек ещё не проверяет, доступна ли база и подходит ли пароль.
	poolConfig, err := pgxpool.ParseConfig(dbUrl.String())
	if err != nil {
		slog.Error("Некорректные настройки подключения к БД", "event", "database_configuration_invalid")
		os.Exit(1)
	}
	// Каждый экземпляр приложения переиспользует свой пул максимум из пяти соединений.
	poolConfig.MaxConns = 5
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)

	if err != nil {
		slog.Error("Не удалось создать пул соединений", "event", "database_pool_failed")
		os.Exit(1)
	}
	// Реально проверяем соединение перед запуском HTTP-сервера, ожидая не более пяти секунд.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = pool.Ping(ctx)
	cancel() // Освобождаем ресурсы контекста проверки; сам пул остаётся работать.

	if err != nil {
		slog.Error("Не удалось подключиться к PostgreSQL", "event", "database_connection_failed")
		pool.Close() // os.Exit не выполняет defer, поэтому здесь за                                            крываем пул явно.
		os.Exit(1)
	}

	defer pool.Close() // Закрываем пул при обычном выходе из main.
	slog.Info("Соединение с PostgreSQL проверено", "event", "database_connected")

	// Функция-посредник передаёт обработчику уже созданный пул, не создавая новый на каждый запрос.
	// Подключаем подсчёт запросов к чтению пользователя.
	http.HandleFunc("GET /users/{id}", metricsMiddleware(
		// Все ID объединяем под одним шаблоном маршрута.
		"/users/{id}",
		// Измеряем запрос и разрешаем чтение только владельцу профиля.
		requireOwnerMiddleware(signingKey,
			// Передаём функцию чтения как задание для обёртки.
			func(w http.ResponseWriter, r *http.Request) {
				// Вызываем существующий обработчик с доступом к БД.
				handler(w, r, pool)
			},
		),
	))

	// Подключаем регистрацию и измерение её запросов
	http.HandleFunc("POST /users/register", metricsMiddleware(
		"/users/register",
		func(w http.ResponseWriter, r *http.Request) {
			// Передаём обработчику регистрации общий пул подключений к БД
			registerHandler(w, r, pool)
		},
	))

	// Подключаем вход и измерение его запросов
	http.HandleFunc("POST /users/login", metricsMiddleware(
		"/users/login",
		func(w http.ResponseWriter, r *http.Request) {
			// Передаём общий пул БД и ключ подписи JWT
			loginHandler(w, r, pool, signingKey)
		},
	))

	// Регистрируем создание пользователя через обёртку подсчёта запросов.
	http.HandleFunc("POST /users", metricsMiddleware(
		// Для метрики передаём шаблон пути; HTTP-метод обёртка возьмёт из запроса.
		"/users",
		// Эта функция станет параметром next внутри metricsMiddleware.
		func(w http.ResponseWriter, r *http.Request) {
			// Передаём запрос и доступ к БД существующему обработчику создания.
			createUserHandler(w, r, pool)
		},
	))

	// Регистрируем удаление пользователя через обёртку подсчёта запросов.
	http.HandleFunc("DELETE /users/{id}", metricsMiddleware(
		// Для метрики передаём шаблон пути; HTTP-метод обёртка возьмёт из запроса.
		"/users/{id}",
		// Удалять профиль может только его владелец.
		requireOwnerMiddleware(signingKey,
			// Передаём функцию удаления как задание для обёртки.
			func(w http.ResponseWriter, r *http.Request) {
				// Вызываем существующий обработчик с доступом к БД.
				deleteUserHandler(w, r, pool)
			},
		),
	))

	// Регистрируем обновление пользователя через обёртку подсчёта запросов.
	http.HandleFunc("PATCH /users/{id}", metricsMiddleware(
		// Все ID объединяем под одним шаблоном маршрута.
		"/users/{id}",
		// Изменять профиль может только его владелец.
		requireOwnerMiddleware(signingKey,
			// Передаём функцию обновления пользователя как задание для обёртки.
			func(w http.ResponseWriter, r *http.Request) {
				// Вызываем существующий обработчик с доступом к БД.
				updateUserHandler(w, r, pool)
			},
		),
	))
	http.HandleFunc("/health/", healthHandler)
	http.Handle("GET /metrics", promhttp.Handler())

	//создаём сервер с адресом и обработчиком запросов
	server := &http.Server{
		Addr:    ":8000",
		Handler: loggingMiddleware(http.DefaultServeMux),
	}

	// Сообщаем о попытке запуска, а не о подтверждённой готовности
	slog.Info("Запуск HTTP-сервера",
		"event", "http_server_starting",
		"address", server.Addr,
	)

	// При SIGINT или SIGTERM этот контекст будет отменён
	stopCtx, stop := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGTERM,
	)

	// Освобождаем реусры подписки при выходе из main
	defer stop()

	// Канал может хранить один результат типа error
	serverErrors := make(chan error, 1)

	// Запускаем обслуживание запросов в gorutine
	go func() {
		// После завершения сервера отправляем его результат в канал
		serverErrors <- server.ListenAndServe()
	}()

	// Ждём одного из двух событий: завершения сервера или сигнала
	select {
	case err = <-serverErrors:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Ошибка HTTP-Сервера",
				"event", "http_server_failed",
				"error", err.Error(),
			)
			pool.Close() // Перед os.Exit явно закрываем пул
			os.Exit(1)
		}
		return

	case <-stopCtx.Done():
		slog.Info("Начинаем остановку приложения",
			"event", "application_stopping",
		)
	}

	// Снимаем подписку: повторный Ctrl+C сможет прервать ожидание
	stop()

	// Даём текущим запросам до 10 секунд на завершение
	shutdownCtx, cancelShutdown := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer cancelShutdown()

	if err := server.Shutdown(shutdownCtx); err != nil {
		slog.Error("Не удалось штатно остановить HTTP-сервер",
			"event", "http_server_shutdown_failed",
			"error", err.Error(),
		)
		// Завершеаем процесс с ошибкой, оставшиеся соединения закроет ОС
		os.Exit(1)
	}

	slog.Info("HTTP-сервер остановлен",
		"event", "http_server_stopped",
	)
}
