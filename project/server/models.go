package main

import (
	"sync"

	"github.com/go-chi/chi/v5"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/websocket"
)

// Card representa uma única carta do jogo, com nome e força.
type Card struct {
	Name  string `json:"name"`
	Forca int    `json:"forca"`
}

// PlayerState representa o estado de um jogador, agora armazenado no servidor.
// A conexão de rede (websocket) é gerenciada pelo servidor local.
type PlayerState struct {
	Name        string
	Deck        []Card
	PacksOpened int
	WsConn      *websocket.Conn // Conexão WebSocket para o cliente local
	ServerID    string

	// --- NOVOS CAMPOS PARA GERENCIAMENTO DE ESTADO ---
	mu          sync.Mutex      // Protege o 'State' e 'CurrentGame'
	State       string          // "Menu", "InGame", "Searching"
	CurrentGame *GameSession    // Referência para o jogo atual (se 'State' == "InGame")
}

// GameSession representa o estado de uma partida 1v1 em andamento.
type GameSession struct {
	Player1     *PlayerState
	Player2     *PlayerState
	Player1Card *Card
	Player2Card *Card
	mu          sync.Mutex // Mutex para proteger o acesso concorrente aos dados da sessão.
	
	// --- NOVOS CAMPOS PARA ARMAZENAR MÃO ---
	Player1Hand [2]Card
	Player2Hand [2]Card
}

// Server é a estrutura principal que gerencia o estado e as conexões do servidor.
type Server struct {
	RedisClient *redis.Client
	Router      *chi.Mux
	Players     map[string]*PlayerState // Mapa de jogadores conectados localmente (key: PlayerName)
	PlayerMutex *sync.Mutex
	ServerID    string // Identificador único do servidor

	// --- NOVOS CAMPOS PARA GERENCIAR PARTIDAS ---
	ActiveGames map[string]*GameSession // Mapa de partidas ativas (key: PlayerName do P1)
	GamesMutex  sync.Mutex              // Protege 'ActiveGames'
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