import http from 'k6/http';
import { check, sleep } from 'k6';

// Настройка запуска сценария.
export const options = {
    vus: 2,

    stages: [
        { duration: '1m', target: 2},
        { duration: '2m', target: 5},
        { duration: '2m', target: 10},
        { duration: '4m', target: 20},
        { duration: '1m', target: 0},
    ],

    // Обращаемся к ingress через локальный адрес Mac.
    // Аналог нашего --resolve в curl.
    hosts: {
        'arch.homework': '127.0.0.1',
    },
};

// Общий адрес сервиса для последующих запросов
const BASE_URL = 'http://arch.homework';

// K6 вызывает эту функцию при каждом проходе сценария
export default function () {
    // Пауза каждого виртуального пользователя перед новым проходом.
    // Она выполняется даже в проходах, которые затем завершатся ошибкой.
    sleep(1);
    // Разные адреса позволяют повторять тест без конфликта email.
    const email = `k6.${Date.now()}.${__VU}.${__ITER}@example.com`;

    // Превращаем объект JavaScript в текст JSON для HTTP-запроса.
    const payload = JSON.stringify({
        name: 'K6 Test',
        email: email,
    });

    const created = http.post(`${BASE_URL}/users`, payload, {
        headers: {'Content-Type': 'application/json'},
        timeout: '10s',
    });

    const createdSuccessfully = check(created, {
        'POST: пользователь создан - 201': (r) => r.status === 201,
    });

    // Если создание не удалось, у нас нет ID для следующих действий.
    if (!createdSuccessfully) {
        return;
    }

    // Разбираем ответ приложения и получаем ID созданной записи.
    const user = created.json();

    const validId = check(user, {
        'POST: получен положительный целый ID':
        (u) => Number.isInteger(u.id) && u.id > 0,
    });

    // Не отправляем запрос удаления с некорректным ID.
    if (!validId) {
        return;
    }

    // Читаем созданного пользователя отдельным HTTP-запросом.
    const fetched = http.get(`${BASE_URL}/users/${user.id}`, {
        timeout: '10s',
        tags: { name: 'GET /users/{id}' },
    });

    const fetchedSuccessfully = check(fetched, {
        'GET: пользователь найден - 200': (r) => r.status === 200,
    });

    // Разбираем карточку только при успешном ответе.
    // При ошибке чтения продолжим выполнение до удаления тестовой записи.
    if (fetchedSuccessfully){
        const fetchedUser = fetched.json();

        check(fetchedUser, {
            'GET: ID совпадает': (u) => u.id === user.id,
            'GET: имя совпадает': (u) => u.name === 'K6 Test',
            'GET: email совпадает': (u) => u.email === email,
        });
    }

    // Меняем только имя: email должен остаться прежним.
    const updated = http.patch(
        `${BASE_URL}/users/${user.id}`,
        JSON.stringify({ name: 'K6 Updated' }),
        {
            headers: { 'Content-Type': 'application/json' },
            timeout: '10s',
            tags: { name: 'PATCH /users/{id}' },
        },
    );

    const updatedSuccessfully = check(updated, {
        'PATCH: пользователь обновлён — 200': (r) => r.status === 200,
    });

    // Проверяем карточку, которую вернул обработчик обновления.
    if (updatedSuccessfully) {
        const updatedUser = updated.json();

        check(updatedUser, {
            'PATCH: ID не изменился': (u) => u.id === user.id,
            'PATCH: имя обновилось': (u) => u.name === 'K6 Updated',
            'PATCH: email сохранился': (u) => u.email === email,
        });
    }

    // Повторно читаем запись: проверяем сохранённые данные после PATCH.
    const verified = http.get(`${BASE_URL}/users/${user.id}`, {
        timeout: '10s',
        tags: { name: 'GET /users/{id}' },
    });

    const verifiedSuccessfully = check(verified, {
        'GET после PATCH: статус 200': (r) => r.status === 200,
    });

    if (verifiedSuccessfully) {
        const savedUser = verified.json();

        check(savedUser, {
            'GET после PATCH: ID совпадает': (u) => u.id === user.id,
            'GET после PATCH: новое имя сохранено':
                (u) => u.name === 'K6 Updated',
            'GET после PATCH: email сохранился': (u) => u.email === email,
        });
    }

    // Удаляем только пользователя, созданного в этом проходе.
    const deleted = http.del(`${BASE_URL}/users/${user.id}`, null, {
        timeout: '10s',
        tags: { name: 'DELETE /users/{id}' },
    });

    check(deleted, {
        'DELETED: пользователь удалён - 204': (r) => r.status === 204,
    });
}

