package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strings"

	"github.com/gorilla/websocket"
)

// upgrader é a configuração para o upgrade de HTTP para WebSocket.
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true // Permite conexões de qualquer origem (para testes)
	},
}

// handleWebSocketConnection lida com a conexão inicial do cliente.
func (s *Server) handleWebSocketConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Erro ao fazer upgrade para WebSocket: %v", err)
		return
	}

	// O primeiro passo é receber o nome do jogador
	_, p, err := conn.ReadMessage()
	if err != nil {
		log.Printf("Erro ao ler nome do jogador: %v", err)
		conn.Close()
		return
	}
	playerName := strings.TrimSpace(string(p))

	if playerName == "" {
		conn.WriteMessage(websocket.TextMessage, []byte("Nome de jogador inválido. Desconectando."))
		conn.Close()
		return
	}

	// Cria o estado do jogador
	player := &PlayerState{
		Name:        playerName,
		Deck:        []Card{},
		PacksOpened: 0,
		WsConn:      conn,
		ServerID:    s.ServerID,
	}

	// Adiciona o jogador ao mapa de jogadores locais
	s.PlayerMutex.Lock()
	s.Players[playerName] = player
	s.PlayerMutex.Unlock()

	log.Printf("Jogador %s conectado via WebSocket.", playerName)

	// Envia o pacote inicial
	s.openCardPack(player, true) // openCardPack agora usa sendWebSocketMessage

	// Inicia a goroutine para escutar mensagens do Redis (Pub/Sub) para este jogador
	go s.listenRedisPubSub(player)

	// Loop principal para escutar comandos do cliente
	s.listenClientCommands(player)
}

// listenClientCommands é o loop principal que processa os comandos do cliente.
func (s *Server) listenClientCommands(player *PlayerState) {
	defer func() {
		// Limpeza ao desconectar
		s.PlayerMutex.Lock()
		delete(s.Players, player.Name)
		s.PlayerMutex.Unlock()
		player.WsConn.Close()
		log.Printf("Jogador %s desconectado.", player.Name)
		// TODO: Lógica para remover jogador da fila de matchmaking se estiver nela
	}()

	for {
		_, message, err := player.WsConn.ReadMessage()
		if err != nil {
			break // Sai do loop se houver erro de leitura (desconexão)
		}

		command := strings.TrimSpace(string(message))
		log.Printf("Comando recebido de %s: %s", player.Name, command)

		// Processa o comando recebido.
		switch command {
		case "FIND_MATCH":
			// Adiciona o jogador à fila de matchmaking distribuída
			s.addToMatchmakingQueue(player)
		case "OPEN_PACK":
			s.openCardPack(player, false)
		case "VIEW_DECK":
			s.viewDeck(player)
		default:
			s.sendWebSocketMessage(player, "Comando inválido.")
		}
	}
}

// sendWebSocketMessage é a função auxiliar para enviar mensagens ao cliente.
func (s *Server) sendWebSocketMessage(player *PlayerState, message string) {
	err := player.WsConn.WriteMessage(websocket.TextMessage, []byte(message))
	if err != nil {
		log.Printf("Erro ao enviar mensagem para %s: %v", player.Name, err)
		// Se falhar, assume que a conexão caiu e fecha.
		player.WsConn.Close()
	}
}

// listenRedisPubSub escuta o canal Pub/Sub do Redis para mensagens destinadas a este jogador.
// Item 3: Comunicação Cliente-Servidor (Pub/Sub)
func (s *Server) listenRedisPubSub(player *PlayerState) {
	ctx := context.Background()
	pubsub := s.RedisClient.Subscribe(ctx, fmt.Sprintf("player:%s", player.Name))
	defer pubsub.Close()

	// Loop para receber mensagens do Redis
	for {
		msg, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			log.Printf("Erro ao receber mensagem Pub/Sub para %s: %v", player.Name, err)
			return
		}

		log.Printf("Mensagem Pub/Sub recebida para %s: %s", player.Name, msg.Payload)
		s.sendWebSocketMessage(player, msg.Payload)
	}
}
