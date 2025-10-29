package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/go-redis/redis/v8"
)

// Constantes globais
const (
	webPort            = ":8080"
	restPort           = ":8081" // Porta para comunicação Server-Server (REST)
	matchmakingTimeout = 15 * time.Second
	gameTurnTimeout    = 10 * time.Second
)

// --- FUNÇÕES DE INICIALIZAÇÃO E ORQUESTRAÇÃO ---

func main() {
	// 1. Obtém o ID do servidor da variável de ambiente
	serverID := os.Getenv("SERVER_ID")
	if serverID == "" {
		serverID = fmt.Sprintf("Server-Local-%d", rand.Intn(10000))
	}
	log.Printf("Iniciando servidor com ID: %s", serverID)

	// 2. Inicializa o cliente Redis
	redisAddr := os.Getenv("REDIS_ADDR")
	if redisAddr == "" {
		redisAddr = "localhost:6379" // Default para desenvolvimento local
	}
	rdb := redis.NewClient(&redis.Options{
		Addr: redisAddr,
		DB:   0,
	})

	// Verifica a conexão com o Redis
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, err := rdb.Ping(ctx).Result()
	if err != nil {
		log.Fatalf("Erro ao conectar ao Redis: %v", err)
	}
	log.Println("Conexão com Redis estabelecida com sucesso.")

	// 3. Inicializa o servidor principal
	s := &Server{
		RedisClient: rdb,
		Players:     make(map[string]*PlayerState),
		PlayerMutex: &sync.Mutex{},
		ServerID:    serverID,
	}

	// 4. Inicializa o estoque de cartas (apenas se não existir)
	s.initializeDistributedStock()

	// 5. Inicia o servidor REST (Server-Server Communication)
	s.Router = chi.NewRouter()
	s.Router.Use(middleware.Logger)
	s.Router.Use(middleware.Recoverer)
	s.setupRestRoutes()
	go func() {
		log.Printf("Servidor REST (Server-Server) iniciado na porta %s", restPort)
		if err := http.ListenAndServe(restPort, s.Router); err != nil {
			log.Fatalf("Erro ao iniciar servidor REST: %v", err)
		}
	}()

	// 6. Inicia o servidor WebSocket (Client-Server Communication)
	http.HandleFunc("/", s.handleWebSocketConnection)
	go func() {
		log.Printf("Servidor WebSocket (Client-Server) iniciado na porta %s", webPort)
		if err := http.ListenAndServe(webPort, nil); err != nil {
			log.Fatalf("Erro ao iniciar servidor WebSocket: %v", err)
		}
	}()

	// 7. Inicia o Matchmaker Distribuído
	go s.distributedMatchmaker()

	fmt.Println("Servidor iniciado. Pressione Ctrl+C para encerrar.")

	// Bloco de encerramento gracioso
	quitChannel := make(chan os.Signal, 1)
	signal.Notify(quitChannel, syscall.SIGINT, syscall.SIGTERM)
	<-quitChannel
	fmt.Println("\nEncerrando servidor...")
	// TODO: Adicionar lógica de encerramento, como salvar estado no Redis, se necessário.
}

// setupRestRoutes configura as rotas para a comunicação Server-Server.
func (s *Server) setupRestRoutes() {
	s.Router.Route("/api/v1", func(r chi.Router) {
		// Endpoint para um servidor solicitar um pacote de cartas do estoque global
		r.Post("/stock/take", s.handleTakeCardPack)
		// Endpoint para um servidor notificar outro sobre um jogador pareado
		r.Post("/match/notify", s.handleMatchNotification)
	})
}

// handleTakeCardPack implementa o endpoint REST para que outros servidores solicitem um pacote de cartas.
// Item 4: Gerenciamento Distribuído de Estoque (Controle de Concorrência)
func (s *Server) handleTakeCardPack(w http.ResponseWriter, r *http.Request) {
	var req TakePackRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Requisição inválida", http.StatusBadRequest)
		return
	}

	// Tenta abrir o pacote de forma distribuída
	pack, err := s.openCardPackDistributed(req.PlayerName)
	if err != nil {
		w.WriteHeader(http.StatusConflict) // 409 Conflict
		json.NewEncoder(w).Encode(TakePackResponse{
			Success: false,
			Message: err.Error(),
		})
		return
	}

	// Sucesso
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(TakePackResponse{
		Success: true,
		Message: "Pacote de cartas retirado com sucesso.",
		Pack:    pack,
	})
}

// handleMatchNotification implementa o endpoint REST para que outros servidores notifiquem
// este servidor sobre um pareamento de partida.
// Item 6: Pareamento em Ambiente Distribuído
func (s *Server) handleMatchNotification(w http.ResponseWriter, r *http.Request) {
	var req MatchNotificationRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "Requisição inválida", http.StatusBadRequest)
		return
	}

	// Verifica se o jogador local é o Player1 ou Player2 da notificação
	isPlayer1Local := req.Server1ID == s.ServerID
	isPlayer2Local := req.Server2ID == s.ServerID

	if isPlayer1Local {
		s.startLocalGame(req.Player1Name, req.Player2Name)
	} else if isPlayer2Local {
		s.startLocalGame(req.Player2Name, req.Player1Name)
	} else {
		// Não deveria acontecer se a lógica do matchmaker estiver correta.
		log.Printf("Notificação de partida recebida, mas nenhum jogador é local: %v", req)
		http.Error(w, "Nenhum jogador local envolvido.", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]bool{"success": true})
}
