package main

import (
	"encoding/json"
	"fmt"
	"net/http"
)

type User struct {
	Name  string `json:"name"`
	Email string `json:"email"`
}

func handler(w http.ResponseWriter, r *http.Request) {
	user := User{Name: "Artem", Email: "artem.protchenko@example.com"}
	data, err := json.Marshal(user)
	if err != nil {
		http.Error(w, "Error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	response := map[string]string{"status": "OK"}
	data, _ := json.Marshal(response)
	w.Header().Set("Content-Type", "application/json")
	w.Write(data)
}

func main() {
	http.HandleFunc("/user", handler)
	http.HandleFunc("/health/", healthHandler)
	fmt.Println("Сервер запущен на :8000")
	err := http.ListenAndServe(":8000", nil)
	if err != nil {
		fmt.Println("Ошибка:", err)
	}
}
