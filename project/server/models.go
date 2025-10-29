package main

import (
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

// Card representa uma única carta do jogo, com nome e força.
type Card struct {
	Name  string `json:"name"`  // <-- ADICIONAR TAG
	Forca int    `json:"forca"` // <-- ADICIONAR TAG
}

// PlayerState (inalterado)
type PlayerState struct {
	Name        string
	Deck        []Card
	PacksOpened int
	WsConn      *websocket.Conn
	ServerID    string

	mu          sync.Mutex
	State       string
	CurrentGame *GameSession
}

// GameSession representa o estado de uma partida 1v1 em andamento.
type GameSession struct {
	Player1 *PlayerState // Pode ser local ou "fantasma"
	Player2 *PlayerState // Pode ser local ou "fantasma"

	// --- ESTES CAMPOS SÓ SERÃO PREENCHIDOS NO P1-SERVER, ANTES DE CHAMAR determineWinner ---
	Player1Card *Card
	Player2Card *Card
	// ---------------------------------------------------------------------------------

	mu          sync.Mutex
	Player1Hand [2]Card // Mão do P1 (só existe no P1-Server)
	Player2Hand [2]Card // Mão do P2 (só existe no P2-Server)

	// --- NOVOS CAMPOS ---
	Server1ID string // ID do servidor do P1
	Server2ID string // ID do servidor do P2
}

// Server (inalterado)
type Server struct {
	RedisClient *redis.Client
	Router      *chi.Mux
	Players     map[string]*PlayerState
	PlayerMutex *sync.Mutex
	ServerID    string
	ActiveGames map[string]*GameSession
	GamesMutex  sync.Mutex
}

// Request/Response DTOs para comunicação Server-Server (REST)
type TakePackRequest struct {
	PlayerName string `json:"player_name"`
}

type TakePackResponse struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Pack    []Card `json:"pack"`
}

type MatchNotificationRequest struct {
	Player1Name string `json:"player1_name"`
	Player2Name string `json:"player2_name"`
	Server1ID   string `json:"server1_id"`
	Server2ID   string `json:"server2_id"`
}

// Estruturas auxiliares para o Matchmaker Distribuído
type MatchmakingTicket struct {
	PlayerName string `json:"player_name"`
	ServerID   string `json:"server_id"`
	Timestamp  int64  `json:"timestamp"`
}
