package main

import (
	"context"
	"encoding/json" // <-- Importar JSON
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync" // Importa o sync

	"github.com/gorilla/websocket"
)

// upgrader (inalterado)
var upgrader = websocket.Upgrader{
	ReadBufferSize:  1024,
	WriteBufferSize: 1024,
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// handleWebSocketConnection (inalterado)
func (s *Server) handleWebSocketConnection(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		log.Printf("Erro ao fazer upgrade para WebSocket: %v", err)
		return
	}

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

	player := &PlayerState{
		Name:        playerName,
		Deck:        []Card{},
		PacksOpened: 0,
		WsConn:      conn,
		ServerID:    s.ServerID,
		mu:          sync.Mutex{},
		State:       "Menu",
		CurrentGame: nil,
	}

	s.PlayerMutex.Lock()
	s.Players[playerName] = player
	s.PlayerMutex.Unlock()

	log.Printf("Jogador %s conectado via WebSocket.", playerName)
	s.openCardPack(player, true)
	go s.listenRedisPubSub(player)
	s.listenClientCommands(player)
}

// listenClientCommands (inalterado)
func (s *Server) listenClientCommands(player *PlayerState) {
	defer func() {
		s.PlayerMutex.Lock()
		delete(s.Players, player.Name)
		s.PlayerMutex.Unlock()
		player.WsConn.Close()
		log.Printf("Jogador %s desconectado.", player.Name)
	}()

	for {
		_, message, err := player.WsConn.ReadMessage()
		if err != nil {
			break
		}

		command := strings.TrimSpace(string(message))
		log.Printf("Comando recebido de %s: %s", player.Name, command)

		player.mu.Lock()
		state := player.State
		game := player.CurrentGame
		player.mu.Unlock()

		if state == "InGame" && game != nil {
			s.handleGameMove(player, game, command)
		} else {
			switch {
			case command == "FIND_MATCH":
				s.addToMatchmakingQueue(player)
			case command == "OPEN_PACK":
				s.openCardPack(player, false)
			case command == "VIEW_DECK":
				s.viewDeck(player)
			case strings.HasPrefix(command, "TRADE_CARD"):
				s.handleTradeCard(player, command)
			default:
				s.sendWebSocketMessage(player, "Comando inválido.")
			}
		}
	}
}

// sendWebSocketMessage (inalterado)
func (s *Server) sendWebSocketMessage(player *PlayerState, message string) {
	err := player.WsConn.WriteMessage(websocket.TextMessage, []byte(message))
	if err != nil {
		log.Printf("Erro ao enviar mensagem para %s: %v", player.Name, err)
		player.WsConn.Close()
	}
}

// --- FUNÇÃO MODIFICADA ---
// listenRedisPubSub agora trata 'RESULT|' e 'TRADE_COMPLETE|'
func (s *Server) listenRedisPubSub(player *PlayerState) {
	ctx := context.Background()
	pubsub := s.RedisClient.Subscribe(ctx, fmt.Sprintf("player:%s", player.Name))
	defer pubsub.Close()

	for {
		msg, err := pubsub.ReceiveMessage(ctx)
		if err != nil {
			log.Printf("Erro ao receber mensagem Pub/Sub para %s: %v", player.Name, err)
			return
		}

		log.Printf("Mensagem Pub/Sub recebida para %s: %s", player.Name, msg.Payload)

		// --- LÓGICA DE ROTEAMENTO DE MENSAGEM ---

		if strings.HasPrefix(msg.Payload, "RESULT|") {
			// --- LIMPEZA DE ESTADO PÓS-JOGO ---
			log.Printf("Limpando estado de jogo para %s após resultado (via Pub/Sub).", player.Name)

			player.mu.Lock()
			player.State = "Menu"

			if player.CurrentGame != nil {
				gameID := player.CurrentGame.Player1.Name
				s.GamesMutex.Lock()
				if _, ok := s.ActiveGames[gameID]; ok {
					log.Printf("Removendo sessão %s do ActiveGames (P2-Server).", gameID)
					delete(s.ActiveGames, gameID)
				}
				s.GamesMutex.Unlock()
				player.CurrentGame = nil
			}
			player.mu.Unlock()

			// Envia a mensagem de resultado (ex: "RESULT|VITÓRIA...")
			s.sendWebSocketMessage(player, msg.Payload)

		} else if strings.HasPrefix(msg.Payload, "TRADE_COMPLETE|") {
			// --- (NOVO) PROCESSAMENTO DE TROCA CONCLUÍDA ---
			log.Printf("Recebida notificação de troca completa para %s.", player.Name)

			cardJSON := strings.TrimPrefix(msg.Payload, "TRADE_COMPLETE|")
			var receivedCard Card
			var notificationMsg string

			if err := json.Unmarshal([]byte(cardJSON), &receivedCard); err == nil {
				// Adiciona a carta recebida ao deck local do jogador
				player.Deck = append(player.Deck, receivedCard)
				notificationMsg = fmt.Sprintf("Troca concluída! Sua carta anterior foi trocada por '%s (Força: %d)'.", receivedCard.Name, receivedCard.Forca)
				log.Printf("Carta %s adicionada ao deck de %s via Pub/Sub.", receivedCard.Name, player.Name)
			} else {
				log.Printf("Erro ao desserializar carta de troca via Pub/Sub para %s: %v", player.Name, err)
				notificationMsg = "Erro ao processar uma troca recebida."
			}

			// Envia a notificação formatada para o cliente
			s.sendWebSocketMessage(player, notificationMsg)

		} else {
			// --- MENSAGEM PADRÃO ---
			// Encaminha qualquer outra mensagem (se houver)
			s.sendWebSocketMessage(player, msg.Payload)
		}
	}
}
