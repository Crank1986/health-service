package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

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

// UpdateUserRequest позволяет определить, какие поля клиент хочет изменить.
// nil означает отсутствие значения, а указатель на "" — явно переданную пустую строку.
type UpdateUserRequest struct {
	Name  *string `json:"name"`
	Email *string `json:"email"`
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

// createUserHandler принимает анкету, сохраняет пользователя и возвращает запись с ID.
func createUserHandler(w http.ResponseWriter, r *http.Request, pool *pgxpool.Pool) {
	input, err := decodeCreateUser(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return // После отказа клиенту прекращаем обработку, не выполняя INSERT.
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
		http.Error(w, "учётная запись с данным email уже зарегистрирована", http.StatusConflict)
		return
	}
	if err != nil {
		fmt.Println("Ошибка создания пользователя:", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// Сначала готовим JSON, затем отправляем статус 201 и тело ответа.
	data, err := json.Marshal(user)
	if err != nil {
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
		http.Error(w, "Пользователь не найден", http.StatusNotFound)
		return
	}
	if err != nil {
		fmt.Println("Ошибка удаления пользователя:", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}
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
		http.Error(w, "учётная запись с данным email уже зарегистрирована", http.StatusConflict)
		return
	}

	// Любая оставшаяся ошибка означает, что операцию не удалось выполнить.
	// Подробности пишем в логи, клиенту отправляем общее сообщение.
	if err != nil {
		fmt.Println("Ошибка обновления пользователя: ", err)
		http.Error(w, "Внутренняя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// Преобразуем обновлённую карточку в JSON для ответа клиенту.
	data, err := json.Marshal(user)

	// Если сформировать JSON не получилось, возвращаем 500.
	if err != nil {
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
		http.Error(w, "Пользователь не найден", http.StatusNotFound)
		return
	}

	// Подробности сбоя оставляем в логах, клиенту возвращаем общее сообщение.
	if err != nil {
		fmt.Println("Ошибка получения пользователя:", err)
		http.Error(w, "Внутреннеяя ошибка сервера", http.StatusInternalServerError)
		return
	}

	// Превращаем найденную запись в JSON и задаём тип ответа до записи его тела.
	data, err := json.Marshal(user)
	if err != nil {
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
		fmt.Println("Ошибка: не заданы обязательные настройки подключения к БД")
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
		fmt.Println("Ощибка: некорректные настройки подключения к БД")
		os.Exit(1)
	}
	// Каждый экземпляр приложения переиспользует свой пул максимум из пяти соединений.
	poolConfig.MaxConns = 5
	pool, err := pgxpool.NewWithConfig(context.Background(), poolConfig)

	if err != nil {
		fmt.Println("Ошибка: не удалось создать пул соединений")
		os.Exit(1)
	}
	// Реально проверяем соединение перед запуском HTTP-сервера, ожидая не более пяти секунд.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	err = pool.Ping(ctx)
	cancel() // Освобождаем ресурсы контекста проверки; сам пул остаётся работать.

	if err != nil {
		fmt.Println("Ошибка: не удалось подключиться к PostgreSql")
		pool.Close() // os.Exit не выполняет defer, поэтому здесь закрываем пул явно.
		os.Exit(1)
	}

	defer pool.Close() // Закрываем пул при обычном выходе из main.
	fmt.Println("Соединение с PostgreSQL проверено")
	// Функция-посредник передаёт обработчику уже созданный пул, не создавая новый на каждый запрос.
	http.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		handler(w, r, pool)
	})
	http.HandleFunc("POST /users", func(w http.ResponseWriter, r *http.Request) {
		createUserHandler(w, r, pool)
	})
	http.HandleFunc("DELETE /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		deleteUserHandler(w, r, pool)
	})
	http.HandleFunc("PATCH /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		updateUserHandler(w, r, pool)
	})
	http.HandleFunc("/health/", healthHandler)
	fmt.Println("Сервер запущен на :8000")
	// Вызов обслуживает HTTP-запросы и блокирует дальнейшее выполнение main до ошибки сервера.
	err = http.ListenAndServe(":8000", nil)
	if err != nil {
		fmt.Println("Ошибка:", err)
	}
}
